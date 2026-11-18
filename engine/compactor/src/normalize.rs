//! Rewrites batches read from an events file into the project's **current**
//! promoted schema (`spec/parquet.md` §3, design §8.4): values move from
//! `props` into newly promoted `p_*` columns, values of columns the target
//! schema does not carry (or carries with another type) move back into
//! `props`. A value whose type does not fit its column stays in `props`.

use crate::json;
use arrow::array::{
    new_null_array, Array, ArrayRef, BooleanArray, BooleanBuilder, Float64Array, Float64Builder, Int64Array,
    Int64Builder, StringArray, StringBuilder,
};
use arrow::datatypes::{DataType, SchemaRef};
use arrow::record_batch::RecordBatch;
use la_core::error::{Error, Result};
use la_core::events::Cell;
use la_core::schema::{events_schema, PromotedColumn, PropType, FIXED_PREFIX};
use std::borrow::Cow;
use std::sync::Arc;

pub struct Normalizer {
    target: SchemaRef,
    promoted: Vec<PromotedColumn>,
    /// `"key"` needles used to skip JSON parsing for rows that cannot carry the key.
    needles: Vec<String>,
}

enum CellBuilder {
    Str(StringBuilder),
    Int(Int64Builder),
    Float(Float64Builder),
    Bool(BooleanBuilder),
}

impl CellBuilder {
    fn new(ty: PropType, n: usize) -> Self {
        match ty {
            PropType::String => CellBuilder::Str(StringBuilder::new()),
            PropType::Int => CellBuilder::Int(Int64Builder::with_capacity(n)),
            PropType::Float => CellBuilder::Float(Float64Builder::with_capacity(n)),
            PropType::Bool => CellBuilder::Bool(BooleanBuilder::with_capacity(n)),
        }
    }

    fn append(&mut self, cell: Option<Cell>) {
        match (self, cell) {
            (CellBuilder::Str(b), Some(Cell::Str(v))) => b.append_value(v),
            (CellBuilder::Int(b), Some(Cell::Int(v))) => b.append_value(v),
            (CellBuilder::Float(b), Some(Cell::Float(v))) => b.append_value(v),
            (CellBuilder::Float(b), Some(Cell::Int(v))) => b.append_value(v as f64),
            (CellBuilder::Bool(b), Some(Cell::Bool(v))) => b.append_value(v),
            (CellBuilder::Str(b), _) => b.append_null(),
            (CellBuilder::Int(b), _) => b.append_null(),
            (CellBuilder::Float(b), _) => b.append_null(),
            (CellBuilder::Bool(b), _) => b.append_null(),
        }
    }

    fn finish(self) -> ArrayRef {
        match self {
            CellBuilder::Str(mut b) => Arc::new(b.finish()),
            CellBuilder::Int(mut b) => Arc::new(b.finish()),
            CellBuilder::Float(mut b) => Arc::new(b.finish()),
            CellBuilder::Bool(mut b) => Arc::new(b.finish()),
        }
    }
}

/// Interprets raw JSON value text as a cell of column type `ty`
/// (`int` widens into `float`; everything else must match exactly).
pub fn typed_cell(raw: &str, ty: PropType) -> Option<Cell> {
    let first = *raw.as_bytes().first()?;
    match ty {
        PropType::String => (first == b'"').then(|| serde_json::from_str::<String>(raw).ok()).flatten().map(Cell::Str),
        PropType::Int => raw.parse::<i64>().ok().map(Cell::Int),
        PropType::Float => {
            if first == b'-' || first.is_ascii_digit() {
                match raw.parse::<i64>() {
                    Ok(i) => Some(Cell::Int(i)),
                    Err(_) => raw.parse::<f64>().ok().filter(|f| f.is_finite()).map(Cell::Float),
                }
            } else {
                None
            }
        }
        PropType::Bool => match raw {
            "true" => Some(Cell::Bool(true)),
            "false" => Some(Cell::Bool(false)),
            _ => None,
        },
    }
}

/// JSON text of a promoted cell that has to go back into `props`.
fn cell_json(col: &ArrayRef, row: usize) -> Option<String> {
    if col.is_null(row) {
        return None;
    }
    match col.data_type() {
        DataType::Utf8 => col.as_any().downcast_ref::<StringArray>().map(|a| json::json_string(a.value(row))),
        DataType::Int64 => col.as_any().downcast_ref::<Int64Array>().map(|a| a.value(row).to_string()),
        DataType::Float64 => col
            .as_any()
            .downcast_ref::<Float64Array>()
            .map(|a| a.value(row))
            .filter(|f| f.is_finite())
            .map(json::json_f64),
        DataType::Boolean => col.as_any().downcast_ref::<BooleanArray>().map(|a| a.value(row).to_string()),
        _ => None,
    }
}

impl Normalizer {
    pub fn new(promoted: &[PromotedColumn]) -> Self {
        Self {
            target: events_schema(promoted),
            promoted: promoted.to_vec(),
            needles: promoted.iter().map(|c| format!("\"{}\"", c.key)).collect(),
        }
    }

    pub fn schema(&self) -> &SchemaRef {
        &self.target
    }

    pub fn promoted(&self) -> &[PromotedColumn] {
        &self.promoted
    }

    pub fn normalize(&self, batch: &RecordBatch) -> Result<RecordBatch> {
        let n = batch.num_rows();
        let mut cols: Vec<ArrayRef> = Vec::with_capacity(self.target.fields().len());
        for f in self.target.fields().iter().take(FIXED_PREFIX) {
            match batch.column_by_name(f.name()) {
                Some(c) if c.data_type() == f.data_type() => cols.push(c.clone()),
                Some(c) => {
                    return Err(Error::invalid(format!("column {} has type {}", f.name(), c.data_type())));
                }
                // A nullable fixed column the file predates reads as null.
                None if f.is_nullable() => cols.push(new_null_array(f.data_type(), n)),
                None => return Err(Error::invalid(format!("missing column {}", f.name()))),
            }
        }
        let props_in = batch
            .column_by_name("props")
            .and_then(|c| c.as_any().downcast_ref::<StringArray>())
            .ok_or_else(|| Error::invalid("missing or mistyped props column"))?;

        // Target columns the file already carries with the right type are copied.
        let mut pcols: Vec<Option<ArrayRef>> = vec![None; self.promoted.len()];
        let mut extract: Vec<usize> = Vec::new();
        for (i, c) in self.promoted.iter().enumerate() {
            match batch.column_by_name(&c.column_name()) {
                Some(a) if a.data_type() == &c.ty.arrow() => pcols[i] = Some(a.clone()),
                _ => extract.push(i),
            }
        }
        // File columns without a home in the target schema go back into props.
        let mut spill: Vec<(String, ArrayRef)> = Vec::new();
        let in_schema = batch.schema();
        for (idx, f) in in_schema.fields().iter().enumerate() {
            let Some(key) = f.name().strip_prefix("p_") else {
                continue;
            };
            let homed = self.promoted.iter().any(|c| c.key == key && c.ty.arrow() == *f.data_type());
            if !homed && batch.column(idx).null_count() < n {
                spill.push((key.to_owned(), batch.column(idx).clone()));
            }
        }

        let props_out: ArrayRef = if extract.is_empty() && spill.is_empty() {
            Arc::new(props_in.clone())
        } else {
            let mut builders: Vec<CellBuilder> =
                extract.iter().map(|&i| CellBuilder::new(self.promoted[i].ty, n)).collect();
            let mut out = StringBuilder::with_capacity(n, props_in.value_data().len());
            for row in 0..n {
                let text = props_in.value(row);
                let wants_spill = spill.iter().any(|(_, a)| a.is_valid(row));
                let may_extract = extract.iter().any(|&i| text.contains(self.needles[i].as_str()));
                let parsed = if wants_spill || may_extract { json::parse_object(text) } else { None };
                let Some(mut map) = parsed else {
                    for b in &mut builders {
                        b.append(None);
                    }
                    out.append_value(text);
                    continue;
                };
                let mut changed = false;
                for (key, col) in &spill {
                    if !map.contains_key(key) {
                        if let Some(js) = cell_json(col, row) {
                            map.insert(key.clone(), Cow::Owned(js));
                            changed = true;
                        }
                    }
                }
                for (b, &i) in builders.iter_mut().zip(&extract) {
                    let c = &self.promoted[i];
                    let cell = map.get(&c.key).and_then(|raw| typed_cell(raw, c.ty));
                    if cell.is_some() {
                        map.remove(&c.key);
                        changed = true;
                    }
                    b.append(cell);
                }
                if changed {
                    out.append_value(json::write_object(map.iter()));
                } else {
                    out.append_value(text);
                }
            }
            for (b, &i) in builders.into_iter().zip(&extract) {
                pcols[i] = Some(b.finish());
            }
            Arc::new(out.finish())
        };
        for (i, c) in pcols.into_iter().enumerate() {
            cols.push(c.ok_or_else(|| Error::invalid(format!("promoted column {i} was not produced")))?);
        }
        cols.push(props_out);
        Ok(RecordBatch::try_new(self.target.clone(), cols)?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::events::{batch_to_rows, rows_to_batch, EventRow};

    fn col(key: &str, ty: PropType, since: u64) -> PromotedColumn {
        PromotedColumn { key: key.into(), ty, since }
    }

    fn row(id: u8, props: &str, promoted: Vec<Option<Cell>>) -> EventRow {
        EventRow {
            uuid: [id; 16],
            ts: id as i64,
            recv_ts: id as i64,
            event: "e".into(),
            person_key: format!("u:{id}"),
            anon_id: "a".into(),
            props: props.into(),
            promoted,
            ..Default::default()
        }
    }

    #[test]
    fn typed_cells() {
        assert_eq!(typed_cell("\"x\"", PropType::String), Some(Cell::Str("x".into())));
        assert_eq!(typed_cell("1", PropType::String), None);
        assert_eq!(typed_cell("12", PropType::Int), Some(Cell::Int(12)));
        assert_eq!(typed_cell("12.0", PropType::Int), None);
        assert_eq!(typed_cell("\"12\"", PropType::Int), None);
        assert_eq!(typed_cell("12", PropType::Float), Some(Cell::Int(12)));
        assert_eq!(typed_cell("1.5e3", PropType::Float), Some(Cell::Float(1500.0)));
        assert_eq!(typed_cell("null", PropType::Float), None);
        assert_eq!(typed_cell("true", PropType::Bool), Some(Cell::Bool(true)));
        assert_eq!(typed_cell("\"true\"", PropType::Bool), None);
    }

    #[test]
    fn same_schema_passes_through() {
        let cols = vec![col("plan", PropType::String, 1)];
        let rows = vec![row(1, r#"{"k":1}"#, vec![Some(Cell::Str("pro".into()))]), row(2, "{}", vec![None])];
        let batch = rows_to_batch(&rows, &cols).unwrap();
        let out = Normalizer::new(&cols).normalize(&batch).unwrap();
        assert_eq!(batch_to_rows(&out, &cols).unwrap(), rows);
    }

    #[test]
    fn newly_promoted_keys_move_out_of_props() {
        let old = vec![col("plan", PropType::String, 1)];
        let new = vec![
            col("plan", PropType::String, 1),
            col("amount", PropType::Float, 2),
            col("n", PropType::Int, 2),
            col("ok", PropType::Bool, 2),
        ];
        let rows = vec![
            row(1, r#"{"amount":3,"n":7,"ok":true,"z":"keep"}"#, vec![Some(Cell::Str("pro".into()))]),
            // type conflicts stay in props
            row(2, r#"{"amount":"a lot","n":1.5,"ok":"yes"}"#, vec![None]),
            row(3, r#"{"amount":2.5,"note":"\"amount\" inside a string"}"#, vec![None]),
            row(4, "{}", vec![None]),
        ];
        let batch = rows_to_batch(&rows, &old).unwrap();
        let out = batch_to_rows(&Normalizer::new(&new).normalize(&batch).unwrap(), &new).unwrap();
        assert_eq!(
            out[0].promoted,
            [Some(Cell::Str("pro".into())), Some(Cell::Float(3.0)), Some(Cell::Int(7)), Some(Cell::Bool(true))]
        );
        assert_eq!(out[0].props, r#"{"z":"keep"}"#);
        assert_eq!(out[1].promoted, [None, None, None, None]);
        assert_eq!(out[1].props, r#"{"amount":"a lot","n":1.5,"ok":"yes"}"#);
        assert_eq!(out[2].promoted[1], Some(Cell::Float(2.5)));
        assert_eq!(out[2].props, r#"{"note":"\"amount\" inside a string"}"#);
        assert_eq!(out[3].props, "{}");
    }

    #[test]
    fn columns_unknown_to_the_target_go_back_into_props() {
        let old = vec![col("plan", PropType::String, 1), col("n", PropType::Int, 1), col("f", PropType::Float, 1)];
        // Target lost `plan` and wants `n` as float.
        let new = vec![col("n", PropType::Float, 1)];
        let rows = vec![
            row(1, r#"{"a":1}"#, vec![Some(Cell::Str("pro".into())), Some(Cell::Int(4)), Some(Cell::Float(0.5))]),
            row(2, r#"{"plan":"already"}"#, vec![Some(Cell::Str("ignored".into())), None, None]),
        ];
        let batch = rows_to_batch(&rows, &old).unwrap();
        let out = batch_to_rows(&Normalizer::new(&new).normalize(&batch).unwrap(), &new).unwrap();
        assert_eq!(out[0].promoted, [Some(Cell::Float(4.0))]);
        assert_eq!(out[0].props, r#"{"a":1,"f":0.5,"plan":"pro"}"#);
        assert_eq!(out[1].promoted, [None]);
        assert_eq!(out[1].props, r#"{"plan":"already"}"#);
    }
}
