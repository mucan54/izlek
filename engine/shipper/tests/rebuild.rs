//! `la-shipper rebuild`: re-deriving `data/` from `wal/` on a live bucket
//! (design §18.5).

mod common;

use common::*;
use la_compactor::{inspect, CompactorConfig, Ctx};
use la_core::events::EventRow;
use la_core::identity::IdentityMap;
use la_core::manifest;
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::{keys, Store};
use la_shipper::exec::Step;
use la_shipper::plan::{latest_plan_number, load_plan, Plan, RebuildSpec};
use la_shipper::rebuild::{rebuild, RebuildOptions};
use la_shipper::ship::ship_once;
use std::collections::BTreeMap;
use std::sync::Arc;

const YESTERDAY: &str = "2026-11-17";

fn cctx(store: &Store) -> Ctx {
    Ctx::new(store.clone(), CompactorConfig::default())
}

fn opts() -> RebuildOptions {
    RebuildOptions { from: YESTERDAY.into(), to: DAY.into(), ..Default::default() }
}

/// Rows per day after the stronger per-person uuid dedupe (what any rewrite produces).
async fn content(store: &Store, project: u32) -> BTreeMap<String, Vec<inspect::LogicalRow>> {
    let mut rows = snapshot(store, project).await.rows;
    rows.values_mut().for_each(inspect::dedupe_uuid_per_person);
    rows
}

async fn ship_fixture(store: &Store, plans: usize) -> Vec<Vec<Entry>> {
    let segments = random_segments(&mut Rng(11), plans);
    for (i, seg) in segments.iter().enumerate() {
        put_segment(store, &seg_key(10, "ing-01", i as u64 + 1), seg).await;
        ship_once(&ctx(store)).await.unwrap();
    }
    segments
}

async fn wipe(store: &Store, prefix: &str) {
    for o in store.list(prefix).await.unwrap() {
        store.delete(&o.key).await.unwrap();
    }
}

#[tokio::test]
async fn rebuild_restores_a_lost_data_prefix() {
    let store = Store::memory();
    ship_fixture(&store, 4).await;
    let before = (content(&store, 7).await, content(&store, 8).await);
    assert_eq!(before.0.len(), 2, "the fixture spans two event days");
    wipe(&store, "data").await;
    assert!(snapshot(&store, 7).await.rows.is_empty());
    // Without a rebuild the shipper would never look at those segments again.
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());

    let report = rebuild(&ctx(&store), &cctx(&store), &opts()).await.unwrap();
    assert_eq!(report.fenced, [("1-0".to_owned(), 5)]);
    assert_eq!(report.segments, 4);
    assert_eq!(report.days_rebuilt.len(), 4, "{report:?}");
    assert!(report.days_untouched.is_empty());
    assert_eq!((content(&store, 7).await, content(&store, 8).await), before);

    // Shape of a rebuilt day: L1 files, plans raised to the rebuild plan.
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert!(m.files.iter().all(|f| f.level == 1 && f.key.contains("/L1-")), "{:?}", m.files);
    assert_eq!(m.plans.get("1-0"), Some(&5));
    assert_eq!(m.files[0].identity_version, IdentityMap::load(&store, 7).await.unwrap().version());
    // The fence is lifted and recorded as an ordinary finished plan.
    let plan: Plan = load_plan(&store, "1-0", 5).await.unwrap().unwrap();
    assert_eq!(plan.rebuild.as_ref().map(|r| (r.from.as_str(), r.to.as_str())), Some((YESTERDAY, DAY)));
    assert!(store.exists(&keys::wal_rebuild_done("1-0", 5)).await.unwrap());

    // Shipping carries on with the next plan number; nothing old is resurrected.
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
    let extra = vec![native(7, t0() + 5, &header("dev-late", "", 0), &[event(900_001, t0(), "tap", vec![])])];
    put_segment(&store, &seg_key(12, "ing-01", 99), &extra).await;
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!((out.len(), out[0].plan, out[0].records_out), (1, 6, 1));
    assert_eq!(content(&store, 7).await[DAY].len(), before.0[DAY].len() + 1);
    // Running the same rebuild again is harmless.
    let again = rebuild(&ctx(&store), &cctx(&store), &opts()).await.unwrap();
    assert_eq!(again.fenced, [("1-0".to_owned(), 7)]);
    assert_eq!(content(&store, 8).await, before.1);
    assert_eq!(content(&store, 7).await[DAY].len(), before.0[DAY].len() + 1);
}

/// After a faulty shipper version: wrong files are replaced, segments that
/// were never shipped are left to the shipper, concurrent state is respected.
#[tokio::test]
async fn rebuild_replaces_existing_files_and_leaves_pending_segments_alone() {
    let store = Store::memory();
    ship_fixture(&store, 3).await;
    let good = content(&store, 7).await;

    // Damage: a bogus file in the day's manifest (as a buggy shipper might have produced).
    let bogus: Vec<EventRow> = (0..50u64)
        .map(|i| EventRow {
            uuid: uuid(5_000_000 + i),
            ts: t0(),
            recv_ts: t0(),
            event: "bogus".into(),
            person_key: format!("u:bogus-{i:02}"),
            anon_id: "x".into(),
            props: "{}".into(),
            ..Default::default()
        })
        .collect();
    let meta = EventsFileMeta { project: 7, day: DAY.into(), level: 2, schema_version: 0, identity_version: 0 };
    let (bytes, mut entry) = write_events_file(&bogus, &[], &meta).unwrap();
    entry.key = keys::level_file(7, DAY, 2, 99, 0);
    store.put(&entry.key, bytes).await.unwrap();
    manifest::commit_day(&store, 7, DAY, |m| {
        m.files.push(entry.clone());
        Ok(true)
    })
    .await
    .unwrap();
    assert_eq!(content(&store, 7).await[DAY].len(), good[DAY].len() + 50);

    // A segment that arrived but was not shipped yet.
    let pending = vec![native(7, t0() + 5, &header("dev-pending", "", 0), &[event(900_002, t0(), "tap", vec![])])];
    put_segment(&store, &seg_key(12, "ing-01", 50), &pending).await;

    let report = rebuild(&ctx(&store), &cctx(&store), &RebuildOptions { project: Some(7), ..opts() }).await.unwrap();
    assert_eq!(report.segments, 3, "the pending segment is not replayed");
    assert_eq!(
        content(&store, 7).await,
        good,
        "bogus rows are gone, shipped rows are back, pending rows not yet there"
    );
    assert!(store.exists(&entry.key).await.unwrap(), "replaced files are left to the garbage collector");

    // The shipper then handles the pending segment as usual.
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!((out.len(), out[0].records_out), (1, 1));
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert!(m.files.iter().any(|f| f.level == 0) && m.files.iter().any(|f| f.level == 1));
    assert_eq!(content(&store, 7).await[DAY].len(), good[DAY].len() + 1);
}

#[tokio::test]
async fn an_unfinished_rebuild_fences_shipping_until_resumed_or_aborted() {
    let store = Store::memory();
    ship_fixture(&store, 2).await;
    let good = content(&store, 7).await;
    // A rebuild that died right after fencing.
    let fence = Plan {
        n: 3,
        shardkey: "1-0".into(),
        created_at: now(),
        holder: "rebuild-x".into(),
        segments: vec![],
        rebuild: Some(RebuildSpec { id: "r1".into(), from: YESTERDAY.into(), to: DAY.into() }),
    };
    assert!(store.put_json_if_absent(&keys::wal_plan("1-0", 3), &fence).await.unwrap());
    let extra = vec![native(7, t0() + 5, &header("dev-late", "", 0), &[event(900_003, t0(), "tap", vec![])])];
    put_segment(&store, &seg_key(12, "ing-01", 77), &extra).await;

    // The shipper waits: no plan is cut, nothing is marked done.
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
    assert_eq!(latest_plan_number(&store, "1-0").await.unwrap(), 3);
    assert!(!store.exists("wal/_done/1-0/ing-01-000000000077.ok").await.unwrap());

    // A different rebuild refuses to start on top of it …
    let other = RebuildOptions { from: DAY.into(), to: DAY.into(), ..Default::default() };
    let err = rebuild(&ctx(&store), &cctx(&store), &other).await.unwrap_err().to_string();
    assert!(err.contains("in progress") && err.contains("--abort"), "{err}");
    // … the same one resumes under the existing fence …
    let report = rebuild(&ctx(&store), &cctx(&store), &opts()).await.unwrap();
    assert_eq!(report.fenced, [("1-0".to_owned(), 3)]);
    assert_eq!(content(&store, 7).await, good);
    // … and shipping continues afterwards.
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!((out.len(), out[0].plan), (1, 4));

    // --abort only lifts the fence.
    let fence2 = Plan { n: 5, ..fence.clone() };
    store.put_json_if_absent(&keys::wal_plan("1-0", 5), &fence2).await.unwrap();
    let extra2 = vec![native(7, t0() + 6, &header("dev-late", "", 0), &[event(900_004, t0(), "tap", vec![])])];
    put_segment(&store, &seg_key(12, "ing-01", 78), &extra2).await;
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
    let versions = manifest::load_day(&store, 7, DAY).await.unwrap().version;
    let aborted =
        rebuild(&ctx(&store), &cctx(&store), &RebuildOptions { abort: true, ..Default::default() }).await.unwrap();
    assert!(aborted.aborted);
    assert_eq!(aborted.fenced, [("1-0".to_owned(), 5)]);
    assert_eq!(manifest::load_day(&store, 7, DAY).await.unwrap().version, versions, "abort rewrites nothing");
    assert_eq!(ship_once(&ctx(&store)).await.unwrap().len(), 1);
    assert_eq!(content(&store, 7).await[DAY].len(), good[DAY].len() + 2);
}

/// A shipper that crashed mid-plan is finished by the rebuild before fencing.
#[tokio::test]
async fn rebuild_first_finishes_an_interrupted_plan() {
    let reference = Store::memory();
    ship_fixture(&reference, 3).await;
    let expect = content(&reference, 7).await;

    let store = Store::memory();
    let segments = random_segments(&mut Rng(11), 3);
    for (i, seg) in segments.iter().enumerate() {
        put_segment(&store, &seg_key(10, "ing-01", i as u64 + 1), seg).await;
        let mut c = ctx(&store);
        if i == 2 {
            // Third plan: die right after the first L0 was uploaded.
            c.fail = Some(Arc::new(|s: &Step| matches!(s, Step::L0Written { .. })));
            assert!(ship_once(&c).await.is_err());
        } else {
            ship_once(&c).await.unwrap();
        }
    }
    let report = rebuild(&ctx(&store), &cctx(&store), &opts()).await.unwrap();
    assert_eq!(report.fenced, [("1-0".to_owned(), 4)], "plan 3 was completed, the fence is plan 4");
    assert_eq!(report.segments, 3);
    assert_eq!(content(&store, 7).await, expect);
}

#[tokio::test]
async fn rebuild_can_restore_identity_and_persons_too() {
    let store = Store::memory();
    let entries = vec![
        native(
            7,
            t0(),
            &header("dev-1", "", 0),
            &[
                event(1, t0() - 300, "open", vec![]),
                identify(2, t0() - 200, "alice", vec![prop("plan", s("pro"))]),
                set_person(3, t0() - 100, vec![prop("theme", s("dark"))]),
            ],
        ),
        native(7, t0(), &header("dev-2", "", 0), &[event(4, t0() - 50, "open", vec![])]),
    ];
    put_segment(&store, &seg_key(10, "ing-01", 1), &entries).await;
    ship_once(&ctx(&store)).await.unwrap();
    let before = snapshot(&store, 7).await;
    assert_eq!(before.identity, [("dev-1".to_owned(), "alice".to_owned())]);

    for prefix in ["data", "identity", "persons"] {
        wipe(&store, prefix).await;
    }
    // Default: only data/ is rebuilt; without the identity map the rows stay anonymous.
    rebuild(&ctx(&store), &cctx(&store), &RebuildOptions { from: DAY.into(), to: DAY.into(), ..Default::default() })
        .await
        .unwrap();
    let plain = snapshot(&store, 7).await;
    assert!(plain.identity.is_empty() && plain.persons.is_empty());
    assert_eq!(plain.rows[DAY].iter().map(|r| r.person_key.as_str()).collect::<Vec<_>>(), ["a:dev-1", "a:dev-2"]);

    let o = RebuildOptions { from: DAY.into(), to: DAY.into(), with_side_tables: true, ..Default::default() };
    rebuild(&ctx(&store), &cctx(&store), &o).await.unwrap();
    let restored = snapshot(&store, 7).await;
    assert_eq!(restored.rows, before.rows);
    assert_eq!(restored.identity, before.identity);
    assert_eq!(restored.persons, before.persons);
}

#[tokio::test]
async fn rebuild_validates_its_range() {
    let store = Store::memory();
    ship_fixture(&store, 1).await;
    let run = |o: RebuildOptions| {
        let store = store.clone();
        async move { rebuild(&ctx(&store), &cctx(&store), &o).await }
    };
    for (from, to) in [("", ""), ("2026-11-18", "2026-11-17"), ("18.11.2026", "2026-11-18")] {
        let err = run(RebuildOptions { from: from.into(), to: to.into(), ..Default::default() }).await.unwrap_err();
        assert!(err.to_string().contains("--from"), "{err}");
    }
    // Older than the WAL retention: the WAL for those days may be incomplete.
    let old = RebuildOptions { from: "2026-09-01".into(), to: DAY.into(), ..Default::default() };
    let err = run(old.clone()).await.unwrap_err().to_string();
    assert!(err.contains("retention horizon") && err.contains("--force"), "{err}");
    assert_eq!(latest_plan_number(&store, "1-0").await.unwrap(), 1, "a refused rebuild fences nothing");
    let before = content(&store, 7).await;
    let report = run(RebuildOptions { force: true, ..old }).await.unwrap();
    assert!(!report.days_rebuilt.is_empty());
    assert_eq!(content(&store, 7).await, before);
    // A day in range that has data but no WAL rows is left untouched and reported.
    let lonely = EventRow {
        uuid: uuid(1),
        event: "e".into(),
        person_key: "u:x".into(),
        anon_id: "x".into(),
        props: "{}".into(),
        ..Default::default()
    };
    let meta =
        EventsFileMeta { project: 7, day: "2026-11-01".into(), level: 2, schema_version: 0, identity_version: 0 };
    let (bytes, mut entry) = write_events_file(&[lonely], &[], &meta).unwrap();
    entry.key = keys::level_file(7, "2026-11-01", 2, 1, 0);
    store.put(&entry.key, bytes).await.unwrap();
    manifest::commit_day(&store, 7, "2026-11-01", |m| {
        m.files.push(entry.clone());
        Ok(true)
    })
    .await
    .unwrap();
    let report = run(RebuildOptions { from: "2026-11-01".into(), to: DAY.into(), ..Default::default() }).await.unwrap();
    assert_eq!(report.days_untouched, [(7, "2026-11-01".to_owned())]);
    assert_eq!(manifest::load_day(&store, 7, "2026-11-01").await.unwrap().files.len(), 1);
}
