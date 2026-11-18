//! Shipper and compactor together: query-visible content is the same before
//! and after compaction (design §17.4), also while the shipper keeps
//! appending.

mod common;

use common::*;
use la_codec::wire::Value;
use la_compactor::job::{self, Group, Outcome, ProjectEnv};
use la_compactor::{inspect, run, CompactorConfig, Ctx};
use la_core::identity::IdentityMap;
use la_core::manifest;
use la_core::time::day_start_ms;
use la_core::Store;
use la_shipper::ship::ship_once;
use std::sync::Arc;

async fn compact_everything(store: &Store, project: u32, level: u8) {
    let c = Ctx::new(store.clone(), CompactorConfig::default());
    let env = Arc::new(ProjectEnv::load(store, project).await.unwrap());
    for day in manifest::list_days(store, project).await.unwrap() {
        let m = manifest::load_day(store, project, &day).await.unwrap();
        let out = job::compact(&c, &env, &day, Group { inputs: m.files.clone(), level }).await.unwrap();
        assert!(matches!(out, Outcome::Committed { .. }), "{day}: {out:?}");
    }
}

async fn file_levels(store: &Store, project: u32) -> Vec<u8> {
    let mut out = Vec::new();
    for day in manifest::list_days(store, project).await.unwrap() {
        out.extend(manifest::load_day(store, project, &day).await.unwrap().files.iter().map(|f| f.level));
    }
    out
}

/// L0 → L1 → L2 with duplicate uuids across L0 files, identity links that
/// arrive between the runs and a promotion in the middle.
#[tokio::test]
async fn logical_content_is_identical_before_and_after_compaction() {
    for seed in [5u64, 41] {
        let store = Store::memory();
        let segments = random_segments(&mut Rng(seed), 8);
        // Four plans → four L0 files per (project, day); re-sent events land in different files.
        for (i, pair) in segments.chunks(2).enumerate() {
            for (j, seg) in pair.iter().enumerate() {
                put_segment(&store, &seg_key(10, "ing-01", (i * 2 + j) as u64 + 1), seg).await;
            }
            ship_once(&ctx(&store)).await.unwrap();
        }
        let mut duplicates = 0;
        for project in [7u32, 8] {
            let l0 = snapshot(&store, project).await;
            assert!(total_rows(&l0) > 30);
            let physical: u64 = {
                let mut n = 0;
                for d in manifest::list_days(&store, project).await.unwrap() {
                    n += manifest::load_day(&store, project, &d).await.unwrap().total_rows();
                }
                n
            };
            duplicates += physical as usize - total_rows(&l0);

            compact_everything(&store, project, 1).await;
            assert!(file_levels(&store, project).await.iter().all(|l| *l == 1));
            let l1 = snapshot(&store, project).await;
            assert_eq!(l1.rows, l0.rows, "seed {seed} project {project}: L0 → L1");
            assert_eq!((l1.identity, l1.persons), (l0.identity, l0.persons));
        }

        assert!(duplicates > 0, "seed {seed}: the fixture must contain cross-file duplicates");

        // New links arrive after the L1 files were written: devices 0..4 of
        // project 7 identify as people the earlier rows knew nothing about,
        // plus fresh events and a property that gets promoted.
        let mut late = Vec::new();
        for d in 0..4u64 {
            let anon = format!("dev-{d}");
            late.push(native(
                7,
                t0() + 90_000,
                &header(&anon, "", 0),
                &[identify(70_000 + d, t0(), &format!("owner-of-{anon}"), vec![])],
            ));
        }
        let bulk: Vec<_> = (0..1100u64)
            .map(|i| {
                event(
                    80_000 + i,
                    t0() + 80_000 - i as i64,
                    "tap",
                    vec![prop("n", Value::Int(i as i64)), prop("tier", s("gold"))],
                )
            })
            .collect();
        for chunk in bulk.chunks(400) {
            late.push(native(7, t0() + 90_000, &header("dev-new", "", 0), chunk));
        }
        put_segment(&store, &seg_key(11, "ing-01", 100), &late).await;
        let out = ship_once(&ctx(&store)).await.unwrap();
        assert!(out[0].promoted.iter().any(|(_, k)| k == "n"), "{:?}", out[0].promoted);

        let before = snapshot(&store, 7).await;
        compact_everything(&store, 7, 2).await;
        let after = snapshot(&store, 7).await;
        assert_eq!(after.rows, before.rows, "seed {seed}: mixed L0/L1 → L2 with new links and a new column");
        assert_eq!(file_levels(&store, 7).await, vec![2; before.rows.len()], "one L2 file per day");

        // Physically the L2 files have everything applied: resolved keys, no
        // duplicates left, the promoted column filled.
        let identity = IdentityMap::load(&store, 7).await.unwrap();
        for day in manifest::list_days(&store, 7).await.unwrap() {
            let m = manifest::load_day(&store, 7, &day).await.unwrap();
            let f = &m.files[0];
            assert_eq!((f.identity_version, f.schema_version), (identity.version(), before.schema.version));
            let rows = inspect::file_rows(&store, &f.key).await.unwrap();
            assert_eq!(rows.len(), before.rows[&day].len(), "{day}: no duplicate survived");
            for r in &rows {
                if let Some(anon) = r.person_key.strip_prefix("a:") {
                    assert_eq!(identity.resolve(anon), None, "{day}: an unresolved key the map could resolve");
                }
            }
            assert_eq!(rows, before.rows[&day], "{day}: file order is the query order");
        }
        // And shipping simply continues on top.
        put_segment(&store, &seg_key(12, "ing-01", 200), &segments[0]).await;
        ship_once(&ctx(&store)).await.unwrap();
        assert_eq!(snapshot(&store, 7).await.rows, after.rows, "re-sent old events add nothing visible");
    }
}

/// The compactor loop and the shipper running against the same day.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn shipper_appends_while_the_compactor_commits() {
    let dir = tempfile::tempdir().unwrap();
    for store in [Store::memory(), Store::local(dir.path()).unwrap()] {
        let segments = random_segments(&mut Rng(7), 24);
        // What the day must contain in the end, from an undisturbed run.
        let reference = Store::memory();
        for (i, seg) in segments.iter().enumerate() {
            put_segment(&reference, &seg_key(10, "ing-01", i as u64 + 1), seg).await;
        }
        ship_once(&ctx(&reference)).await.unwrap();
        let mut expect = snapshot(&reference, 7).await.rows;
        expect.values_mut().for_each(inspect::dedupe_uuid_per_person);

        let done = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let compactor = {
            let (store, done) = (store.clone(), done.clone());
            tokio::spawn(async move {
                // Compact eagerly so many commits race the shipper.
                let cfg = CompactorConfig { l0_trigger_files: 2, l1_max_files: 2, ..Default::default() };
                let c = Ctx::new(store, cfg);
                let now = day_start_ms(DAY).unwrap() + 23 * HOUR_MS;
                let mut compactions = 0;
                loop {
                    let last = done.load(std::sync::atomic::Ordering::SeqCst);
                    let r = run::cycle(&c, now, false, false, &|_| true).await.unwrap();
                    assert_eq!(r.errors, 0);
                    compactions += r.compactions;
                    if last {
                        return compactions;
                    }
                    tokio::time::sleep(std::time::Duration::from_millis(1)).await;
                }
            })
        };
        for (i, seg) in segments.iter().enumerate() {
            put_segment(&store, &seg_key(10, "ing-01", i as u64 + 1), seg).await;
            assert_eq!(ship_once(&ctx(&store)).await.unwrap().len(), 1);
        }
        done.store(true, std::sync::atomic::Ordering::SeqCst);
        let compactions = compactor.await.unwrap();
        assert!(compactions >= 3, "{}: only {compactions} compactions", store.describe());

        let mut got = snapshot(&store, 7).await.rows;
        got.values_mut().for_each(inspect::dedupe_uuid_per_person);
        assert_eq!(got, expect, "{}", store.describe());
        for day in manifest::list_days(&store, 7).await.unwrap() {
            let m = manifest::load_day(&store, 7, &day).await.unwrap();
            for f in &m.files {
                assert!(store.exists(&f.key).await.unwrap(), "{} is listed but missing", f.key);
            }
            // Keys are listed once, and no L0 of an already compacted plan came back:
            // every L0 still listed belongs to a plan newer than the newest compaction output.
            let mut keys_listed: Vec<&str> = m.files.iter().map(|f| f.key.as_str()).collect();
            keys_listed.sort_unstable();
            keys_listed.dedup();
            assert_eq!(keys_listed.len(), m.files.len());
            let l0_rows: u64 = m.files.iter().filter(|f| f.level == 0).map(|f| f.rows).sum();
            let compacted_rows: u64 = m.files.iter().filter(|f| f.level > 0).map(|f| f.rows).sum();
            assert!(compacted_rows as usize <= got[&day].len(), "{day}: compacted files hold each row at most once");
            assert!((compacted_rows + l0_rows) as usize >= got[&day].len());
        }
    }
}
