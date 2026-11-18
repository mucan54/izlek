//! Fixtures for the compactor integration tests: events files are fabricated
//! directly, the way a shipper plan would have written them.
#![allow(dead_code)]

use la_compactor::inspect::{self, LogicalRow};
use la_compactor::{CompactorConfig, Ctx};
use la_core::events::{sort_and_dedupe, EventRow};
use la_core::identity::IdentityMap;
use la_core::manifest::{self, DayManifest};
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::schema::ProjectSchema;
use la_core::time::day_start_ms;
use la_core::tombstone::{self, TombstoneSet};
use la_core::{keys, Store};

pub const DAY: &str = "2026-11-18";
pub const HOUR_MS: i64 = 3_600_000;
pub const DAY_MS: i64 = 24 * HOUR_MS;

pub fn t0() -> i64 {
    day_start_ms(DAY).unwrap() + 10 * HOUR_MS
}

/// While [`DAY`] is still open.
pub fn open_now() -> i64 {
    day_start_ms(DAY).unwrap() + 20 * HOUR_MS
}

/// After [`DAY`] closed and the L2 delay passed.
pub fn closed_now() -> i64 {
    day_start_ms(DAY).unwrap() + DAY_MS + 7 * HOUR_MS
}

pub fn ctx(store: &Store) -> Ctx {
    Ctx::new(store.clone(), CompactorConfig::default())
}

pub fn uuid(n: u64) -> [u8; 16] {
    let mut u = [0u8; 16];
    u[8..].copy_from_slice(&n.to_be_bytes());
    u
}

pub fn row(id: u64, person: &str, anon: &str, ts: i64) -> EventRow {
    EventRow {
        uuid: uuid(id),
        ts,
        recv_ts: ts + 100,
        event: "tap".into(),
        person_key: person.into(),
        anon_id: anon.into(),
        user_id: person.strip_prefix("u:").map(str::to_owned),
        props: format!("{{\"n\":{id}}}"),
        ..Default::default()
    }
}

/// Writes rows as the L0 file of shipper plan `plan` and appends it to the
/// day manifest, like plan execution does.
pub async fn add_l0(store: &Store, project: u32, day: &str, plan: u64, mut rows: Vec<EventRow>) -> String {
    sort_and_dedupe(&mut rows);
    let schema = ProjectSchema::load(store, project).await.unwrap();
    let identity = IdentityMap::load(store, project).await.unwrap();
    let meta = EventsFileMeta {
        project,
        day: day.to_owned(),
        level: 0,
        schema_version: schema.version,
        identity_version: identity.version(),
    };
    let (bytes, mut entry) = write_events_file(&rows, &schema.columns, &meta).unwrap();
    entry.key = keys::l0_file(project, day, "1-0", plan);
    store.put(&entry.key, bytes).await.unwrap();
    let key = entry.key.clone();
    manifest::commit_day(store, project, day, |m| {
        m.files.push(entry.clone());
        m.plans.insert("1-0".into(), plan);
        Ok(true)
    })
    .await
    .unwrap();
    key
}

pub async fn day(store: &Store, project: u32, day: &str) -> DayManifest {
    manifest::load_day(store, project, day).await.unwrap()
}

/// Query-visible rows of a day with the current identity map and all tombstones applied.
pub async fn visible(store: &Store, project: u32, d: &str) -> Vec<LogicalRow> {
    let identity = IdentityMap::load(store, project).await.unwrap();
    let tombs = TombstoneSet::build(&tombstone::load_all(store, project).await.unwrap(), &identity, 0);
    inspect::day_rows(store, project, d, &identity, Some(&tombs)).await.unwrap()
}

pub fn ids(rows: &[LogicalRow]) -> Vec<u64> {
    rows.iter().map(|r| u64::from_be_bytes(r.uuid[8..].try_into().unwrap())).collect()
}

pub async fn all_keys(store: &Store, prefix: &str) -> Vec<String> {
    store.list(prefix).await.unwrap().into_iter().map(|o| o.key).collect()
}

/// Every file a manifest lists must exist with the advertised size.
pub async fn assert_manifest_consistent(store: &Store, project: u32, d: &str) {
    let m = day(store, project, d).await;
    let objs = store.list(&keys::data_day_prefix(project, d)).await.unwrap();
    for f in &m.files {
        let o = objs.iter().find(|o| o.key == f.key).unwrap_or_else(|| panic!("{} is listed but missing", f.key));
        assert_eq!(o.size, f.bytes, "{}", f.key);
    }
    let mut keys_seen: Vec<&str> = m.files.iter().map(|f| f.key.as_str()).collect();
    keys_seen.sort();
    keys_seen.dedup();
    assert_eq!(keys_seen.len(), m.files.len(), "a file is listed twice");
}
