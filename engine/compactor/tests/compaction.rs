//! Compaction jobs and the manifest commit protocol (design §10.1–10.2).

mod common;

use common::*;
use la_compactor::job::{self, CommitPlan, Group, Outcome, ProjectEnv};
use la_compactor::run;
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::schema::{PromotedColumn, PropType, ProjectSchema};
use la_core::{keys, manifest, Store};
use std::sync::Arc;

async fn env(store: &Store, project: u32) -> Arc<ProjectEnv> {
    Arc::new(ProjectEnv::load(store, project).await.unwrap())
}

fn group(m: &manifest::DayManifest, level: u8) -> Group {
    Group { inputs: m.files.clone(), level }
}

#[tokio::test]
async fn l0_files_merge_into_one_l1_and_then_l2() {
    let store = Store::memory();
    for plan in 1..=5u64 {
        let rows = (0..200).map(|i| row(plan * 1000 + i, &format!("u:{:03}", (i * 7 + plan) % 60), "d", t0() + i as i64)).collect();
        add_l0(&store, 7, DAY, plan, rows).await;
    }
    let before = visible(&store, 7, DAY).await;
    assert_eq!(before.len(), 1000);
    let c = ctx(&store);
    let e = env(&store, 7).await;
    let m = day(&store, 7, DAY).await;
    let Outcome::Committed { manifest: m1, added } = job::compact(&c, &e, DAY, group(&m, 1)).await.unwrap() else {
        panic!("not committed")
    };
    assert_eq!((m1.version, m1.files.len(), added.len()), (6, 1, 1));
    assert_eq!(m1.files[0].key, format!("data/p=7/d={DAY}/L1-00000006-000.parquet"), "named after the adding version");
    assert_eq!((m1.files[0].level, m1.files[0].rows), (1, 1000));
    assert_eq!(m1.plans.get("1-0"), Some(&5), "plans survive compaction");
    assert_eq!(visible(&store, 7, DAY).await, before);
    assert_manifest_consistent(&store, 7, DAY).await;
    // The replaced L0 objects are left for GC (24 h grace), not deleted here.
    assert!(store.exists(&keys::l0_file(7, DAY, "1-0", 1)).await.unwrap());

    let Outcome::Committed { manifest: m2, .. } = job::compact(&c, &e, DAY, group(&m1, 2)).await.unwrap() else {
        panic!("not committed")
    };
    assert_eq!(m2.files[0].key, format!("data/p=7/d={DAY}/L2-00000007-000.parquet"));
    assert_eq!(visible(&store, 7, DAY).await, before);
    assert_eq!(c.metrics.counter_value("compactions_total", &[("level", "1")]), 1);
    assert_eq!(c.metrics.counter_value("rows_rewritten_total", &[]), 2000);
    assert!(c.metrics.counter_value("bytes_rewritten_total", &[]) > 0);
    assert!(c.metrics.render().contains("la_compactor_compaction_seconds_count{level=\"2\"} 1"));
}

#[tokio::test]
async fn identity_links_are_applied_and_duplicates_dropped() {
    let store = Store::memory();
    // dev-1's events were written before it identified as alice.
    add_l0(&store, 7, DAY, 1, vec![row(1, "a:dev-1", "dev-1", t0()), row(2, "a:dev-2", "dev-2", t0()), row(3, "u:bob", "x", t0())]).await;
    // The same event again from another plan (retry through another node), plus a later resend with a shifted timestamp.
    add_l0(&store, 7, DAY, 2, vec![row(1, "a:dev-1", "dev-1", t0()), row(3, "u:bob", "x", t0() + 7), row(4, "u:alice", "dev-1", t0() + 1)]).await;
    let mut map = IdentityMap::default();
    identity::append_delta(
        &store,
        7,
        "1-0",
        3,
        vec![Candidate { anon_id: "dev-1".into(), user_id: "alice".into(), ts: t0(), uuid: [9; 16] }],
        &mut map,
    )
    .await
    .unwrap();

    let before = visible(&store, 7, DAY).await;
    // Query rule: only rows equal in (person_key, ts, uuid) collapse.
    assert_eq!(ids(&before), [2, 1, 4, 3, 3]);
    let c = ctx(&store);
    let m = day(&store, 7, DAY).await;
    job::compact(&c, &env(&store, 7).await, DAY, group(&m, 1)).await.unwrap();
    let after = visible(&store, 7, DAY).await;
    // Compaction is stricter: one row per (person, uuid), the earliest.
    assert_eq!(ids(&after), [2, 1, 4, 3]);
    let mut expect = before.clone();
    la_compactor::inspect::dedupe_uuid_per_person(&mut expect);
    assert_eq!(after, expect);
    assert_eq!(after[3].ts, t0(), "the earliest copy of a repeated uuid is the one kept");

    // Physically the file now carries the resolved keys, in order, and says so.
    let m = day(&store, 7, DAY).await;
    assert_eq!((m.files[0].identity_version, m.files[0].rows), (1, 4));
    assert_eq!((m.files[0].min_person.as_str(), m.files[0].max_person.as_str()), ("a:dev-2", "u:bob"));
    let physical = la_compactor::inspect::file_rows(&store, &m.files[0].key).await.unwrap();
    let keys_in_file: Vec<&str> = physical.iter().map(|r| r.person_key.as_str()).collect();
    assert_eq!(keys_in_file, ["a:dev-2", "u:alice", "u:alice", "u:bob"]);
    assert_eq!(physical[1].user_id, None, "user_id keeps what the event carried");
    assert_eq!(c.metrics.counter_value("duplicates_dropped_total", &[]), 2);
}

#[tokio::test]
async fn rewrite_follows_the_promoted_schema() {
    let store = Store::memory();
    let with = |id: u64, props: &str| {
        let mut r = row(id, "u:a", "d", t0() + id as i64);
        r.props = props.into();
        r
    };
    add_l0(
        &store,
        7,
        DAY,
        1,
        vec![with(1, r#"{"amount":5,"plan":"pro"}"#), with(2, r#"{"amount":"lots","plan":7}"#), with(3, "{}")],
    )
    .await;
    // The schema gains two columns afterwards.
    let mut schema = ProjectSchema::load(&store, 7).await.unwrap();
    schema.version = 1;
    schema.columns = vec![
        PromotedColumn { key: "plan".into(), ty: PropType::String, since: 1 },
        PromotedColumn { key: "amount".into(), ty: PropType::Float, since: 1 },
    ];
    schema.save(&store).await.unwrap();
    let mut newer = with(4, "{}");
    newer.promoted = vec![Some(la_core::events::Cell::Str("free".into())), Some(la_core::events::Cell::Float(1.5))];
    add_l0(&store, 7, DAY, 2, vec![newer]).await;

    let before = visible(&store, 7, DAY).await;
    let m = day(&store, 7, DAY).await;
    assert_eq!(m.files.iter().map(|f| f.schema_version).collect::<Vec<_>>(), [0, 1]);
    job::compact(&ctx(&store), &env(&store, 7).await, DAY, group(&m, 1)).await.unwrap();
    assert_eq!(visible(&store, 7, DAY).await, before, "logical content does not depend on where a value is stored");

    let m = day(&store, 7, DAY).await;
    assert_eq!(m.files[0].schema_version, 1);
    let data = store.get(&m.files[0].key).await.unwrap().unwrap();
    let rows: Vec<_> = la_core::parquet_io::read_parquet(data)
        .unwrap()
        .iter()
        .flat_map(|b| la_core::events::batch_to_rows(b, &schema.columns).unwrap())
        .collect();
    use la_core::events::Cell;
    assert_eq!(rows[0].promoted, [Some(Cell::Str("pro".into())), Some(Cell::Float(5.0))]);
    assert_eq!(rows[0].props, "{}");
    assert_eq!(rows[1].promoted, [None, None], "type conflicts stay in props");
    assert_eq!(rows[1].props, r#"{"amount":"lots","plan":7}"#);
    assert_eq!(rows[3].promoted, [Some(Cell::Str("free".into())), Some(Cell::Float(1.5))]);
}

#[tokio::test]
async fn large_days_split_by_person_range() {
    let store = Store::memory();
    for plan in 1..=3u64 {
        let rows = (0..4000)
            .map(|i| {
                let mut r = row(plan * 10_000 + i, &format!("u:{:04}", (i * 13 + plan * 5) % 800), "d", t0() + i as i64);
                r.props = format!("{{\"n\":{i},\"pad\":\"{}\"}}", "x".repeat(40 + (i % 17) as usize));
                r
            })
            .collect();
        add_l0(&store, 7, DAY, plan, rows).await;
    }
    let before = visible(&store, 7, DAY).await;
    let mut c = ctx(&store);
    c.cfg.target_file_bytes = 40_000;
    c.cfg.batch_rows = 512;
    let m = day(&store, 7, DAY).await;
    let Outcome::Committed { manifest: m2, added } = job::compact(&c, &env(&store, 7).await, DAY, group(&m, 2)).await.unwrap() else {
        panic!("not committed")
    };
    assert!(added.len() >= 3, "{} parts", added.len());
    for (i, f) in added.iter().enumerate() {
        assert_eq!(f.key, format!("data/p=7/d={DAY}/L2-{:08}-{i:03}.parquet", m2.version));
        assert!(f.min_person <= f.max_person);
        if i > 0 {
            assert!(added[i - 1].max_person < f.min_person, "person ranges must not overlap");
        }
    }
    assert_eq!(added.iter().map(|f| f.rows).sum::<u64>(), 12_000);
    assert_eq!(visible(&store, 7, DAY).await, before);
    assert_manifest_consistent(&store, 7, DAY).await;
}

/// A commit that loses the race against a shipper append keeps the new file.
#[tokio::test]
async fn commit_preserves_files_added_meanwhile() {
    let store = Store::memory();
    add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    let c = ctx(&store);
    let e = env(&store, 7).await;
    let m = day(&store, 7, DAY).await;
    let staged = job::stage(&c, &e, DAY, vec![group(&m, 1)]).await.unwrap().unwrap();
    // The shipper appends plan 3 between staging and commit.
    let late = add_l0(&store, 7, DAY, 3, vec![row(3, "u:c", "d", t0())]).await;
    let Outcome::Committed { manifest: after, added } =
        job::commit(&c, 7, DAY, &staged, &CommitPlan::replace_inputs()).await.unwrap()
    else {
        panic!("not committed")
    };
    assert_eq!(after.version, 4);
    assert_eq!(added[0].key, format!("data/p=7/d={DAY}/L1-00000004-000.parquet"));
    let listed: Vec<&str> = after.files.iter().map(|f| f.key.as_str()).collect();
    assert_eq!(listed, [late.as_str(), added[0].key.as_str()], "inputs gone, late L0 kept, output added");
    assert_eq!(after.plans.get("1-0"), Some(&3));
    assert_eq!(ids(&visible(&store, 7, DAY).await), [1, 2, 3]);
}

#[tokio::test]
async fn second_compactor_gives_up_when_inputs_vanished() {
    let store = Store::memory();
    add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    let c = ctx(&store);
    let e = env(&store, 7).await;
    let m = day(&store, 7, DAY).await;
    let first = job::stage(&c, &e, DAY, vec![group(&m, 1)]).await.unwrap().unwrap();
    let second = job::stage(&c, &e, DAY, vec![group(&m, 1)]).await.unwrap().unwrap();
    assert!(matches!(job::commit(&c, 7, DAY, &first, &CommitPlan::replace_inputs()).await.unwrap(), Outcome::Committed { .. }));
    let keys_before = all_keys(&store, "data").await;
    let version_before = day(&store, 7, DAY).await.version;
    assert!(matches!(job::commit(&c, 7, DAY, &second, &CommitPlan::replace_inputs()).await.unwrap(), Outcome::InputVanished));
    assert_eq!(all_keys(&store, "data").await, keys_before, "the loser leaves nothing behind");
    assert_eq!(day(&store, 7, DAY).await.version, version_before);
    assert_eq!(ids(&visible(&store, 7, DAY).await), [1, 2]);
    // An input object that disappeared is handled the same way at staging time.
    add_l0(&store, 7, DAY, 3, vec![row(3, "u:c", "d", t0())]).await;
    let m = day(&store, 7, DAY).await;
    store.delete(&m.files[0].key).await.unwrap();
    assert!(job::stage(&c, &e, DAY, vec![group(&m, 1)]).await.unwrap().is_none());
}

/// Output names left behind by a writer that died before its manifest write
/// can never be reused; the commit steps over them.
#[tokio::test]
async fn orphaned_output_names_do_not_block_compaction() {
    let store = Store::memory();
    add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    let orphan = keys::level_file(7, DAY, 1, 3, 0);
    store.put(&orphan, &b"left by a crashed compactor"[..]).await.unwrap();
    let c = ctx(&store);
    let m = day(&store, 7, DAY).await;
    let Outcome::Committed { manifest: after, added } =
        job::compact(&c, &env(&store, 7).await, DAY, group(&m, 1)).await.unwrap()
    else {
        panic!("not committed")
    };
    assert_eq!(after.version, 4, "version 3 was spent stepping over the orphan");
    assert_eq!(added[0].key, keys::level_file(7, DAY, 1, 4, 0));
    assert_eq!(store.get(&orphan).await.unwrap().unwrap(), &b"left by a crashed compactor"[..], "never overwritten");
    assert_eq!(ids(&visible(&store, 7, DAY).await), [1, 2]);
    assert_manifest_consistent(&store, 7, DAY).await;
}

/// The cycle driver: L0 pile-up → L1 while the day is open, everything → L2
/// once it closed, later L0s wait until they pile up again.
#[tokio::test]
async fn cycle_applies_the_level_triggers() {
    let store = Store::memory();
    for plan in 1..=8u64 {
        add_l0(&store, 7, DAY, plan, vec![row(plan, &format!("u:{plan}"), "d", t0())]).await;
    }
    let c = ctx(&store);
    let all = |_: u32| true;
    let r = run::cycle(&c, open_now(), false, false, &all).await.unwrap();
    assert_eq!((r.projects, r.compactions, r.errors), (1, 0, 0), "8 L0 files are not more than 8");
    add_l0(&store, 7, DAY, 9, vec![row(9, "u:9", "d", t0())]).await;
    let r = run::cycle(&c, open_now(), false, false, &all).await.unwrap();
    assert_eq!(r.compactions, 1);
    let m = day(&store, 7, DAY).await;
    assert_eq!(m.files.iter().map(|f| f.level).collect::<Vec<_>>(), [1]);
    add_l0(&store, 7, DAY, 10, vec![row(10, "u:10", "d", t0())]).await;
    assert_eq!(run::cycle(&c, open_now(), false, false, &all).await.unwrap().compactions, 0);
    assert!(c.metrics.render().contains("la_compactor_l0_files{project=\"7\"} 1"));

    // Day closed + 6 h: one L2.
    let r = run::cycle(&c, closed_now(), true, false, &all).await.unwrap();
    assert_eq!(r.compactions, 1);
    let m = day(&store, 7, DAY).await;
    assert_eq!(m.files.iter().map(|f| f.level).collect::<Vec<_>>(), [2]);
    assert_eq!(ids(&visible(&store, 7, DAY).await).len(), 10);
    // A late L0 waits …
    add_l0(&store, 7, DAY, 11, vec![row(11, "u:11", "d", t0())]).await;
    assert_eq!(run::cycle(&c, closed_now(), true, false, &all).await.unwrap().compactions, 0);
    // … until late files pile up; then the day is finalized again.
    for plan in 12..=20u64 {
        add_l0(&store, 7, DAY, plan, vec![row(plan, &format!("u:{plan}"), "d", t0())]).await;
    }
    assert_eq!(run::cycle(&c, closed_now(), true, false, &all).await.unwrap().compactions, 1);
    let m = day(&store, 7, DAY).await;
    assert_eq!((m.files.len(), m.files[0].level, m.files[0].rows), (1, 2, 20));
    assert_eq!(m.plans.get("1-0"), Some(&20));
    // Projects of other shards are not touched.
    add_l0(&store, 8, DAY, 1, vec![row(1, "u:1", "d", t0())]).await;
    let r = run::cycle(&c, closed_now(), true, false, &|p| p == 7).await.unwrap();
    assert_eq!(r.projects, 1);
    assert_eq!(day(&store, 8, DAY).await.files[0].level, 0);
}

/// Many compactors and an appending shipper at the same time: nothing is
/// lost, nothing comes back, every listed file exists.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_appends_and_compactions() {
    let dir = tempfile::tempdir().unwrap();
    for store in [Store::memory(), Store::local(dir.path()).unwrap()] {
        add_l0(&store, 7, DAY, 1, vec![row(1, "u:0001", "d", t0())]).await;
        let done = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let appender = {
            let (store, done) = (store.clone(), done.clone());
            tokio::spawn(async move {
                for plan in 2..=40u64 {
                    let rows = (0..5).map(|i| row(plan * 100 + i, &format!("u:{:04}", (plan * 7 + i) % 50), "d", t0() + i as i64)).collect();
                    add_l0(&store, 7, DAY, plan, rows).await;
                    tokio::time::sleep(std::time::Duration::from_millis(2)).await;
                }
                done.store(true, std::sync::atomic::Ordering::SeqCst);
            })
        };
        let compactors: Vec<_> = (0..3)
            .map(|_| {
                let (store, done) = (store.clone(), done.clone());
                tokio::spawn(async move {
                    let c = ctx(&store);
                    let mut committed = 0;
                    loop {
                        // Read the flag first so one more pass runs after the last append.
                        let last_pass = done.load(std::sync::atomic::Ordering::SeqCst);
                        let m = day(&store, 7, DAY).await;
                        let inputs: Vec<_> = m.files.iter().filter(|f| f.level < 2).cloned().collect();
                        if inputs.len() >= 2 {
                            let e = Arc::new(ProjectEnv::load(&store, 7).await.unwrap());
                            if let Outcome::Committed { .. } = job::compact(&c, &e, DAY, Group { inputs, level: 1 }).await.unwrap() {
                                committed += 1;
                            }
                        }
                        if last_pass {
                            return committed;
                        }
                        tokio::time::sleep(std::time::Duration::from_millis(1)).await;
                    }
                })
            })
            .collect();
        appender.await.unwrap();
        let mut committed = 0;
        for t in compactors {
            committed += t.await.unwrap();
        }
        assert!(committed > 0, "{}: no compaction ever won", store.describe());
        let m = day(&store, 7, DAY).await;
        assert_eq!(m.plans.get("1-0"), Some(&40));
        assert_eq!(m.total_rows(), 196, "{}: every row exactly once, physically", store.describe());
        let mut got = ids(&visible(&store, 7, DAY).await);
        got.sort_unstable();
        let mut expect: Vec<u64> = (2..=40u64).flat_map(|p| (0..5).map(move |i| p * 100 + i)).collect();
        expect.push(1);
        expect.sort_unstable();
        assert_eq!(got, expect, "{}", store.describe());
        assert_manifest_consistent(&store, 7, DAY).await;
        // Versions are gapless.
        let versions = store.list(&keys::data_manifest_dir(7, DAY)).await.unwrap();
        assert_eq!(versions.len() as u64, m.version);
    }
}
