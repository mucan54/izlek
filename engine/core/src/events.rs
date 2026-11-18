//! Row model for the `events` table and its Arrow conversion (`spec/parquet.md`).

use crate::error::{Error, Result};
use crate::schema::{events_schema, PromotedColumn, PropType, DIM_COLUMNS, FIXED_PREFIX};
use arrow::array::{
    Array, ArrayRef, BooleanArray, BooleanBuilder, FixedSizeBinaryArray, FixedSizeBinaryBuilder, Float64Array,
    Float64Builder, Int32Array, Int32Builder, Int64Array, Int64Builder, StringArray, StringBuilder,
    TimestampMillisecondArray, TimestampMillisecondBuilder,
};
use arrow::record_batch::RecordBatch;
use std::cmp::Ordering;
use std::sync::Arc;

pub const FLAG_CLOCK_CORRECTED: i32 = la_codec::clock::FLAG_CLOCK_CORRECTED;
pub const FLAG_LATE: i32 = la_codec::clock::FLAG_LATE;
pub const FLAG_COMPAT: i32 = la_codec::clock::FLAG_COMPAT;
pub const FLAG_WEB: i32 = la_codec::clock::FLAG_WEB;

/// Value of a promoted column cell.
#[derive(Debug, Clone, PartialEq)]
pub enum Cell {
    Str(String),
    Int(i64),
    Float(f64),
    Bool(bool),
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct EventRow {
    pub uuid: [u8; 16],
    pub ts: i64,
    pub recv_ts: i64,
    pub event: String,
    pub person_key: String,
    pub anon_id: String,
    pub user_id: Option<String>,
    pub session_id: Option<[u8; 16]>,
    /// Indexed by `schema::DIM_*`.
    pub dims: [Option<String>; DIM_COLUMNS.len()],
    pub flags: i32,
    /// Aligned with the promoted columns the batch is built for.
    pub promoted: Vec<Option<Cell>>,
    /// Remaining properties: JSON object text, keys byte-sorted, no whitespace.
    pub props: String,
}

impl EventRow {
    /// File order: `(person_key, ts, uuid)`, byte-wise.
    pub fn sort_key_cmp(&self, other: &Self) -> Ordering {
        self.person_key
            .as_bytes()
            .cmp(other.person_key.as_bytes())
            .then(self.ts.cmp(&other.ts))
            .then(self.uuid.cmp(&other.uuid))
    }
}

/// `"u:"+user_id` or `"a:"+anon_id` (`docs/design.md` §9.1).
pub fn person_key(user_id: Option<&str>, anon_id: &str) -> String {
    match user_id {
        Some(u) if !u.is_empty() => format!("u:{u}"),
        _ => format!("a:{anon_id}"),
    }
}

/// Sorts into file order and drops rows repeating the `uuid` of an earlier
/// row of the same person (duplicates inside one shipper plan).
pub fn sort_and_dedupe(rows: &mut Vec<EventRow>) {
    rows.sort_by(|a, b| a.sort_key_cmp(b));
    let mut seen: std::collections::HashSet<[u8; 16]> = std::collections::HashSet::new();
    let mut current: Option<String> = None;
    rows.retain(|r| {
        if current.as_deref() != Some(r.person_key.as_str()) {
            current = Some(r.person_key.clone());
            seen.clear();
        }
        seen.insert(r.uuid)
    });
}

/// Builds a `RecordBatch` in the schema for `promoted`. Rows whose `promoted`
/// vector is shorter than the column list get nulls for the missing cells;
/// a cell of the wrong type is an error (callers must type-check first).
pub fn rows_to_batch(rows: &[EventRow], promoted: &[PromotedColumn]) -> Result<RecordBatch> {
    let n = rows.len();
    let mut uuid = FixedSizeBinaryBuilder::with_capacity(n, 16);
    let mut ts = TimestampMillisecondBuilder::with_capacity(n);
    let mut recv = TimestampMillisecondBuilder::with_capacity(n);
    let mut event = StringBuilder::new();
    let mut pkey = StringBuilder::new();
    let mut anon = StringBuilder::new();
    let mut user = StringBuilder::new();
    let mut session = FixedSizeBinaryBuilder::with_capacity(n, 16);
    let mut dims: Vec<StringBuilder> = (0..DIM_COLUMNS.len()).map(|_| StringBuilder::new()).collect();
    let mut flags = Int32Builder::with_capacity(n);
    let mut props = StringBuilder::new();
    for r in rows {
        uuid.append_value(r.uuid)?;
        ts.append_value(r.ts);
        recv.append_value(r.recv_ts);
        event.append_value(&r.event);
        pkey.append_value(&r.person_key);
        anon.append_value(&r.anon_id);
        user.append_option(r.user_id.as_deref());
        match &r.session_id {
            Some(s) => session.append_value(s)?,
            None => session.append_null(),
        }
        for (b, d) in dims.iter_mut().zip(&r.dims) {
            b.append_option(d.as_deref());
        }
        flags.append_value(r.flags);
        props.append_value(if r.props.is_empty() { "{}" } else { &r.props });
    }
    let mut cols: Vec<ArrayRef> = vec![
        Arc::new(uuid.finish()),
        Arc::new(ts.finish().with_timezone("UTC")),
        Arc::new(recv.finish().with_timezone("UTC")),
        Arc::new(event.finish()),
        Arc::new(pkey.finish()),
        Arc::new(anon.finish()),
        Arc::new(user.finish()),
        Arc::new(session.finish()),
    ];
    for mut b in dims {
        cols.push(Arc::new(b.finish()));
    }
    cols.push(Arc::new(flags.finish()));
    for (i, c) in promoted.iter().enumerate() {
        let bad = |r: &EventRow| Error::invalid(format!("cell type mismatch in p_{} (uuid {:02x?})", c.key, r.uuid));
        let col: ArrayRef = match c.ty {
            PropType::String => {
                let mut b = StringBuilder::new();
                for r in rows {
                    match r.promoted.get(i).and_then(Option::as_ref) {
                        None => b.append_null(),
                        Some(Cell::Str(s)) => b.append_value(s),
                        Some(_) => return Err(bad(r)),
                    }
                }
                Arc::new(b.finish())
            }
            PropType::Int => {
                let mut b = Int64Builder::with_capacity(n);
                for r in rows {
                    match r.promoted.get(i).and_then(Option::as_ref) {
                        None => b.append_null(),
                        Some(Cell::Int(v)) => b.append_value(*v),
                        Some(_) => return Err(bad(r)),
                    }
                }
                Arc::new(b.finish())
            }
            PropType::Float => {
                let mut b = Float64Builder::with_capacity(n);
                for r in rows {
                    match r.promoted.get(i).and_then(Option::as_ref) {
                        None => b.append_null(),
                        Some(Cell::Float(v)) => b.append_value(*v),
                        Some(Cell::Int(v)) => b.append_value(*v as f64),
                        Some(_) => return Err(bad(r)),
                    }
                }
                Arc::new(b.finish())
            }
            PropType::Bool => {
                let mut b = BooleanBuilder::with_capacity(n);
                for r in rows {
                    match r.promoted.get(i).and_then(Option::as_ref) {
                        None => b.append_null(),
                        Some(Cell::Bool(v)) => b.append_value(*v),
                        Some(_) => return Err(bad(r)),
                    }
                }
                Arc::new(b.finish())
            }
        };
        cols.push(col);
    }
    cols.push(Arc::new(props.finish()));
    Ok(RecordBatch::try_new(events_schema(promoted), cols)?)
}

fn col<'a, T: 'static>(batch: &'a RecordBatch, name: &str) -> Result<&'a T> {
    batch
        .column_by_name(name)
        .ok_or_else(|| Error::invalid(format!("missing column {name}")))?
        .as_any()
        .downcast_ref::<T>()
        .ok_or_else(|| Error::invalid(format!("unexpected type for column {name}")))
}

fn id16(a: &FixedSizeBinaryArray, i: usize) -> [u8; 16] {
    let mut out = [0u8; 16];
    out.copy_from_slice(a.value(i));
    out
}

/// Inverse of [`rows_to_batch`] for batches read back from an events file
/// (plain `Utf8` strings). `promoted` selects which `p_*` columns to extract;
/// columns absent from the batch yield `None` cells.
pub fn batch_to_rows(batch: &RecordBatch, promoted: &[PromotedColumn]) -> Result<Vec<EventRow>> {
    let n = batch.num_rows();
    let uuid = col::<FixedSizeBinaryArray>(batch, "uuid")?;
    let ts = col::<TimestampMillisecondArray>(batch, "ts")?;
    let recv = col::<TimestampMillisecondArray>(batch, "recv_ts")?;
    let event = col::<StringArray>(batch, "event")?;
    let pkey = col::<StringArray>(batch, "person_key")?;
    let anon = col::<StringArray>(batch, "anon_id")?;
    let user = col::<StringArray>(batch, "user_id")?;
    let session = col::<FixedSizeBinaryArray>(batch, "session_id")?;
    let flags = col::<Int32Array>(batch, "flags")?;
    let props = col::<StringArray>(batch, "props")?;
    let mut dims: Vec<Option<&StringArray>> = Vec::with_capacity(DIM_COLUMNS.len());
    for name in DIM_COLUMNS {
        dims.push(if batch.column_by_name(name).is_some() { Some(col::<StringArray>(batch, name)?) } else { None });
    }
    let pcols: Vec<Option<&ArrayRef>> = promoted.iter().map(|c| batch.column_by_name(&c.column_name())).collect();
    let mut out = Vec::with_capacity(n);
    for i in 0..n {
        let mut row = EventRow {
            uuid: id16(uuid, i),
            ts: ts.value(i),
            recv_ts: recv.value(i),
            event: event.value(i).to_owned(),
            person_key: pkey.value(i).to_owned(),
            anon_id: anon.value(i).to_owned(),
            user_id: (!user.is_null(i)).then(|| user.value(i).to_owned()),
            session_id: (!session.is_null(i)).then(|| id16(session, i)),
            flags: flags.value(i),
            props: props.value(i).to_owned(),
            ..Default::default()
        };
        for (d, a) in row.dims.iter_mut().zip(&dims) {
            if let Some(a) = a {
                if !a.is_null(i) {
                    *d = Some(a.value(i).to_owned());
                }
            }
        }
        row.promoted = Vec::with_capacity(promoted.len());
        for (c, a) in promoted.iter().zip(&pcols) {
            let cell = match a {
                Some(a) if !a.is_null(i) => match c.ty {
                    PropType::String => {
                        a.as_any().downcast_ref::<StringArray>().map(|x| Cell::Str(x.value(i).to_owned()))
                    }
                    PropType::Int => a.as_any().downcast_ref::<Int64Array>().map(|x| Cell::Int(x.value(i))),
                    PropType::Float => a.as_any().downcast_ref::<Float64Array>().map(|x| Cell::Float(x.value(i))),
                    PropType::Bool => a.as_any().downcast_ref::<BooleanArray>().map(|x| Cell::Bool(x.value(i))),
                },
                _ => None,
            };
            row.promoted.push(cell);
        }
        out.push(row);
    }
    Ok(out)
}

/// Index of the `props` column for a schema with `promoted` columns.
pub fn props_index(promoted: usize) -> usize {
    FIXED_PREFIX + promoted
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::schema::{DIM_COUNTRY, DIM_PLATFORM};

    pub(crate) fn row(person: &str, ts: i64, id: u8) -> EventRow {
        let mut r = EventRow {
            uuid: [id; 16],
            ts,
            recv_ts: ts + 5,
            event: "e".into(),
            person_key: person.into(),
            anon_id: "anon".into(),
            props: "{}".into(),
            ..Default::default()
        };
        r.dims[DIM_PLATFORM] = Some("ios".into());
        r
    }

    #[test]
    fn person_keys() {
        assert_eq!(person_key(Some("42"), "a1"), "u:42");
        assert_eq!(person_key(Some(""), "a1"), "a:a1");
        assert_eq!(person_key(None, "a1"), "a:a1");
    }

    #[test]
    fn sort_and_dedupe_orders_and_drops_repeats() {
        let mut rows = vec![
            row("u:b", 5, 1),
            row("a:z", 9, 2),
            row("u:b", 5, 0),
            row("u:b", 1, 3),
            row("u:b", 7, 1),
            row("u:c", 5, 1),
        ];
        sort_and_dedupe(&mut rows);
        let got: Vec<_> = rows.iter().map(|r| (r.person_key.as_str(), r.ts, r.uuid[0])).collect();
        // uuid 1 appears twice for u:b (ts 5 and 7): the earlier one stays; u:c keeps its own.
        assert_eq!(got, [("a:z", 9, 2), ("u:b", 1, 3), ("u:b", 5, 0), ("u:b", 5, 1), ("u:c", 5, 1)]);
    }

    #[test]
    fn batch_roundtrip_with_promoted_columns() {
        let cols = vec![
            PromotedColumn { key: "plan".into(), ty: PropType::String, since: 1 },
            PromotedColumn { key: "amount".into(), ty: PropType::Float, since: 1 },
            PromotedColumn { key: "n".into(), ty: PropType::Int, since: 2 },
            PromotedColumn { key: "ok".into(), ty: PropType::Bool, since: 2 },
        ];
        let mut a = row("u:1", 10, 1);
        a.user_id = Some("1".into());
        a.session_id = Some([9; 16]);
        a.dims[DIM_COUNTRY] = Some("TR".into());
        a.flags = FLAG_WEB | FLAG_LATE;
        a.promoted =
            vec![Some(Cell::Str("pro".into())), Some(Cell::Int(3)), Some(Cell::Int(-7)), Some(Cell::Bool(true))];
        a.props = r#"{"k":"v"}"#.into();
        let b = row("u:2", 11, 2); // no promoted cells at all
        let batch = rows_to_batch(&[a.clone(), b.clone()], &cols).unwrap();
        assert_eq!(batch.num_columns(), FIXED_PREFIX + 4 + 1);
        assert_eq!(batch.schema().field(props_index(4)).name(), "props");
        let back = batch_to_rows(&batch, &cols).unwrap();
        let mut a2 = a.clone();
        a2.promoted[1] = Some(Cell::Float(3.0)); // int widened into the float column
        let mut b2 = b.clone();
        b2.promoted = vec![None; 4];
        assert_eq!(back, [a2, b2]);
        // Reading with a narrower / wider column list
        assert_eq!(batch_to_rows(&batch, &[]).unwrap()[0].promoted.len(), 0);
        let extra = PromotedColumn { key: "later".into(), ty: PropType::String, since: 3 };
        assert_eq!(batch_to_rows(&batch, &[extra]).unwrap()[0].promoted, [None]);
    }

    #[test]
    fn wrong_cell_type_is_an_error() {
        let cols = vec![PromotedColumn { key: "n".into(), ty: PropType::Int, since: 1 }];
        let mut a = row("u:1", 10, 1);
        a.promoted = vec![Some(Cell::Str("x".into()))];
        assert!(rows_to_batch(&[a], &cols).is_err());
    }
}
