//! `anon_id → user_id` mapping files (`spec/manifest.md` §3).

use crate::error::{Error, Result};
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

/// Side-table files are merged into one once a manifest lists more than this.
pub const COMPACT_ABOVE_FILES: usize = 16;

/// Entry of an identity or persons manifest.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct SideFile {
    pub key: String,
    #[serde(default)]
    pub rows: u64,
    /// For a delta file both equal the manifest version that added it and are
    /// **authoritative** over the per-row values inside the file.
    #[serde(default)]
    pub min_version: u64,
    #[serde(default)]
    pub max_version: u64,
}

impl SideFile {
    pub fn is_delta(&self) -> bool {
        self.min_version == self.max_version
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct IdentityManifest {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub project: u32,
    /// `shardkey → highest shipper plan whose links are included`.
    #[serde(default)]
    pub applied: BTreeMap<String, u64>,
    #[serde(default)]
    pub files: Vec<SideFile>,
}

impl Versioned for IdentityManifest {
    fn version(&self) -> u64 {
        self.version
    }
    fn set_version(&mut self, v: u64) {
        self.version = v;
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Link {
    pub anon_id: String,
    pub user_id: String,
    /// Identity manifest version that introduced the link.
    pub version: u64,
    pub ts: i64,
}

/// A link proposed by an `identify` record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    pub anon_id: String,
    pub user_id: String,
    pub ts: i64,
    pub uuid: [u8; 16],
}

fn map_schema() -> SchemaRef {
    Arc::new(Schema::new(vec![
        Field::new("anon_id", DataType::Utf8, false),
        Field::new("user_id", DataType::Utf8, false),
        Field::new("version", DataType::Int64, false),
        Field::new("ts", DataType::Timestamp(TimeUnit::Millisecond, Some("UTC".into())), false),
    ]))
}

fn encode_links(links: &[Link]) -> Result<Bytes> {
    let schema = map_schema();
    let batch = RecordBatch::try_new(
        schema.clone(),
        vec![
            Arc::new(StringArray::from_iter_values(links.iter().map(|l| l.anon_id.as_str()))),
            Arc::new(StringArray::from_iter_values(links.iter().map(|l| l.user_id.as_str()))),
            Arc::new(Int64Array::from_iter_values(links.iter().map(|l| l.version as i64))),
            Arc::new(TimestampMillisecondArray::from_iter_values(links.iter().map(|l| l.ts)).with_timezone("UTC")),
        ],
    )?;
    write_parquet(&schema, &[batch], table_writer_props()?)
}

fn decode_links(data: Bytes, file: &SideFile) -> Result<Vec<Link>> {
    let mut out = Vec::new();
    for b in read_parquet(data)? {
        let get = |name: &str| b.column_by_name(name).ok_or_else(|| Error::invalid(format!("{}: no {name}", file.key)));
        let bad = || Error::invalid(format!("{}: unexpected column type", file.key));
        let anon = get("anon_id")?.as_any().downcast_ref::<StringArray>().ok_or_else(bad)?;
        let user = get("user_id")?.as_any().downcast_ref::<StringArray>().ok_or_else(bad)?;
        let ver = get("version")?.as_any().downcast_ref::<Int64Array>().ok_or_else(bad)?;
        let ts = get("ts")?.as_any().downcast_ref::<TimestampMillisecondArray>().ok_or_else(bad)?;
        for i in 0..b.num_rows() {
            let version = if file.is_delta() { file.max_version } else { ver.value(i).max(0) as u64 };
            out.push(Link {
                anon_id: anon.value(i).to_owned(),
                user_id: user.value(i).to_owned(),
                version,
                ts: ts.value(i),
            });
        }
    }
    Ok(out)
}

/// In-memory view of a project's identity map, refreshable incrementally.
#[derive(Debug, Clone, Default)]
pub struct IdentityMap {
    version: u64,
    links: HashMap<String, (String, u64, i64)>,
    loaded: HashSet<String>,
    applied: BTreeMap<String, u64>,
    files: usize,
}

impl IdentityMap {
    pub async fn load(store: &Store, project: u32) -> Result<Self> {
        let mut m = Self::default();
        m.refresh(store, project).await?;
        Ok(m)
    }

    /// Brings the map up to the newest manifest, reading only files not seen
    /// before. Returns the manifest it is now consistent with.
    pub async fn refresh(&mut self, store: &Store, project: u32) -> Result<IdentityManifest> {
        // Files listed by a manifest can only vanish if a newer manifest
        // replaced them (compaction + GC), so a retry observes a newer state.
        'outer: for _ in 0..4 {
            let manifest: IdentityManifest =
                manifest::load_latest(store, &keys::identity_manifest_dir(project)).await?;
            for f in &manifest.files {
                if self.loaded.contains(&f.key) {
                    continue;
                }
                let Some(data) = store.get(&f.key).await? else {
                    continue 'outer;
                };
                for l in decode_links(data, f)? {
                    self.insert(l);
                }
                self.loaded.insert(f.key.clone());
            }
            self.version = manifest.version;
            self.applied = manifest.applied.clone();
            self.files = manifest.files.len();
            return Ok(manifest);
        }
        Err(Error::invalid(format!("identity files of project {project} keep disappearing")))
    }

    /// First link wins; a re-read of the same link keeps the lowest version.
    fn insert(&mut self, l: Link) {
        match self.links.get_mut(&l.anon_id) {
            Some(cur) => {
                if cur.0 == l.user_id {
                    cur.1 = cur.1.min(l.version);
                } else if l.version < cur.1 {
                    *cur = (l.user_id, l.version, l.ts);
                }
            }
            None => {
                self.links.insert(l.anon_id, (l.user_id, l.version, l.ts));
            }
        }
    }

    /// Identity manifest version this map reflects.
    pub fn version(&self) -> u64 {
        self.version
    }

    pub fn len(&self) -> usize {
        self.links.len()
    }

    pub fn is_empty(&self) -> bool {
        self.links.is_empty()
    }

    pub fn file_count(&self) -> usize {
        self.files
    }

    pub fn resolve(&self, anon_id: &str) -> Option<&str> {
        self.links.get(anon_id).map(|l| l.0.as_str())
    }

    /// The user an anonymous id maps to, considering only links **newer** than
    /// `after_version` (what a data file written at that identity version has
    /// not seen yet).
    pub fn resolve_after(&self, anon_id: &str, after_version: u64) -> Option<&str> {
        self.links.get(anon_id).filter(|l| l.1 > after_version).map(|l| l.0.as_str())
    }

    pub fn has_links_after(&self, after_version: u64) -> bool {
        after_version < self.version && self.links.values().any(|l| l.1 > after_version)
    }

    /// Resolved person key for a row (`docs/design.md` §9.2 stage 1).
    pub fn person_key(&self, user_id: Option<&str>, anon_id: &str) -> String {
        match user_id.filter(|u| !u.is_empty()).or_else(|| self.resolve(anon_id)) {
            Some(u) => format!("u:{u}"),
            None => format!("a:{anon_id}"),
        }
    }

    /// All anonymous ids linked to any of `users`.
    pub fn anons_of(&self, users: &HashSet<String>) -> HashSet<String> {
        self.links.iter().filter(|(_, l)| users.contains(&l.0)).map(|(a, _)| a.clone()).collect()
    }

    pub fn iter(&self) -> impl Iterator<Item = Link> + '_ {
        self.links.iter().map(|(a, (u, v, ts))| Link { anon_id: a.clone(), user_id: u.clone(), version: *v, ts: *ts })
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct AppendOutcome {
    /// Identity manifest version after the call (what data files written next
    /// may record as their `identity_version`).
    pub version: u64,
    pub added: usize,
    pub conflicts: usize,
    /// The plan's delta had been committed by an earlier execution.
    pub already_applied: bool,
}

/// Commits the links proposed by shipper plan `plan` and brings `map` up to
/// date. Idempotent per `(shardkey, plan)`.
pub async fn append_delta(
    store: &Store,
    project: u32,
    shardkey: &str,
    plan: u64,
    mut candidates: Vec<Candidate>,
    map: &mut IdentityMap,
) -> Result<AppendOutcome> {
    candidates.sort_by(|a, b| a.ts.cmp(&b.ts).then(a.uuid.cmp(&b.uuid)));
    let dir = keys::identity_manifest_dir(project);
    for attempt in 0u32..64 {
        let mut manifest = map.refresh(store, project).await?;
        if manifest.applied.get(shardkey).is_some_and(|n| *n >= plan) {
            return Ok(AppendOutcome { version: manifest.version, already_applied: true, ..Default::default() });
        }
        let v = manifest.version + 1;
        let mut fresh: HashMap<&str, &str> = HashMap::new();
        let mut links: Vec<Link> = Vec::new();
        let mut conflicts = 0usize;
        for c in &candidates {
            if c.anon_id.is_empty() || c.user_id.is_empty() {
                continue;
            }
            match map.resolve(&c.anon_id).or_else(|| fresh.get(c.anon_id.as_str()).copied()) {
                Some(u) if u == c.user_id => {}
                Some(_) => conflicts += 1,
                None => {
                    fresh.insert(&c.anon_id, &c.user_id);
                    links.push(Link { anon_id: c.anon_id.clone(), user_id: c.user_id.clone(), version: v, ts: c.ts });
                }
            }
        }
        if links.is_empty() {
            return Ok(AppendOutcome { version: manifest.version, conflicts, ..Default::default() });
        }
        links.sort_by(|a, b| a.anon_id.cmp(&b.anon_id));
        let key = keys::identity_delta(project, shardkey, plan);
        // Not referenced by any manifest yet, so overwriting on retry is safe;
        // readers take the version from the manifest entry, not from the rows.
        store.put(&key, encode_links(&links)?).await?;
        manifest.version = v;
        manifest.project = project;
        manifest.applied.insert(shardkey.to_owned(), plan);
        manifest.files.retain(|f| f.key != key);
        manifest.files.push(SideFile { key: key.clone(), rows: links.len() as u64, min_version: v, max_version: v });
        if store.put_json_if_absent(&keys::manifest_file(&dir, v), &manifest).await? {
            let added = links.len();
            for l in links {
                map.insert(l);
            }
            map.loaded.insert(key);
            map.version = v;
            map.applied = manifest.applied;
            map.files = manifest.files.len();
            return Ok(AppendOutcome { version: v, added, conflicts, already_applied: false });
        }
        metrics::MANIFEST_CONFLICTS.inc();
        tokio::time::sleep(std::time::Duration::from_millis(rand::random_range(0..=(10u64 << attempt.min(6))))).await;
    }
    Err(Error::Conflict(dir))
}

/// Merges all listed files into one when there are more than
/// [`COMPACT_ABOVE_FILES`]. Row versions are preserved. Returns `true` if a
/// new manifest was written.
pub async fn compact(store: &Store, project: u32) -> Result<bool> {
    let dir = keys::identity_manifest_dir(project);
    let base: IdentityManifest = manifest::load_latest(store, &dir).await?;
    if base.files.len() <= COMPACT_ABOVE_FILES {
        return Ok(false);
    }
    let mut merged = IdentityMap::default();
    for f in &base.files {
        let Some(data) = store.get(&f.key).await? else {
            return Ok(false);
        };
        for l in decode_links(data, f)? {
            merged.insert(l);
        }
    }
    let mut links: Vec<Link> = merged.iter().collect();
    links.sort_by(|a, b| a.anon_id.cmp(&b.anon_id));
    let inputs: HashSet<String> = base.files.iter().map(|f| f.key.clone()).collect();
    let (lo, hi) = links.iter().fold((u64::MAX, 0u64), |(lo, hi), l| (lo.min(l.version), hi.max(l.version)));
    let data = encode_links(&links)?;
    // The output key embeds the version it is committed at, so every attempt
    // writes a fresh object and never touches a referenced file.
    for _ in 0..8 {
        let mut m: IdentityManifest = manifest::load_latest(store, &dir).await?;
        if !inputs.iter().all(|k| m.files.iter().any(|f| &f.key == k)) {
            return Ok(false); // someone else compacted
        }
        let v = m.version + 1;
        let key = keys::identity_compacted(project, v);
        store.put(&key, data.clone()).await?;
        m.files.retain(|f| !inputs.contains(&f.key));
        // min == max would mark the file as a delta; keep them distinct.
        let (min_version, max_version) = if lo == hi { (lo.saturating_sub(1), hi) } else { (lo, hi) };
        m.files.insert(0, SideFile { key, rows: links.len() as u64, min_version, max_version });
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

    fn cand(anon: &str, user: &str, ts: i64, id: u8) -> Candidate {
        Candidate { anon_id: anon.into(), user_id: user.into(), ts, uuid: [id; 16] }
    }

    #[tokio::test]
    async fn first_link_wins_and_is_permanent() {
        let s = Store::memory();
        let mut map = IdentityMap::load(&s, 1).await.unwrap();
        assert_eq!((map.version(), map.len()), (0, 0));
        // Within one plan the earliest (ts, uuid) wins regardless of input order.
        let out = append_delta(
            &s,
            1,
            "1-0",
            1,
            vec![cand("a1", "late", 20, 1), cand("a1", "early", 10, 2), cand("a2", "u2", 5, 3)],
            &mut map,
        )
        .await
        .unwrap();
        assert_eq!(out, AppendOutcome { version: 1, added: 2, conflicts: 1, already_applied: false });
        assert_eq!(map.resolve("a1"), Some("early"));
        // A later plan cannot rebind a1 even with an older timestamp.
        let out = append_delta(&s, 1, "1-0", 2, vec![cand("a1", "other", 1, 4), cand("a3", "u3", 1, 5)], &mut map)
            .await
            .unwrap();
        assert_eq!((out.version, out.added, out.conflicts), (2, 1, 1));
        assert_eq!(map.resolve("a1"), Some("early"));
        assert_eq!(map.person_key(None, "a3"), "u:u3");
        assert_eq!(map.person_key(None, "zz"), "a:zz");
        assert_eq!(map.person_key(Some("explicit"), "a3"), "u:explicit");

        let fresh = IdentityMap::load(&s, 1).await.unwrap();
        assert_eq!((fresh.version(), fresh.len()), (2, 3));
        assert_eq!(fresh.resolve_after("a1", 0), Some("early"));
        assert_eq!(fresh.resolve_after("a1", 1), None);
        assert_eq!(fresh.resolve_after("a3", 1), Some("u3"));
        assert!(fresh.has_links_after(1) && !fresh.has_links_after(2));
        assert_eq!(fresh.anons_of(&["early".to_owned()].into()), ["a1".to_owned()].into());
    }

    #[tokio::test]
    async fn replaying_a_plan_is_a_noop() {
        let s = Store::memory();
        let mut map = IdentityMap::default();
        let c = vec![cand("a1", "u1", 10, 1)];
        append_delta(&s, 1, "1-0", 5, c.clone(), &mut map).await.unwrap();
        let again = append_delta(&s, 1, "1-0", 5, c.clone(), &mut IdentityMap::default()).await.unwrap();
        assert!(again.already_applied);
        assert_eq!(again.version, 1);
        // An older plan number is also treated as applied.
        assert!(append_delta(&s, 1, "1-0", 4, c, &mut map).await.unwrap().already_applied);
        assert_eq!(manifest::latest_version(&s, &keys::identity_manifest_dir(1)).await.unwrap(), 1);
    }

    #[tokio::test]
    async fn plans_without_new_links_write_nothing() {
        let s = Store::memory();
        let mut map = IdentityMap::default();
        append_delta(&s, 1, "1-0", 1, vec![cand("a1", "u1", 10, 1)], &mut map).await.unwrap();
        let out =
            append_delta(&s, 1, "1-0", 2, vec![cand("a1", "u1", 99, 2), cand("", "u", 1, 3)], &mut map).await.unwrap();
        assert_eq!((out.version, out.added, out.conflicts), (1, 0, 0));
        let out = append_delta(&s, 1, "1-0", 3, vec![], &mut map).await.unwrap();
        assert_eq!(out.version, 1);
    }

    #[tokio::test]
    async fn manifest_entry_version_overrides_row_version() {
        // A racing duplicate executor may overwrite a delta with rows stamped
        // with a stale version; readers must trust the manifest entry.
        let s = Store::memory();
        let mut map = IdentityMap::default();
        append_delta(&s, 1, "1-0", 1, vec![cand("a1", "u1", 10, 1)], &mut map).await.unwrap();
        let key = keys::identity_delta(1, "1-0", 1);
        let stale = encode_links(&[Link { anon_id: "a1".into(), user_id: "u1".into(), version: 0, ts: 10 }]).unwrap();
        s.put(&key, stale).await.unwrap();
        let fresh = IdentityMap::load(&s, 1).await.unwrap();
        assert_eq!(fresh.resolve_after("a1", 0), Some("u1"));
    }

    #[tokio::test]
    async fn compaction_preserves_links_and_versions() {
        let s = Store::memory();
        let mut map = IdentityMap::default();
        for i in 0..(COMPACT_ABOVE_FILES as u64 + 2) {
            append_delta(
                &s,
                1,
                "1-0",
                i + 1,
                vec![cand(&format!("a{i}"), &format!("u{i}"), i as i64, i as u8)],
                &mut map,
            )
            .await
            .unwrap();
        }
        assert!(compact(&s, 1).await.unwrap());
        assert!(!compact(&s, 1).await.unwrap());
        let m: IdentityManifest = manifest::load_latest(&s, &keys::identity_manifest_dir(1)).await.unwrap();
        assert_eq!(m.files.len(), 1);
        assert!(!m.files[0].is_delta());
        assert_eq!(m.applied.get("1-0"), Some(&(COMPACT_ABOVE_FILES as u64 + 2)));
        let fresh = IdentityMap::load(&s, 1).await.unwrap();
        assert_eq!(fresh.len(), COMPACT_ABOVE_FILES + 2);
        assert_eq!(fresh.resolve_after("a3", 3), Some("u3"));
        assert_eq!(fresh.resolve_after("a3", 4), None);
        // A map that had loaded the deltas keeps working after compaction…
        let before = map.len();
        map.refresh(&s, 1).await.unwrap();
        assert_eq!(map.len(), before);
        assert_eq!(map.resolve_after("a3", 3), Some("u3"));
        // …and new deltas still append on top of the compacted file.
        let out = append_delta(&s, 1, "1-0", 100, vec![cand("new", "u", 1, 200)], &mut map).await.unwrap();
        assert_eq!(out.added, 1);
        assert_eq!(IdentityMap::load(&s, 1).await.unwrap().len(), before + 1);
    }

    #[tokio::test]
    async fn single_version_compaction_is_not_mistaken_for_a_delta() {
        let s = Store::memory();
        let dir = keys::identity_manifest_dir(1);
        // 17 files all carrying version-1 rows (hand-built).
        let mut m = IdentityManifest { version: 1, project: 1, ..Default::default() };
        for i in 0..=COMPACT_ABOVE_FILES {
            let key = format!("identity/p=1/map-x-{i}.parquet");
            s.put(
                &key,
                encode_links(&[Link { anon_id: format!("a{i}"), user_id: "u".into(), version: 1, ts: 0 }]).unwrap(),
            )
            .await
            .unwrap();
            m.files.push(SideFile { key, rows: 1, min_version: 1, max_version: 1 });
        }
        s.put_json(&keys::manifest_file(&dir, 1), &m).await.unwrap();
        assert!(compact(&s, 1).await.unwrap());
        let fresh = IdentityMap::load(&s, 1).await.unwrap();
        assert_eq!(fresh.resolve_after("a0", 0), Some("u"));
        assert_eq!(fresh.resolve_after("a0", 1), None);
    }
}
