//! Failure injection (design §17.5): kill the shipper at every step boundary
//! of a plan, restart, and require the same final content as an
//! uninterrupted run.

mod common;

use common::*;
use la_codec::wire::Value;
use la_compactor::job::{self, Group, Outcome, ProjectEnv};
use la_core::manifest;
use la_core::time::day_start_ms;
use la_core::{keys, Store};
use la_shipper::exec::Step;
use la_shipper::ship::ship_once;
use la_shipper::ShipError;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

/// Two projects, two event days each, identity links, person properties and
/// enough volume on one key to trigger a promotion in the first plan.
fn first_segments() -> Vec<(String, Vec<Entry>)> {
    let yesterday = t0() - 20 * HOUR_MS;
    let mut a = Vec::new();
    for b in 0..11u64 {
        let anon = format!("dev-{}", b % 4);
        let recs: Vec<_> = (0..100)
            .map(|i| {
                let id = b * 100 + i;
                let ts = if id % 7 == 0 { yesterday - id as i64 } else { t0() - 5000 - id as i64 };
                event(id, ts, "tap", vec![prop("plan", s("pro")), prop("n", Value::Int(id as i64))])
            })
            .collect();
        a.push(native(7, t0(), &header(&anon, "", 0), &recs));
    }
    a.push(native(
        7,
        t0(),
        &header("dev-1", "", 0),
        &[identify(5001, t0() - 100, "alice", vec![prop("tier", s("gold"))]), set_person(5002, t0() - 50, vec![prop("x", Value::Int(1))])],
    ));
    let b = vec![
        native(8, t0(), &header("other", "bob", 0), &[event(1, t0() - 1, "e", vec![]), event(2, yesterday, "e", vec![])]),
        posthog(8, t0(), r#"{"event":"$identify","distinct_id":"carol","properties":{"$anon_distinct_id":"c-anon","$set":{"k":"v"}}}"#),
        native(7, t0(), &header("dev-2", "", 0), &[identify(5003, t0() - 90, "dave", vec![])]),
    ];
    vec![(seg_key(10, "ing-01", 1), a), (seg_key(10, "ing-02", 1), b)]
}

fn second_segment() -> (String, Vec<Entry>) {
    let recs: Vec<_> =
        (0..50).map(|i| event(9000 + i, t0() - i as i64, "tap", vec![prop("plan", s("free")), prop("n", Value::Int(i as i64))])).collect();
    (seg_key(11, "ing-01", 2), vec![native(7, t0() + 1, &header("dev-3", "", 0), &recs)])
}

async fn seed(store: &Store) {
    for (key, entries) in first_segments() {
        put_segment(store, &key, &entries).await;
    }
}

async fn full_snapshot(store: &Store) -> (Snapshot, Snapshot, Vec<String>) {
    let mut markers: Vec<String> =
        store.list("wal/_done").await.unwrap().into_iter().map(|o| o.key).collect();
    markers.sort();
    (snapshot(store, 7).await, snapshot(store, 8).await, markers)
}

#[tokio::test]
async fn crash_at_every_step_boundary_then_restart() {
    // Reference run, recording every step boundary on the way.
    let reference = Store::memory();
    seed(&reference).await;
    let steps: Arc<Mutex<Vec<Step>>> = Arc::default();
    let mut c = ctx(&reference);
    let rec = steps.clone();
    c.fail = Some(Arc::new(move |s: &Step| {
        rec.lock().unwrap().push(s.clone());
        false
    }));
    let out = ship_once(&c).await.unwrap();
    assert_eq!(out.len(), 1);
    assert_eq!(out[0].promoted, [(7, "n".to_owned()), (7, "plan".to_owned())]);
    let after_plan_1 = full_snapshot(&reference).await;
    let (key, entries) = second_segment();
    put_segment(&reference, &key, &entries).await;
    ship_once(&ctx(&reference)).await.unwrap();
    let after_plan_2 = full_snapshot(&reference).await;

    let steps = steps.lock().unwrap().clone();
    // identity+persons+schema per project, an L0 and a manifest per (project, day), a marker per segment
    let count = |f: fn(&Step) -> bool| steps.iter().filter(|s| f(s)).count();
    assert_eq!(count(|s| matches!(s, Step::IdentityDelta { .. })), 2);
    assert_eq!(count(|s| matches!(s, Step::PersonsDelta { .. })), 2);
    assert_eq!(count(|s| matches!(s, Step::L0Written { .. })), 4);
    assert_eq!(count(|s| matches!(s, Step::ManifestCommitted { .. })), 4);
    assert_eq!(count(|s| matches!(s, Step::SchemaSaved { .. })), 2);
    assert_eq!(count(|s| matches!(s, Step::DoneMarker { .. })), 2);
    assert_eq!(steps.len(), 16);
    assert_eq!(after_plan_1.0.rows.len(), 2, "two event days");
    assert_eq!(total_rows(&after_plan_1.0), 1100);
    assert_eq!(after_plan_1.0.identity, [("dev-1".to_owned(), "alice".to_owned()), ("dev-2".to_owned(), "dave".to_owned())]);

    for (i, step) in steps.iter().enumerate() {
        let store = Store::memory();
        seed(&store).await;
        let seen = Arc::new(AtomicUsize::new(0));
        let mut crashing = ctx(&store);
        let counter = seen.clone();
        crashing.fail = Some(Arc::new(move |_: &Step| counter.fetch_add(1, Ordering::SeqCst) == i));
        match ship_once(&crashing).await {
            Err(ShipError::Injected(s)) => assert_eq!(&s, step),
            other => panic!("step {i} ({step:?}): expected an injected failure, got {other:?}"),
        }
        // "Restart": a fresh process finds the unfinished plan and re-executes it.
        let out = ship_once(&ctx(&store)).await.unwrap();
        if i + 1 == steps.len() {
            // The crash came after the very last marker: the plan was complete.
            assert!(out.is_empty());
        } else {
            assert_eq!(out.len(), 1, "step {i} ({step:?}): exactly the interrupted plan is re-run");
            assert_eq!(out[0].plan, 1);
        }
        assert_eq!(full_snapshot(&store).await, after_plan_1, "after a crash following {step:?}");
        assert_eq!(manifest::load_day(&store, 7, DAY).await.unwrap().plans.get("1-0"), Some(&1));
        assert_eq!(store.list("wal/_plan/1-0").await.unwrap().len(), 1, "no extra plan was cut");
        // Every day lists exactly one L0 for the plan — never two.
        for p in [7, 8] {
            for day in manifest::list_days(&store, p).await.unwrap() {
                assert_eq!(manifest::load_day(&store, p, &day).await.unwrap().files.len(), 1, "{p}/{day}");
            }
        }
        // The pipeline carries on normally afterwards.
        let (key, entries) = second_segment();
        put_segment(&store, &key, &entries).await;
        let out = ship_once(&ctx(&store)).await.unwrap();
        assert_eq!((out.len(), out[0].plan), (1, 2));
        assert_eq!(full_snapshot(&store).await, after_plan_2, "second plan after a crash following {step:?}");
    }
}

/// Crashing repeatedly — a different step on every attempt — still converges.
#[tokio::test]
async fn repeated_crashes_converge() {
    let reference = Store::memory();
    seed(&reference).await;
    ship_once(&ctx(&reference)).await.unwrap();
    let expect = full_snapshot(&reference).await;

    let store = Store::memory();
    seed(&store).await;
    let mut crashes = 0;
    for nth in [1usize, 4, 7, 10, 13] {
        let seen = Arc::new(AtomicUsize::new(0));
        let mut c = ctx(&store);
        let counter = seen.clone();
        c.fail = Some(Arc::new(move |_: &Step| counter.fetch_add(1, Ordering::SeqCst) == nth));
        if ship_once(&c).await.is_err() {
            crashes += 1;
        }
    }
    assert!(crashes >= 3, "only {crashes} crashes were injected");
    ship_once(&ctx(&store)).await.unwrap();
    assert_eq!(full_snapshot(&store).await, expect);
}

/// Two executions of the same plan at once (a standby that wrongly believes
/// the lease expired) produce the same result as one.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_execution_of_the_same_plan() {
    let reference = Store::memory();
    seed(&reference).await;
    ship_once(&ctx(&reference)).await.unwrap();
    let expect = full_snapshot(&reference).await;
    for _ in 0..5 {
        let store = Store::memory();
        seed(&store).await;
        let tasks: Vec<_> = (0..3)
            .map(|_| {
                let c = ctx(&store);
                tokio::spawn(async move { ship_once(&c).await })
            })
            .collect();
        for t in tasks {
            t.await.unwrap().unwrap();
        }
        assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
        assert_eq!(full_snapshot(&store).await, expect);
        for day in manifest::list_days(&store, 7).await.unwrap() {
            assert_eq!(manifest::load_day(&store, 7, &day).await.unwrap().files.len(), 1);
        }
    }
}

/// `plans[shardkey]` is what keeps a re-executed plan from adding its L0
/// again after compaction replaced (and GC removed) it.
#[tokio::test]
async fn reexecuting_a_plan_after_compaction_does_not_resurrect_data() {
    let store = Store::memory();
    seed(&store).await;
    ship_once(&ctx(&store)).await.unwrap();
    let before = full_snapshot(&store).await;

    // Compact the day's L0 into an L1 and remove the L0 object, as GC would.
    let cctx = la_compactor::Ctx::new(store.clone(), la_compactor::CompactorConfig::default());
    let env = Arc::new(ProjectEnv::load(&store, 7).await.unwrap());
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    let l0 = m.files[0].clone();
    assert_eq!(l0.level, 0);
    let outcome = job::compact(&cctx, &env, DAY, Group { inputs: vec![l0.clone()], level: 1 }).await.unwrap();
    assert!(matches!(outcome, Outcome::Committed { .. }));
    store.delete(&l0.key).await.unwrap();

    // The crash happened after the manifests but before the `_done` markers.
    for o in store.list("wal/_done").await.unwrap() {
        store.delete(&o.key).await.unwrap();
    }
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!((out.len(), out[0].plan, out[0].records_out, out[0].files), (1, 1, 0, 0));

    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert_eq!(m.files.len(), 1);
    assert_eq!(m.files[0].level, 1, "the L0 was not added again");
    assert!(!store.exists(&l0.key).await.unwrap(), "and its object was not rewritten");
    assert_eq!(full_snapshot(&store).await, before);
    // Sanity: the day really is the one the plan wrote to.
    assert!(day_start_ms(DAY).is_some() && keys::l0_file(7, DAY, "1-0", 1) == l0.key);
}
