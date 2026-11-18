//! Person property files (`spec/manifest.md` §4): `(person_key, key) → value`,
//! last writer wins by `(ts, seq)`.

use crate::error::{Error, Result};
use crate::identity::{IdentityMap, SideFile, COMPACT_ABOVE_FILES};
use crate::manifest::{self, Versioned};
use crate::parquet_io::{read_parquet, table_writer_props, write_parquet};
use crate::{keys, metrics, Store};
use arrow::array::{Array, Int64Array, StringArray, TimestampMillisecondArray};
use arrow::datatypes::{DataType, Field, Schema, SchemaRef, TimeUnit};
use arrow::record_batch::RecordBatch;
use bytes::Bytes;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::Arc;

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct PersonsManifest {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub project: u32,
    #[serde(default)]
    pub applied: BTreeMap<String, u64>,
    #[serde(default)]
    pub files: Vec<SideFile>,
}

impl Versioned for PersonsManifest {
    fn version(&self) -> u64 {
        self.version
    }
    fn set_version(&mut self, v: u64) {
        self.version = v;
    }
}

/// One property assignment. `value` is JSON text (`"abc"`, `12`, `true`,
/// `null` = delete). `ts = 0` marks a `$set_once` style weakest write.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PropWrite {
    pub person_key: String,
    pub key: String,
    pub value: String,
    pub ts: i64,
    /// Tie-breaker for equal `ts`: persons manifest version of the write.
    pub seq: u64,
}

fn props_schema() -> SchemaRef {
    Arc::new(Schema::new(vec![
        Field::new("person_key", DataType::Utf8, false),
        Field::new("key", DataType::Utf8, false),
        Field::new("value", DataType::Utf8, false),
        Field::new("ts", DataType::Timestamp(TimeUnit::Millisecond, Some("UTC".into())), false),
        Field::new("seq", DataType::Int64, false),
    ]))
}

fn encode(rows: &[PropWrite]) -> Result<Bytes> {
    let schema = props_schema();
    let batch = RecordBatch::try_new(
        schema.clone(),
        vec![
            Arc::new(StringArray::from_iter_values(rows.iter().map(|r| r.person_key.as_str()))),
            Arc::new(StringArray::from_iter_values(rows.iter().map(|r| r.key.as_str()))),
            Arc::new(StringArray::from_iter_values(rows.iter().map(|r| r.value.as_str()))),
            Arc::new(TimestampMillisecondArray::from_iter_values(rows.iter().map(|r| r.ts)).with_timezone("UTC")),
            Arc::new(Int64Array::from_iter_values(rows.iter().map(|r| r.seq as i64))),
        ],
    )?;
    write_parquet(&schema, &[batch], table_writer_props()?)
}

fn decode(data: Bytes, file: &SideFile) -> Result<Vec<PropWrite>> {
    let mut out = Vec::new();
    for b in read_parquet(data)? {
        let get = |name: &str| b.column_by_name(name).ok_or_else(|| Error::invalid(format!("{}: no {name}", file.key)));
        let bad = || Error::invalid(format!("{}: unexpected column type", file.key));
        let pk = get("person_key")?.as_any().downcast_ref::<StringArray>().ok_or_else(bad)?;
        let key = get("key")?.as_any().downcast_ref::<StringArray>().ok_or_else(bad)?;
        let value = get("value")?.as_any().downcast_ref::<StringArray>().ok_or_else(bad)?;
        let ts = get("ts")?.as_any().downcast_ref::<TimestampMillisecondArray>().ok_or_else(bad)?;
        let seq = get("seq")?.as_any().downcast_ref::<Int64Array>().ok_or_else(bad)?;
        for i in 0..b.num_rows() {
            out.push(PropWrite {
                person_key: pk.value(i).to_owned(),
                key: key.value(i).to_owned(),
                value: value.value(i).to_owned(),
                ts: ts.value(i),
                // Same rule as identity deltas: the manifest entry is authoritative.
                seq: if file.is_delta() { file.max_version } else { seq.value(i).max(0) as u64 },
            });
        }
    }
    Ok(out)
}

/// Keeps the winning write per `(person_key, key)`.
fn last_writer_wins(rows: impl IntoIterator<Item = PropWrite>) -> Vec<PropWrite> {
    let mut best: HashMap<(String, String), PropWrite> = HashMap::new();
    for r in rows {
        match best.get_mut(&(r.person_key.clone(), r.key.clone())) {
            Some(cur) => {
                if (r.ts, r.seq) >= (cur.ts, cur.seq) {
                    *cur = r;
                }
            }
            None => {
                best.insert((r.person_key.clone(), r.key.clone()), r);
            }
        }
    }
    let mut out: Vec<PropWrite> = best.into_values().collect();
    out.sort_by(|a, b| (a.person_key.as_str(), a.key.as_str()).cmp(&(b.person_key.as_str(), b.key.as_str())));
    out
}

/// Commits the property writes of shipper plan `plan` (in arrival order — for
/// equal `ts` the later element wins). Idempotent per `(shardkey, plan)`.
/// Returns the persons manifest version after the call.
pub async fn append_delta(
    store: &Store,
    project: u32,
    shardkey: &str,
    plan: u64,
    writes: Vec<PropWrite>,
) -> Result<u64> {
    let dir = keys::persons_manifest_dir(project);
    if writes.is_empty() {
        return manifest::latest_version(store, &dir).await;
    }
    for attempt in 0u32..64 {
        let mut m: PersonsManifest = manifest::load_latest(store, &dir).await?;
        if m.applied.get(shardkey).is_some_and(|n| *n >= plan) {
            return Ok(m.version);
        }
        let v = m.version + 1;
        let rows = last_writer_wins(writes.iter().cloned().map(|mut w| {
            w.seq = v;
            w
        }));
        let key = keys::persons_delta(project, shardkey, plan);
        store.put(&key, encode(&rows)?).await?;
        m.version = v;
        m.project = project;
        m.applied.insert(shardkey.to_owned(), plan);
        m.files.retain(|f| f.key != key);
        m.files.push(SideFile { key, rows: rows.len() as u64, min_version: v, max_version: v });
        if store.put_json_if_absent(&keys::manifest_file(&dir, v), &m).await? {
            return Ok(v);
        }
        metrics::MANIFEST_CONFLICTS.inc();
        tokio::time::sleep(std::time::Duration::from_millis(rand::random_range(0..=(10u64 << attempt.min(6))))).await;
    }
    Err(Error::Conflict(dir))
}

/// Resolved, merged view of every person's current properties.
#[derive(Debug, Clone, Default)]
pub struct PersonTable {
    version: u64,
    /// person_key → key → JSON value text (deleted keys already removed).
    persons: HashMap<String, BTreeMap<String, String>>,
}

impl PersonTable {
    /// Loads all files, re-resolves `a:` keys through `identity`, applies
    /// last-writer-wins and drops deleted (`null`) properties.
    pub async fn load(store: &Store, project: u32, identity: &IdentityMap) -> Result<Self> {
        'outer: for _ in 0..4 {
            let m: PersonsManifest = manifest::load_latest(store, &keys::persons_manifest_dir(project)).await?;
            let mut all = Vec::new();
            for f in &m.files {
                let Some(data) = store.get(&f.key).await? else {
                    continue 'outer;
                };
                all.extend(decode(data, f)?);
            }
            for w in &mut all {
                if let Some(anon) = w.person_key.strip_prefix("a:") {
                    if let Some(user) = identity.resolve(anon) {
                        w.person_key = format!("u:{user}");
                    }
                }
            }
            let mut persons: HashMap<String, BTreeMap<String, String>> = HashMap::new();
            for w in last_writer_wins(all) {
                if w.value != "null" {
                    persons.entry(w.person_key).or_default().insert(w.key, w.value);
                }
            }
            persons.retain(|_, p| !p.is_empty());
            return Ok(Self { version: m.version, persons });
        }
        Err(Error::invalid(format!("person files of project {project} keep disappearing")))
    }

    pub fn version(&self) -> u64 {
        self.version
    }

    pub fn len(&self) -> usize {
        self.persons.len()
    }

    pub fn is_empty(&self) -> bool {
        self.persons.is_empty()
    }

    /// JSON text of one property.
    pub fn get(&self, person_key: &str, key: &str) -> Option<&str> {
        self.persons.get(person_key)?.get(key).map(String::as_str)
    }

    pub fn person(&self, person_key: &str) -> Option<&BTreeMap<String, String>> {
        self.persons.get(person_key)
    }

    pub fn iter(&self) -> impl Iterator<Item = (&String, &BTreeMap<String, String>)> {
        self.persons.iter()
    }

    /// The person's properties as one JSON object (keys sorted).
    pub fn person_json(&self, person_key: &str) -> String {
        let mut s = String::from("{");
        if let Some(p) = self.persons.get(person_key) {
            for (i, (k, v)) in p.iter().enumerate() {
                if i > 0 {
                    s.push(',');
                }
                s.push_str(&serde_json::Value::String(k.clone()).to_string());
                s.push(':');
                s.push_str(v);
            }
        }
        s.push('}');
        s
    }
}

/// Merges all files into one once there are more than
/// [`COMPACT_ABOVE_FILES`]; losing writes are dropped, and so are all rows of
/// `drop_persons` (tombstoned people). Returns `true` if a manifest was written.
pub async fn compact(store: &Store, project: u32, drop_persons: &HashSet<String>, force: bool) -> Result<bool> {
    let dir = keys::persons_manifest_dir(project);
    let base: PersonsManifest = manifest::load_latest(store, &dir).await?;
    if base.files.is_empty() || (!force && base.files.len() <= COMPACT_ABOVE_FILES) {
        return Ok(false);
    }
    let mut all = Vec::new();
    for f in &base.files {
        let Some(data) = store.get(&f.key).await? else {
            return Ok(false);
        };
        all.extend(decode(data, f)?);
    }
    all.retain(|w| !drop_persons.contains(&w.person_key));
    let rows = last_writer_wins(all);
    let inputs: HashSet<String> = base.files.iter().map(|f| f.key.clone()).collect();
    let (lo, hi) = rows.iter().fold((u64::MAX, 0u64), |(lo, hi), r| (lo.min(r.seq), hi.max(r.seq)));
    let data = if rows.is_empty() { None } else { Some(encode(&rows)?) };
    for _ in 0..8 {
        let mut m: PersonsManifest = manifest::load_latest(store, &dir).await?;
        if !inputs.iter().all(|k| m.files.iter().any(|f| &f.key == k)) {
            return Ok(false);
        }
        let v = m.version + 1;
        m.files.retain(|f| !inputs.contains(&f.key));
        if let Some(data) = &data {
            let key = keys::persons_compacted(project, v);
            store.put(&key, data.clone()).await?;
            let (min_version, max_version) = if lo == hi { (lo.saturating_sub(1), hi) } else { (lo, hi) };
            m.files.insert(0, SideFile { key, rows: rows.len() as u64, min_version, max_version });
        }
        m.version = v;
        if store.put_json_if_absent(&keys::manifest_file(&dir, v), &m).await? {
            return Ok(true);
        }
        metrics::MANIFEST_CONFLICTS.inc();
    }
    Ok(false)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::{self, Candidate};

    fn w(pk: &str, key: &str, value: &str, ts: i64) -> PropWrite {
        PropWrite { person_key: pk.into(), key: key.into(), value: value.into(), ts, seq: 0 }
    }

    #[tokio::test]
    async fn last_writer_wins_across_deltas() {
        let s = Store::memory();
        let id = IdentityMap::default();
        assert_eq!(
            append_delta(&s, 1, "1-0", 1, vec![w("u:1", "plan", "\"free\"", 100), w("u:1", "age", "30", 100)])
                .await
                .unwrap(),
            1
        );
        assert_eq!(append_delta(&s, 1, "1-0", 2, vec![w("u:1", "plan", "\"pro\"", 200)]).await.unwrap(), 2);
        // Older timestamp arriving later loses; equal timestamp arriving later wins.
        append_delta(&s, 1, "1-0", 3, vec![w("u:1", "plan", "\"old\"", 150), w("u:1", "age", "31", 100)])
            .await
            .unwrap();
        let t = PersonTable::load(&s, 1, &id).await.unwrap();
        assert_eq!(t.get("u:1", "plan"), Some("\"pro\""));
        assert_eq!(t.get("u:1", "age"), Some("31"));
        assert_eq!(t.person_json("u:1"), r#"{"age":31,"plan":"pro"}"#);
        assert_eq!(t.person_json("u:none"), "{}");
        assert_eq!(t.version(), 3);
    }

    #[tokio::test]
    async fn within_one_delta_later_write_wins_ties() {
        let s = Store::memory();
        append_delta(&s, 1, "1-0", 1, vec![w("u:1", "k", "1", 5), w("u:1", "k", "2", 5), w("u:1", "k", "0", 4)])
            .await
            .unwrap();
        let t = PersonTable::load(&s, 1, &IdentityMap::default()).await.unwrap();
        assert_eq!(t.get("u:1", "k"), Some("2"));
    }

    #[tokio::test]
    async fn set_once_is_weakest_and_null_deletes() {
        let s = Store::memory();
        append_delta(&s, 1, "1-0", 1, vec![w("u:1", "first_seen", "\"a\"", 0)]).await.unwrap();
        append_delta(&s, 1, "1-0", 2, vec![w("u:1", "first_seen", "\"b\"", 0), w("u:1", "x", "1", 10)]).await.unwrap();
        append_delta(&s, 1, "1-0", 3, vec![w("u:1", "x", "null", 20)]).await.unwrap();
        let t = PersonTable::load(&s, 1, &IdentityMap::default()).await.unwrap();
        // Two $set_once writes: both ts=0, the later seq wins — a real $set (ts>0) always beats them.
        assert_eq!(t.get("u:1", "first_seen"), Some("\"b\""));
        assert_eq!(t.get("u:1", "x"), None);
    }

    #[tokio::test]
    async fn anonymous_props_follow_the_identity_merge() {
        let s = Store::memory();
        append_delta(&s, 1, "1-0", 1, vec![w("a:dev1", "theme", "\"dark\"", 10), w("a:dev1", "plan", "\"free\"", 10)])
            .await
            .unwrap();
        append_delta(&s, 1, "1-0", 2, vec![w("u:42", "plan", "\"pro\"", 20)]).await.unwrap();
        let mut id = IdentityMap::default();
        let t = PersonTable::load(&s, 1, &id).await.unwrap();
        assert_eq!(t.get("a:dev1", "theme"), Some("\"dark\""));
        identity::append_delta(
            &s,
            1,
            "1-0",
            3,
            vec![Candidate { anon_id: "dev1".into(), user_id: "42".into(), ts: 30, uuid: [1; 16] }],
            &mut id,
        )
        .await
        .unwrap();
        let t = PersonTable::load(&s, 1, &id).await.unwrap();
        assert_eq!(t.get("a:dev1", "theme"), None);
        assert_eq!(t.get("u:42", "theme"), Some("\"dark\""));
        assert_eq!(t.get("u:42", "plan"), Some("\"pro\""));
        assert_eq!(t.len(), 1);
    }

    #[tokio::test]
    async fn replay_is_idempotent_and_compaction_keeps_the_view() {
        let s = Store::memory();
        for i in 0..(COMPACT_ABOVE_FILES as u64 + 3) {
            let writes = vec![w("u:1", "n", &i.to_string(), i as i64), w(&format!("u:p{i}"), "k", "true", 1)];
            append_delta(&s, 1, "1-0", i + 1, writes.clone()).await.unwrap();
            let v = manifest::latest_version(&s, &keys::persons_manifest_dir(1)).await.unwrap();
            assert_eq!(append_delta(&s, 1, "1-0", i + 1, writes).await.unwrap(), v, "replay must not write");
        }
        let id = IdentityMap::default();
        let before = PersonTable::load(&s, 1, &id).await.unwrap();
        assert!(compact(&s, 1, &HashSet::new(), false).await.unwrap());
        assert!(!compact(&s, 1, &HashSet::new(), false).await.unwrap());
        let after = PersonTable::load(&s, 1, &id).await.unwrap();
        assert_eq!(after.len(), before.len());
        assert_eq!(after.get("u:1", "n"), before.get("u:1", "n"));
        let m: PersonsManifest = manifest::load_latest(&s, &keys::persons_manifest_dir(1)).await.unwrap();
        assert_eq!(m.files.len(), 1);
        // A newer write still wins over the compacted file; forced compaction drops tombstoned people.
        append_delta(&s, 1, "1-0", 1000, vec![w("u:1", "n", "\"last\"", 10_000)]).await.unwrap();
        assert!(compact(&s, 1, &["u:1".to_owned()].into(), true).await.unwrap());
        let t = PersonTable::load(&s, 1, &id).await.unwrap();
        assert!(t.person("u:1").is_none());
        assert_eq!(t.get("u:p0", "k"), Some("true"));
    }
}
