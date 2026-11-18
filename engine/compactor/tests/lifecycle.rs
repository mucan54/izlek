//! Garbage collection, retention and the WAL lifecycle
//! (design §10.3–10.4, `spec/manifest.md` §10).

mod common;

use common::*;
use la_compactor::job::{self, Group, Outcome, ProjectEnv};
use la_compactor::{gc, run, wal};
use la_core::control::ProjectConfig;
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::{keys, manifest, Store};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

async fn compact_all(store: &Store, level: u8) {
    let m = day(store, 7, DAY).await;
    let e = Arc::new(ProjectEnv::load(store, 7).await.unwrap());
    let out = job::compact(&ctx(store), &e, DAY, Group { inputs: m.files.clone(), level }).await.unwrap();
    assert!(matches!(out, Outcome::Committed { .. }));
}

fn real_now() -> i64 {
    la_core::time::now_ms()
}

#[tokio::test]
async fn gc_spares_referenced_and_young_files() {
    let store = Store::memory();
    let a = add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    let b = add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    compact_all(&store, 1).await;
    let l1 = day(&store, 7, DAY).await.files[0].key.clone();
    // An output that was uploaded but whose manifest write has not happened (yet).
    let pending = keys::level_file(7, DAY, 1, 99, 0);
    store.put(&pending, &b"in flight"[..]).await.unwrap();
    let c = ctx(&store);

    // Right away: the replaced L0s and the in-flight output are younger than the grace period.
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now()).await.unwrap(), 0);
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 23 * HOUR_MS).await.unwrap(), 0);
    for k in [&a, &b, &l1, &pending] {
        assert!(store.exists(k).await.unwrap(), "{k}");
    }
    // Past grace + slack: everything unreferenced goes, the referenced file stays.
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 26 * HOUR_MS).await.unwrap(), 3);
    assert!(
        !store.exists(&a).await.unwrap() && !store.exists(&b).await.unwrap() && !store.exists(&pending).await.unwrap()
    );
    assert!(store.exists(&l1).await.unwrap());
    // However long we wait, a file listed by the current manifest is never touched.
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 5000 * DAY_MS).await.unwrap(), 0);
    assert!(store.exists(&l1).await.unwrap());
    assert_eq!(ids(&visible(&store, 7, DAY).await), [1, 2]);
    assert_eq!(c.metrics.counter_value("gc_deleted_total", &[]), 3);
}

fn age(dir: &std::path::Path, key: &str, hours: u64) {
    let f = std::fs::File::options().write(true).open(dir.join(key)).unwrap();
    f.set_modified(SystemTime::now() - Duration::from_secs(hours * 3600)).unwrap();
}

/// A file that stopped being referenced less than the grace period ago is
/// kept even if the object itself is old: a query may still be reading it
/// through the previous manifest version.
#[tokio::test]
async fn gc_waits_a_grace_period_after_a_file_was_dropped() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::local(dir.path()).unwrap();
    let a = add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    let b = add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    for key in all_keys(&store, &keys::data_day_prefix(7, DAY)).await {
        age(dir.path(), &key, 30);
    }
    compact_all(&store, 1).await; // version 3, written "now", drops A and B
    let c = ctx(&store);
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now()).await.unwrap(), 0, "dropped moments ago");
    assert!(store.exists(&a).await.unwrap() && store.exists(&b).await.unwrap());
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 23 * HOUR_MS).await.unwrap(), 0);
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 25 * HOUR_MS).await.unwrap(), 2);
    assert!(!store.exists(&a).await.unwrap() && !store.exists(&b).await.unwrap());
    assert_manifest_consistent(&store, 7, DAY).await;
}

#[tokio::test]
async fn gc_keeps_the_last_twenty_manifest_versions() {
    let store = Store::memory();
    for plan in 1..=30u64 {
        add_l0(&store, 7, DAY, plan, vec![row(plan, "u:a", "d", t0() + plan as i64)]).await;
    }
    let c = ctx(&store);
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now()).await.unwrap(), 0, "young versions are all kept");
    assert_eq!(gc::gc_day(&c, 7, DAY, real_now() + 26 * HOUR_MS).await.unwrap(), 10);
    let versions: Vec<u64> = all_keys(&store, &keys::data_manifest_dir(7, DAY))
        .await
        .iter()
        .filter_map(|k| keys::manifest_version(k))
        .collect();
    assert_eq!(versions, (11..=30).collect::<Vec<u64>>());
    assert_eq!(day(&store, 7, DAY).await.files.len(), 30, "data files are all still referenced");
    assert_eq!(visible(&store, 7, DAY).await.len(), 30);
}

#[tokio::test]
async fn side_tables_are_compacted_and_collected() {
    let store = Store::memory();
    add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    let mut map = IdentityMap::default();
    for i in 0..20u64 {
        let c = Candidate { anon_id: format!("dev-{i}"), user_id: format!("user-{i}"), ts: i as i64, uuid: uuid(i) };
        identity::append_delta(&store, 7, "1-0", i + 1, vec![c], &mut map).await.unwrap();
        let w = la_core::persons::PropWrite {
            person_key: format!("u:user-{i}"),
            key: "k".into(),
            value: "1".into(),
            ts: 1,
            seq: 0,
        };
        la_core::persons::append_delta(&store, 7, "1-0", i + 1, vec![w]).await.unwrap();
    }
    let c = ctx(&store);
    let all = |_: u32| true;
    run::cycle(&c, real_now(), true, false, &all).await.unwrap();
    // More than 16 delta files → merged into one; the deltas stay for the grace period.
    assert_eq!(IdentityMap::load(&store, 7).await.unwrap().file_count(), 1);
    assert_eq!(all_keys(&store, "identity/p=7").await.iter().filter(|k| k.ends_with(".parquet")).count(), 21);
    let r = run::cycle(&c, real_now() + 26 * HOUR_MS, true, false, &all).await.unwrap();
    assert!(r.gc_deleted >= 40, "{r:?}");
    let left: Vec<String> =
        all_keys(&store, "identity/p=7").await.into_iter().filter(|k| k.ends_with(".parquet")).collect();
    assert_eq!(left.len(), 1, "{left:?}");
    assert_eq!(all_keys(&store, "persons/p=7").await.iter().filter(|k| k.ends_with(".parquet")).count(), 1);
    assert_eq!(IdentityMap::load(&store, 7).await.unwrap().len(), 20);
    let table = la_core::persons::PersonTable::load(&store, 7, &IdentityMap::default()).await.unwrap();
    assert_eq!(table.len(), 20);
}

#[tokio::test]
async fn retention_removes_whole_days() {
    let store = Store::memory();
    add_l0(&store, 7, "2025-11-17", 1, vec![row(1, "u:a", "d", 1)]).await;
    add_l0(&store, 7, "2025-11-19", 2, vec![row(2, "u:a", "d", 1)]).await;
    add_l0(&store, 7, DAY, 3, vec![row(3, "u:a", "d", t0())]).await;
    store.put("replay/p=7/d=2026-10-01/aa/000000.bin.zst", &b"old"[..]).await.unwrap();
    store.put("replay/p=7/d=2026-11-10/aa/000000.bin.zst", &b"new"[..]).await.unwrap();
    let c = ctx(&store);
    let now = open_now(); // 2026-11-18
    assert_eq!(gc::retention_cutoff(now, 365).as_deref(), Some("2025-11-18"));
    assert_eq!(gc::retention_cutoff(now, 0), None);

    // retention_days = 0 means "keep forever".
    let forever = ProjectConfig { retention_days: 0, replay_retention_days: 0, ..Default::default() };
    let days = manifest::list_days(&store, 7).await.unwrap();
    assert!(gc::apply_retention(&c, 7, &forever, &days, now).await.unwrap().is_empty());
    assert_eq!(gc::apply_replay_retention(&c, 7, &forever, now).await.unwrap(), 0);

    // Defaults (365 / 30 days) through the cycle driver.
    let all = |_: u32| true;
    let r = run::cycle(&c, now, true, false, &all).await.unwrap();
    assert_eq!(r.days_expired, 1);
    assert_eq!(manifest::list_days(&store, 7).await.unwrap(), ["2025-11-19", DAY]);
    assert!(all_keys(&store, "data/p=7/d=2025-11-17").await.is_empty(), "manifests and files are both gone");
    assert_eq!(all_keys(&store, "replay").await, ["replay/p=7/d=2026-11-10/aa/000000.bin.zst"]);
    assert_eq!(c.metrics.counter_value("days_expired_total", &[]), 1);

    // A shorter retention published by the control plane takes effect on the next cycle.
    store.put(keys::PROJECTS, &br#"{"version":2,"projects":{"7":{"retention_days":30}}}"#[..]).await.unwrap();
    run::cycle(&c, now, false, false, &all).await.unwrap();
    assert_eq!(manifest::list_days(&store, 7).await.unwrap(), [DAY]);
}

#[tokio::test]
async fn wal_lifecycle_keeps_the_highest_plan() {
    let store = Store::memory();
    let now = open_now();
    for key in [
        "wal/dt=2026-10-01/hh=05/ing-01-000000000001.wal.zst", // > 30 days before `now`
        "wal/dt=2026-10-19/hh=00/ing-01-000000000002.wal.zst", // exactly at the cutoff day: kept
        "wal/dt=2026-11-18/hh=09/ing-01-000000000003.wal.zst",
        "wal/_plan/1-0/0000000001.json",
        "wal/_plan/1-0/0000000002.json",
        "wal/_plan/1-0/0000000003.json",
        "wal/_plan/2-1/0000000007.json",
        "wal/_done/1-0/ing-01-000000000001.ok",
        "wal/_done/1-0/_rebuild-0000000002.ok",
        "wal/_quarantine/1-0/ing-01-000000000001.json",
    ] {
        store.put(key, &b"x"[..]).await.unwrap();
    }
    let c = ctx(&store);
    // Segments go by the day in their key; everything else by object age.
    assert_eq!(wal::wal_lifecycle(&c, now).await.unwrap(), 1);
    assert!(!store.exists("wal/dt=2026-10-01/hh=05/ing-01-000000000001.wal.zst").await.unwrap());
    assert!(store.exists("wal/dt=2026-10-19/hh=00/ing-01-000000000002.wal.zst").await.unwrap());

    // 31 days later (object age): old plans and quarantine records go, the newest plan of each shardkey stays,
    // `_done` markers outlive their segments by three more days.
    let later = real_now() + 31 * DAY_MS;
    wal::wal_lifecycle(&c, later).await.unwrap();
    assert_eq!(all_keys(&store, "wal/_plan").await, ["wal/_plan/1-0/0000000003.json", "wal/_plan/2-1/0000000007.json"]);
    assert!(all_keys(&store, "wal/_quarantine").await.is_empty());
    assert_eq!(all_keys(&store, "wal/_done").await.len(), 2);
    wal::wal_lifecycle(&c, real_now() + 34 * DAY_MS).await.unwrap();
    assert!(all_keys(&store, "wal/_done").await.is_empty());
    assert_eq!(all_keys(&store, "wal/_plan").await.len(), 2, "the highest plan is never removed");
}

/// `closed: true` (not `retention_days: 0`, which keeps everything) removes
/// all state of a project and nothing of its neighbours.
#[tokio::test]
async fn closed_project_is_purged() {
    let store = Store::memory();
    for pid in [7, 70] {
        add_l0(&store, pid, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
        add_l0(&store, pid, "2026-11-01", 2, vec![row(2, "u:a", "d", 1)]).await;
        for key in [
            format!("replay/p={pid}/d=2026-11-10/aa/000000.bin.zst"),
            format!("identity/p={pid}/map-c00000001.parquet"),
            format!("identity/p={pid}/_m/00000001.json"),
            format!("persons/p={pid}/props-c00000001.parquet"),
            format!("tombstones/p={pid}/00000001.json"),
            keys::project_schema(pid),
        ] {
            store.put(&key, &b"{}"[..]).await.unwrap();
        }
    }
    let before_70: Vec<String> = all_keys(&store, "").await.into_iter().filter(|k| k.contains("p=70")).collect();
    assert!(before_70.len() >= 10);
    store
        .put(keys::PROJECTS, &br#"{"version":3,"projects":{"7":{"retention_days":0,"closed":true},"70":{}}}"#[..])
        .await
        .unwrap();
    let c = ctx(&store);
    let all = |_: u32| true;
    // Even a hot-only cycle deletes: closing is an explicit request.
    let r = run::cycle(&c, open_now(), false, false, &all).await.unwrap();
    assert_eq!(r.errors, 0);
    assert!(r.gc_deleted >= 10, "{r:?}");
    let left = all_keys(&store, "").await;
    let of_7: Vec<&String> = left.iter().filter(|k| k.contains("p=7/") || k.ends_with("p=7.json")).collect();
    assert!(of_7.is_empty(), "{of_7:?}");
    let after_70: Vec<String> = left.into_iter().filter(|k| k.contains("p=70")).collect();
    assert_eq!(after_70, before_70);
    assert_eq!(c.metrics.counter_value("projects_purged_total", &[]), 1);

    // Nothing left to find: the next cycle is a no-op for the closed project.
    let r = run::cycle(&c, open_now(), true, false, &all).await.unwrap();
    assert_eq!(r.projects, 1);
}
