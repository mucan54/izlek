//! Versioned manifests and the conditional-write commit protocol
//! (`spec/manifest.md` §2).

use crate::error::{Error, Result};
use crate::metrics;
use crate::{keys, Store};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::Duration;

/// A JSON document stored as `<dir>/<version:08>.json`, version starting at 1.
pub trait Versioned: Serialize + DeserializeOwned + Default + Send {
    fn version(&self) -> u64;
    fn set_version(&mut self, v: u64);
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct FileEntry {
    pub key: String,
    pub level: u8,
    pub rows: u64,
    pub bytes: u64,
    pub min_ts: i64,
    pub max_ts: i64,
    pub min_person: String,
    pub max_person: String,
    #[serde(default)]
    pub identity_version: u64,
    #[serde(default)]
    pub schema_version: u64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DayManifest {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub project: u32,
    #[serde(default)]
    pub day: String,
    #[serde(default)]
    pub created_at: i64,
    /// `shardkey → highest shipper plan applied to this day`.
    #[serde(default)]
    pub plans: BTreeMap<String, u64>,
    /// Largest tombstone `ts_ms` already applied to every file.
    #[serde(default)]
    pub tombstone_ts: i64,
    #[serde(default)]
    pub files: Vec<FileEntry>,
}

impl Versioned for DayManifest {
    fn version(&self) -> u64 {
        self.version
    }
    fn set_version(&mut self, v: u64) {
        self.version = v;
    }
}

impl DayManifest {
    pub fn plan_applied(&self, shardkey: &str, n: u64) -> bool {
        self.plans.get(shardkey).is_some_and(|applied| *applied >= n)
    }

    pub fn total_rows(&self) -> u64 {
        self.files.iter().map(|f| f.rows).sum()
    }
}

/// Highest version present under `dir` (0 when there is none).
pub async fn latest_version(store: &Store, dir: &str) -> Result<u64> {
    Ok(store.list(dir).await?.iter().filter_map(|o| keys::manifest_version(&o.key)).max().unwrap_or(0))
}

/// Loads the newest manifest in `dir`, or `T::default()` (version 0) if the
/// directory is empty.
pub async fn load_latest<T: Versioned>(store: &Store, dir: &str) -> Result<T> {
    // A version can only disappear through GC of *old* versions, so retrying
    // the listing is enough if the object we picked vanished underneath us.
    for _ in 0..3 {
        let v = latest_version(store, dir).await?;
        if v == 0 {
            return Ok(T::default());
        }
        if let Some(m) = store.get_json::<T>(&keys::manifest_file(dir, v)).await? {
            return Ok(m);
        }
    }
    Err(Error::invalid(format!("manifest directory {dir} keeps changing underneath the reader")))
}

pub async fn load_day(store: &Store, project: u32, day: &str) -> Result<DayManifest> {
    let mut m: DayManifest = load_latest(store, &keys::data_manifest_dir(project, day)).await?;
    if m.version == 0 {
        m.project = project;
        m.day = day.to_owned();
    }
    Ok(m)
}

/// Outcome of [`commit`].
#[derive(Debug, Clone, PartialEq)]
pub struct Committed<T> {
    pub manifest: T,
    /// `false` when the mutation reported "nothing to do" and nothing was written.
    pub written: bool,
    pub conflicts: u32,
}

const MAX_ATTEMPTS: u32 = 64;

/// Read-modify-write with create-if-absent on `<dir>/<v+1>.json`.
///
/// `mutate` receives the latest manifest (version already loaded) and returns
/// `Ok(true)` if it changed something. It may be called several times when
/// other writers win the race, so it must be a pure function of its input.
pub async fn commit<T, F>(store: &Store, dir: &str, mut mutate: F) -> Result<Committed<T>>
where
    T: Versioned,
    F: FnMut(&mut T) -> Result<bool>,
{
    let mut conflicts = 0u32;
    loop {
        let mut m: T = load_latest(store, dir).await?;
        let base = m.version();
        if !mutate(&mut m)? {
            return Ok(Committed { manifest: m, written: false, conflicts });
        }
        m.set_version(base + 1);
        if store.put_json_if_absent(&keys::manifest_file(dir, base + 1), &m).await? {
            return Ok(Committed { manifest: m, written: true, conflicts });
        }
        conflicts += 1;
        metrics::MANIFEST_CONFLICTS.inc();
        if conflicts >= MAX_ATTEMPTS {
            return Err(Error::Conflict(dir.to_owned()));
        }
        // Exponential backoff with full jitter, capped at ~1 s.
        let cap = 10u64 << conflicts.min(7);
        tokio::time::sleep(Duration::from_millis(rand::random_range(0..=cap))).await;
    }
}

/// Commits a change to a day manifest, filling in identity fields on first write.
pub async fn commit_day<F>(store: &Store, project: u32, day: &str, mut mutate: F) -> Result<Committed<DayManifest>>
where
    F: FnMut(&mut DayManifest) -> Result<bool>,
{
    let dir = keys::data_manifest_dir(project, day);
    commit(store, &dir, |m: &mut DayManifest| {
        m.project = project;
        m.day = day.to_owned();
        let changed = mutate(m)?;
        if changed {
            m.created_at = crate::time::now_ms();
        }
        Ok(changed)
    })
    .await
}

/// Days that have a data directory for `project`, sorted ascending.
pub async fn list_days(store: &Store, project: u32) -> Result<Vec<String>> {
    Ok(store
        .list_dirs(&keys::data_project_prefix(project))
        .await?
        .iter()
        .filter_map(|d| keys::parse_day_dir(d).map(str::to_owned))
        .collect())
}

/// Project ids that have any data, sorted ascending.
pub async fn list_projects(store: &Store) -> Result<Vec<u32>> {
    let mut out: Vec<u32> = store.list_dirs("data").await?.iter().filter_map(|d| keys::parse_project_dir(d)).collect();
    out.sort_unstable();
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(key: &str) -> FileEntry {
        FileEntry { key: key.into(), rows: 1, ..Default::default() }
    }

    #[tokio::test]
    async fn first_commit_creates_version_one() {
        let s = Store::memory();
        let c = commit_day(&s, 7, "2026-11-18", |m| {
            m.files.push(file("a"));
            m.plans.insert("1-0".into(), 1);
            Ok(true)
        })
        .await
        .unwrap();
        assert!(c.written);
        assert_eq!((c.manifest.version, c.manifest.project, c.manifest.day.as_str()), (1, 7, "2026-11-18"));
        let m = load_day(&s, 7, "2026-11-18").await.unwrap();
        assert_eq!(m, c.manifest);
        assert!(m.plan_applied("1-0", 1) && !m.plan_applied("1-0", 2) && !m.plan_applied("1-1", 1));
        assert_eq!(list_days(&s, 7).await.unwrap(), ["2026-11-18"]);
        assert_eq!(list_projects(&s).await.unwrap(), [7]);
    }

    #[tokio::test]
    async fn noop_mutation_writes_nothing() {
        let s = Store::memory();
        let c = commit_day(&s, 7, "2026-11-18", |_| Ok(false)).await.unwrap();
        assert!(!c.written);
        assert_eq!(latest_version(&s, &keys::data_manifest_dir(7, "2026-11-18")).await.unwrap(), 0);
        assert_eq!(load_day(&s, 7, "2026-11-18").await.unwrap().project, 7);
    }

    #[tokio::test]
    async fn unknown_fields_and_missing_fields_are_tolerated() {
        let s = Store::memory();
        let dir = keys::data_manifest_dir(1, "2026-01-01");
        s.put(&keys::manifest_file(&dir, 3), &br#"{"version":3,"future_field":{"x":1},"files":[{"key":"k","level":2,"rows":5,"bytes":9,"min_ts":1,"max_ts":2,"min_person":"a:x","max_person":"u:y","new":true}]}"#[..]).await.unwrap();
        let m = load_day(&s, 1, "2026-01-01").await.unwrap();
        assert_eq!((m.version, m.files.len(), m.files[0].rows, m.files[0].identity_version), (3, 1, 5, 0));
    }

    /// `spec/manifest.md` §2 / design §17.5: concurrent writers — every
    /// mutation lands exactly once and versions are gapless.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn concurrent_commits_are_linearized() {
        let dir = tempfile::tempdir().unwrap();
        for store in [Store::memory(), Store::local(dir.path()).unwrap()] {
            let writers = 12;
            let mut tasks = Vec::new();
            for i in 0..writers {
                let s = store.clone();
                tasks.push(tokio::spawn(async move {
                    commit_day(&s, 9, "2026-11-18", |m| {
                        let key = format!("f{i}");
                        if m.files.iter().any(|f| f.key == key) {
                            return Ok(false);
                        }
                        m.files.push(file(&key));
                        Ok(true)
                    })
                    .await
                    .unwrap()
                }));
            }
            for t in tasks {
                assert!(t.await.unwrap().written);
            }
            let m = load_day(&store, 9, "2026-11-18").await.unwrap();
            assert_eq!(m.version, writers);
            let mut keys_seen: Vec<_> = m.files.iter().map(|f| f.key.clone()).collect();
            keys_seen.sort();
            keys_seen.dedup();
            assert_eq!(keys_seen.len() as u64, writers);
            let versions = store.list(&keys::data_manifest_dir(9, "2026-11-18")).await.unwrap();
            assert_eq!(versions.len() as u64, writers, "gapless: one object per version");
        }
    }
}
