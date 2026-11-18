//! Reads the query-visible ("logical") content of a day straight from the
//! objects, independent of how the rows are split over files, which promoted
//! columns each file carries and which identity links were already applied at
//! write time. Used by the property tests and handy for ad-hoc verification.

use arrow::datatypes::DataType;
use la_core::error::{Error, Result};
use la_core::events::{batch_to_rows, Cell, EventRow};
use la_core::identity::IdentityMap;
use la_core::manifest::{self, DayManifest};
use la_core::parquet_io::read_parquet;
use la_core::schema::{PromotedColumn, PropType};
use la_core::tombstone::TombstoneSet;
use la_core::Store;
use std::collections::BTreeMap;

/// A row with promoted cells folded back into its property map and numbers
/// normalized to `f64` (an `int` widened into a `float` column compares equal).
#[derive(Debug, Clone, PartialEq)]
pub struct LogicalRow {
    pub person_key: String,
    pub ts: i64,
    pub uuid: [u8; 16],
    pub recv_ts: i64,
    pub event: String,
    pub anon_id: String,
    pub user_id: Option<String>,
    pub session_id: Option<[u8; 16]>,
    pub dims: Vec<Option<String>>,
    pub flags: i32,
    pub props: BTreeMap<String, serde_json::Value>,
}

fn normalize_numbers(v: serde_json::Value) -> serde_json::Value {
    use serde_json::Value;
    match v {
        Value::Number(n) => n.as_f64().and_then(serde_json::Number::from_f64).map(Value::Number).unwrap_or(Value::Null),
        Value::Array(a) => Value::Array(a.into_iter().map(normalize_numbers).collect()),
        Value::Object(o) => Value::Object(o.into_iter().map(|(k, v)| (k, normalize_numbers(v))).collect()),
        other => other,
    }
}

impl LogicalRow {
    pub fn from_row(row: EventRow, cols: &[PromotedColumn]) -> Result<Self> {
        let parsed: BTreeMap<String, serde_json::Value> = serde_json::from_str(&row.props)
            .map_err(|e| Error::invalid(format!("props of {:02x?} is not a JSON object: {e}", row.uuid)))?;
        let mut props: BTreeMap<String, serde_json::Value> =
            parsed.into_iter().map(|(k, v)| (k, normalize_numbers(v))).collect();
        for (c, cell) in cols.iter().zip(row.promoted) {
            let v = match cell {
                None => continue,
                Some(Cell::Str(s)) => serde_json::Value::String(s),
                Some(Cell::Int(i)) => serde_json::json!(i as f64),
                Some(Cell::Float(f)) => serde_json::json!(f),
                Some(Cell::Bool(b)) => serde_json::Value::Bool(b),
            };
            // A promoted cell wins over a same-named leftover in props (coalesce(p_x, props->x)).
            props.insert(c.key.clone(), v);
        }
        Ok(Self {
            person_key: row.person_key,
            ts: row.ts,
            uuid: row.uuid,
            recv_ts: row.recv_ts,
            event: row.event,
            anon_id: row.anon_id,
            user_id: row.user_id,
            session_id: row.session_id,
            dims: row.dims.to_vec(),
            flags: row.flags,
            props,
        })
    }

    fn order(&self, other: &Self) -> std::cmp::Ordering {
        (self.person_key.as_bytes(), self.ts, self.uuid, self.recv_ts).cmp(&(
            other.person_key.as_bytes(),
            other.ts,
            other.uuid,
            other.recv_ts,
        ))
    }
}

/// Promoted columns an events file actually carries, from its Arrow schema.
pub fn file_columns(schema: &arrow::datatypes::Schema) -> Vec<PromotedColumn> {
    schema
        .fields()
        .iter()
        .filter_map(|f| {
            let key = f.name().strip_prefix("p_")?;
            let ty = match f.data_type() {
                DataType::Utf8 => PropType::String,
                DataType::Int64 => PropType::Int,
                DataType::Float64 => PropType::Float,
                DataType::Boolean => PropType::Bool,
                _ => return None,
            };
            Some(PromotedColumn { key: key.to_owned(), ty, since: 0 })
        })
        .collect()
}

/// Rows of one events object, in file order, as logical rows.
pub async fn file_rows(store: &Store, key: &str) -> Result<Vec<LogicalRow>> {
    let data = store.get(key).await?.ok_or_else(|| Error::invalid(format!("{key} is listed but missing")))?;
    let mut out = Vec::new();
    for batch in read_parquet(data)? {
        let cols = file_columns(&batch.schema());
        for row in batch_to_rows(&batch, &cols)? {
            out.push(LogicalRow::from_row(row, &cols)?);
        }
    }
    Ok(out)
}

/// What a query sees for `(project, day)` (`spec/query.md` "Tekilleştirme"):
/// all files of the current manifest, `a:` keys re-resolved through
/// `identity`, tombstoned rows removed, sorted by `(person_key, ts, uuid)`,
/// and of adjacent rows equal in that key only one kept (the one with the
/// smallest `recv_ts`).
pub async fn day_rows(
    store: &Store,
    project: u32,
    day: &str,
    identity: &IdentityMap,
    tombstones: Option<&TombstoneSet>,
) -> Result<Vec<LogicalRow>> {
    let m: DayManifest = manifest::load_day(store, project, day).await?;
    let mut rows = Vec::new();
    for f in &m.files {
        let file = file_rows(store, &f.key).await?;
        if file.len() as u64 != f.rows {
            return Err(Error::invalid(format!("{}: manifest says {} rows, file has {}", f.key, f.rows, file.len())));
        }
        if !file.windows(2).all(|w| (w[0].person_key.as_bytes(), w[0].ts, w[0].uuid) <= (w[1].person_key.as_bytes(), w[1].ts, w[1].uuid))
        {
            return Err(Error::invalid(format!("{} is not sorted by (person_key, ts, uuid)", f.key)));
        }
        rows.extend(file);
    }
    for r in &mut rows {
        if let Some(user) = r.person_key.strip_prefix("a:").and_then(|a| identity.resolve(a)) {
            r.person_key = format!("u:{user}");
        }
    }
    if let Some(t) = tombstones {
        rows.retain(|r| !t.matches(&r.person_key, &r.anon_id, r.user_id.as_deref()));
    }
    rows.sort_by(|a, b| a.order(b));
    rows.dedup_by(|b, a| (a.person_key.as_str(), a.ts, a.uuid) == (b.person_key.as_str(), b.ts, b.uuid));
    Ok(rows)
}

/// [`day_rows`] for every day of a project, keyed by day.
pub async fn project_rows(
    store: &Store,
    project: u32,
    identity: &IdentityMap,
    tombstones: Option<&TombstoneSet>,
) -> Result<BTreeMap<String, Vec<LogicalRow>>> {
    let mut out = BTreeMap::new();
    for day in manifest::list_days(store, project).await? {
        let rows = day_rows(store, project, &day, identity, tombstones).await?;
        if !rows.is_empty() {
            out.insert(day, rows);
        }
    }
    Ok(out)
}

/// Keeps, per person, only the earliest row of every `uuid` — the stronger
/// rule compaction applies on top of the query-time rule.
pub fn dedupe_uuid_per_person(rows: &mut Vec<LogicalRow>) {
    let mut seen = std::collections::HashSet::new();
    let mut person = String::new();
    rows.retain(|r| {
        if r.person_key != person {
            person.clone_from(&r.person_key);
            seen.clear();
        }
        seen.insert(r.uuid)
    });
}
