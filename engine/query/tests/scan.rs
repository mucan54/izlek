//! Scan-layer behaviour: schema evolution, identity rewrite (incl. ordering),
//! dedupe across files, tombstones without a rewrite.

mod common;

use arrow::array::{Array, AsArray, RecordBatch};
use arrow::datatypes::{Float64Type, TimestampMillisecondType};
use common::*;
use datafusion::physical_plan::ExecutionPlan;
use datafusion::prelude::ident;
use la_core::events::Cell;
use la_core::schema::PropType;
use la_core::Store;
use la_query::filecache::QueryStats;
use la_query::filter::{Filter, Ref};
use la_query::scan::ScanRequest;
use la_query::session::{merged_stream, QueryCx};
use la_query::state::Engine;
use std::sync::Arc;

const P: u32 = 7;

async fn cx(engine: &Arc<Engine>, from: &str, to: &str) -> QueryCx {
    let (f, t) = la_query::state::parse_range(from, to).unwrap();
    let snap = Arc::new(engine.snapshot(P, f, t, true).await.unwrap());
    QueryCx::new(engine, snap, QueryStats::new(0)).unwrap()
}

async fn collect(cx: &QueryCx, req: &ScanRequest) -> Vec<RecordBatch> {
    let plan = cx.plan(req).unwrap();
    let plan: Arc<dyn ExecutionPlan> = plan;
    let stream = if req.ordered {
        merged_stream(plan, &["person_key", "ts"], cx.task_ctx()).unwrap()
    } else {
        Arc::new(datafusion::physical_plan::coalesce_partitions::CoalescePartitionsExec::new(plan))
            .execute(0, cx.task_ctx())
            .unwrap()
    };
    datafusion::physical_plan::common::collect(stream).await.unwrap()
}

fn cols(names: &[&str]) -> Vec<(datafusion::prelude::Expr, String)> {
    names.iter().map(|n| (ident(*n), n.to_string())).collect()
}

/// `(person_key, ts - day start, event)` triples in stream order.
fn triples(batches: &[RecordBatch], base: i64) -> Vec<(String, i64, String)> {
    let mut out = Vec::new();
    for b in batches {
        let pk = b.column_by_name("person_key").unwrap().as_string::<i32>();
        let ts = b.column_by_name("ts").unwrap().as_primitive::<TimestampMillisecondType>();
        let ev = b.column_by_name("event").unwrap().as_string::<i32>();
        for i in 0..b.num_rows() {
            out.push((pk.value(i).to_owned(), ts.value(i) - base, ev.value(i).to_owned()));
        }
    }
    out
}

fn t(pk: &str, ts: i64, ev: &str) -> (String, i64, String) {
    (pk.to_owned(), ts, ev.to_owned())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn single_file_day_is_read_in_file_order() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "a").level(2),
        vec![ev("x", Some("2"), d + 30, "b"), ev("x", Some("1"), d + 20, "a"), ev("y", None, d + 10, "c")],
    )
    .await;
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY1).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: true };
    let got = triples(&collect(&cx, &req).await, d);
    assert_eq!(got, [t("a:y", 10, "c"), t("u:1", 20, "a"), t("u:2", 30, "b")]);
    // Only the projected columns are decoded: far fewer bytes than the file.
    assert!(cx.stats.scanned() > 0);
    assert_eq!(cx.stats.files.load(std::sync::atomic::Ordering::Relaxed), 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn time_range_and_filters_apply_per_row() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "a"),
        vec![
            ev("x", Some("1"), d + 1000, "a").platform("ios"),
            ev("x", Some("1"), d + 2000, "b").platform("web"),
            ev("x", Some("1"), d + 3000, "a").platform("web"),
        ],
    )
    .await;
    add_file(&store, FileSpec::l0(P, DAY2, "a"), vec![ev("x", Some("1"), d + 86_400_000, "a")]).await;
    let engine = engine(&store).await;
    // RFC 3339 `to` is exclusive: [d+1000, d+3000)
    let cx = cx(&engine, "2026-11-01T00:00:01Z", "2026-11-01T00:00:03Z").await;
    let f = Filter::parse(&serde_json::json!({"prop": "platform", "op": "eq", "value": "web"})).unwrap();
    let req = ScanRequest {
        exprs: cols(&["person_key", "ts", "event"]),
        filters: vec![cx.compiler.compile(&f)],
        ordered: false,
    };
    assert_eq!(triples(&collect(&cx, &req).await, d), [t("u:1", 2000, "b")]);
    let all = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: false };
    assert_eq!(triples(&collect(&cx, &all).await, d).len(), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn duplicate_uuids_across_files_are_collapsed() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    let a = ev("x", Some("1"), d + 10, "a").uuid(900);
    let b = ev("x", Some("1"), d + 10, "b").uuid(901);
    let c = ev("y", Some("2"), d + 5, "c").uuid(902);
    add_file(&store, FileSpec::l0(P, DAY1, "f1"), vec![a.clone(), b.clone()]).await;
    add_file(&store, FileSpec::l0(P, DAY1, "f2"), vec![a.clone(), c.clone()]).await;
    add_file(&store, FileSpec::l0(P, DAY1, "f3"), vec![b.clone(), c.clone(), a.clone()]).await;
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY1).await;
    for ordered in [true, false] {
        let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered };
        let got = triples(&collect(&cx, &req).await, d);
        assert_eq!(got, [t("u:1", 10, "a"), t("u:1", 10, "b"), t("u:2", 5, "c")], "ordered={ordered}");
    }
    // Dedupe must not depend on which columns were requested.
    let req = ScanRequest { exprs: cols(&["event"]), filters: vec![], ordered: false };
    let n: usize = collect(&cx, &req).await.iter().map(|b| b.num_rows()).sum();
    assert_eq!(n, 3);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn identity_links_newer_than_a_file_rewrite_and_resort_its_rows() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    // File written before any identify: everything anonymous.
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "old").identity(0),
        vec![
            ev("anonA", None, d + 10, "view"),
            ev("anonA", None, d + 30, "click"),
            ev("anonB", None, d + 20, "view"),
            ev("anonZ", None, d + 5, "view"),
            ev("dev", Some("alice"), d + 1, "login"),
        ],
    )
    .await;
    // anonA → alice, anonB → bob arrive afterwards.
    let v = link(&store, P, 1, &[("anonA", "alice"), ("anonB", "bob")]).await;
    assert_eq!(v, 1);
    // A later file already written with the link applied (and a duplicate of
    // an old row, now carrying the resolved key).
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "new").identity(1),
        vec![
            ev("anonA", None, d + 40, "buy").key("u:alice"),
            ev("anonA", None, d + 30, "click").key("u:alice").uuid(7777),
        ],
    )
    .await;
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY1).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: true };
    let got = triples(&collect(&cx, &req).await, d);
    assert_eq!(
        got,
        [
            t("a:anonZ", 5, "view"),
            t("u:alice", 1, "login"),
            t("u:alice", 10, "view"),
            t("u:alice", 30, "click"),
            t("u:alice", 30, "click"),
            t("u:alice", 40, "buy"),
            t("u:bob", 20, "view"),
        ]
    );
    // A person_key filter sees the rewritten key, including for rows that
    // were anonymous on disk.
    let f = Filter::parse(&serde_json::json!({"prop": "person_key", "op": "eq", "value": "u:alice"})).unwrap();
    let req = ScanRequest {
        exprs: cols(&["person_key", "ts", "event"]),
        filters: vec![cx.compiler.compile(&f)],
        ordered: true,
    };
    assert_eq!(collect(&cx, &req).await.iter().map(|b| b.num_rows()).sum::<usize>(), 5);
    let f = Filter::parse(&serde_json::json!({"prop": "person_key", "op": "eq", "value": "a:anonA"})).unwrap();
    let req = ScanRequest { exprs: cols(&["event"]), filters: vec![cx.compiler.compile(&f)], ordered: false };
    assert_eq!(collect(&cx, &req).await.iter().map(|b| b.num_rows()).sum::<usize>(), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn rewritten_duplicate_of_a_resolved_row_is_deduped() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    let original = ev("anonA", None, d + 30, "click").uuid(4242);
    add_file(&store, FileSpec::l0(P, DAY1, "old").identity(0), vec![original.clone()]).await;
    link(&store, P, 1, &[("anonA", "alice")]).await;
    add_file(&store, FileSpec::l0(P, DAY1, "new").identity(1), vec![original.key("u:alice")]).await;
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY1).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: true };
    assert_eq!(triples(&collect(&cx, &req).await, d), [t("u:alice", 30, "click")]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn schema_evolution_unifies_files() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    let promoted = [col("amount", PropType::Float, 2), col("plan", PropType::String, 2)];
    // Before promotion: values live in props.
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "old"),
        vec![
            ev("a", Some("1"), d + 1, "buy").props(r#"{"amount":10,"plan":"pro"}"#),
            ev("a", Some("1"), d + 2, "buy").props(r#"{"amount":"oops"}"#),
        ],
    )
    .await;
    // After promotion: typed columns; a mismatching value stays in props.
    add_file(
        &store,
        FileSpec::l0(P, DAY2, "new").schema(2, &promoted),
        vec![
            ev("a", Some("1"), d + 86_400_001, "buy")
                .promoted(vec![Some(Cell::Float(2.5)), Some(Cell::Str("team".into()))]),
            ev("a", Some("1"), d + 86_400_002, "buy").promoted(vec![None, None]).props(r#"{"amount":"7","plan":3}"#),
        ],
    )
    .await;
    save_schema(&store, P, 2, &promoted).await;
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY2).await;
    let amount = cx.compiler.number(&Ref::parse("props.amount").unwrap());
    let plan = cx.compiler.text(&Ref::parse("props.plan").unwrap());
    let req = ScanRequest {
        exprs: vec![
            (ident("person_key"), "person_key".into()),
            (ident("ts"), "ts".into()),
            (amount, "amount".into()),
            (plan, "plan".into()),
            (ident("p_amount"), "p_amount".into()),
        ],
        filters: vec![],
        ordered: true,
    };
    let batches = collect(&cx, &req).await;
    let mut rows = Vec::new();
    for b in &batches {
        let a = b.column_by_name("amount").unwrap().as_primitive::<Float64Type>();
        let p = b.column_by_name("plan").unwrap().as_string::<i32>();
        let raw = b.column_by_name("p_amount").unwrap().as_primitive::<Float64Type>();
        for i in 0..b.num_rows() {
            rows.push((
                (!a.is_null(i)).then(|| a.value(i)),
                (!p.is_null(i)).then(|| p.value(i).to_owned()),
                (!raw.is_null(i)).then(|| raw.value(i)),
            ));
        }
    }
    assert_eq!(
        rows,
        [
            (Some(10.0), Some("pro".to_owned()), None),
            (None, None, None),
            (Some(2.5), Some("team".to_owned()), Some(2.5)),
            (Some(7.0), Some("3".to_owned()), None),
        ]
    );
    // Filters use the same logical view.
    let f = Filter::parse(&serde_json::json!({"prop": "props.amount", "op": "gte", "value": 2.5})).unwrap();
    let req = ScanRequest { exprs: cols(&["ts"]), filters: vec![cx.compiler.compile(&f)], ordered: false };
    assert_eq!(collect(&cx, &req).await.iter().map(|b| b.num_rows()).sum::<usize>(), 3);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tombstones_hide_rows_without_any_rewrite() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    add_file(
        &store,
        FileSpec::l0(P, DAY1, "a").level(2),
        vec![
            ev("devA", Some("alice"), d + 1, "x"),
            ev("devA", None, d + 2, "pre-identify"),
            ev("devB", Some("bob"), d + 3, "x"),
            ev("devC", None, d + 4, "x"),
            ev("shared", Some("alice"), d + 5, "x").key("u:other"),
        ],
    )
    .await;
    link(&store, P, 1, &[("devA", "alice")]).await;
    let engine = engine(&store).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: true };
    let before = triples(&collect(&cx(&engine, DAY1, DAY1).await, &req).await, d);
    assert_eq!(before.len(), 5);

    // Delete alice: her identified rows, her linked anonymous rows and rows
    // carrying her user_id under another key all disappear at once.
    tombstone(&store, P, 1_000, &["u:alice"], &["devC"]).await;
    let files_before = store.list("data").await.unwrap();
    let after = triples(&collect(&cx(&engine, DAY1, DAY1).await, &req).await, d);
    assert_eq!(after, [t("u:bob", 3, "x")]);
    assert_eq!(store.list("data").await.unwrap(), files_before, "no data file was touched");

    // Once the compactor has applied the tombstone the manifest says so and
    // the filter is no longer needed (rows are physically gone).
    replace_day(&store, FileSpec::l0(P, DAY1, "c").level(2), vec![ev("devB", Some("bob"), d + 3, "x")], 1_000).await;
    let cx2 = cx(&engine, DAY1, DAY1).await;
    assert_eq!(triples(&collect(&cx2, &req).await, d), [t("u:bob", 3, "x")]);
    // A newer tombstone still applies to the compacted file.
    tombstone(&store, P, 2_000, &["u:bob"], &[]).await;
    assert!(triples(&collect(&cx(&engine, DAY1, DAY1).await, &req).await, d).is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn declared_ordering_lets_the_planner_drop_sorts() {
    use datafusion::physical_plan::displayable;
    let store = Store::memory();
    let d = day_ms(DAY1);
    for (i, day) in [DAY1, DAY2, DAY3].iter().enumerate() {
        let base = d + i as i64 * 86_400_000;
        add_file(
            &store,
            FileSpec::l0(P, day, "a").level(2),
            vec![ev("x", Some("2"), base + 30, "b"), ev("x", Some("1"), base + 20, "a"), ev("y", None, base + 10, "c")],
        )
        .await;
    }
    let engine = engine(&store).await;
    let cx = cx(&engine, DAY1, DAY3).await;
    let table = Arc::new(la_query::scan::EventsTable::new(cx.scan.clone()));
    let ordered = table.ordered_flag();
    cx.ctx.register_table("events", table).unwrap();

    let plan_of = |sql: &'static str| {
        let ctx = cx.ctx.clone();
        async move {
            let plan = ctx.sql(sql).await.unwrap().create_physical_plan().await.unwrap();
            let text = displayable(plan.as_ref()).indent(false).to_string();
            text
        }
    };
    // With the file order kept, ORDER BY the sort key needs only a merge of the day partitions…
    ordered.store(true, std::sync::atomic::Ordering::Relaxed);
    let text = plan_of("SELECT person_key, ts, uuid FROM events ORDER BY person_key, ts, uuid").await;
    assert!(text.contains("SortPreservingMergeExec"), "{text}");
    assert!(!text.contains("SortExec"), "redundant sort was not removed:\n{text}");
    assert!(text.contains("EventsScanExec: days=3, files=3, partitions=3, ordered=true"), "{text}");
    // …a prefix of the key works too, and so does a per-person window.
    let text = plan_of("SELECT person_key, ts FROM events ORDER BY person_key, ts").await;
    assert!(!text.contains("SortExec"), "{text}");
    let text =
        plan_of("SELECT person_key, row_number() OVER (PARTITION BY person_key ORDER BY ts) FROM events").await;
    assert!(!text.contains("SortExec"), "{text}");
    // An order the files do not have still sorts.
    let text = plan_of("SELECT person_key, ts FROM events ORDER BY ts").await;
    assert!(text.contains("SortExec"), "{text}");
    // Without the flag nothing is promised (days are chained into fewer partitions).
    ordered.store(false, std::sync::atomic::Ordering::Relaxed);
    let text = plan_of("SELECT person_key, ts FROM events ORDER BY person_key, ts").await;
    assert!(text.contains("SortExec") && text.contains("partitions=2, ordered=false"), "{text}");

    // And the merged result really is in order.
    ordered.store(true, std::sync::atomic::Ordering::Relaxed);
    let batches = cx.ctx.sql("SELECT person_key, ts, event FROM events ORDER BY person_key, ts").await.unwrap().collect().await.unwrap();
    let got = triples(&batches, d);
    let mut sorted = got.clone();
    sorted.sort();
    assert_eq!(got.len(), 9);
    assert_eq!(got, sorted);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn disk_tier_serves_repeat_reads_and_plan_summary_hides_literals() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    let key = add_file(
        &store,
        FileSpec::l0(P, DAY1, "a").level(2),
        vec![ev("x", Some("1"), d + 20, "a").props(r#"{"k":"s3cr3t-literal"}"#), ev("y", None, d + 10, "c")],
    )
    .await;
    let dir = tempfile::tempdir().unwrap();
    let cfg = la_query::config::Config { cache_dir: Some(dir.path().into()), cache_bytes: 1 << 20, ..test_config() };
    let engine = engine_with(&store, cfg).await;
    let f = Filter::parse(&serde_json::json!({"prop": "props.k", "op": "eq", "value": "s3cr3t-literal"})).unwrap();
    let cx1 = cx(&engine, DAY1, DAY1).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![cx1.compiler.compile(&f)], ordered: true };
    assert_eq!(triples(&collect(&cx1, &req).await, d), [t("u:1", 20, "a")]);
    assert!(engine.files.counters.remote_bytes.load(std::sync::atomic::Ordering::Relaxed) > 0);

    // The slow-query log only ever sees operator names.
    let summary = cx1.plan_summaries().join(" | ");
    assert!(summary.contains("EventsScanExec[") && summary.contains("DataSourceExec×1"), "{summary}");
    assert!(!summary.contains("s3cr3t") && !summary.contains("props"), "{summary}");

    // Once the background fill has landed, the object store is no longer needed for this file.
    for _ in 0..400 {
        if engine.files.disk_files() == 1 {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(5)).await;
    }
    assert_eq!(engine.files.disk_files(), 1);
    store.delete(&key).await.unwrap();
    let remote_before = engine.files.counters.remote_bytes.load(std::sync::atomic::Ordering::Relaxed);
    let cx2 = cx(&engine, DAY1, DAY1).await;
    assert_eq!(triples(&collect(&cx2, &req).await, d), [t("u:1", 20, "a")]);
    assert_eq!(engine.files.counters.remote_bytes.load(std::sync::atomic::Ordering::Relaxed), remote_before);
    assert!(engine.files.counters.footer_hits.load(std::sync::atomic::Ordering::Relaxed) > 0);
    assert!(cx2.stats.scanned() > 0, "bytes served from the cache still count as scanned");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn rewritten_l0_with_a_stale_manifest_size_is_still_readable() {
    // A re-executed shipper plan may overwrite an L0 file under the same name
    // while the manifest keeps the old entry (spec/manifest.md §2).
    let store = Store::memory();
    let d = day_ms(DAY1);
    let key = add_file(&store, FileSpec::l0(P, DAY1, "p1"), vec![ev("x", Some("1"), d + 1, "a")]).await;
    let engine = engine(&store).await;
    let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![], ordered: true };
    assert_eq!(triples(&collect(&cx(&engine, DAY1, DAY1).await, &req).await, d).len(), 1);
    // Same key, different bytes and size; the manifest is not touched.
    let other = Store::memory();
    let rewritten = add_file(
        &other,
        FileSpec::l0(P, DAY1, "p1"),
        (0..50).map(|i| ev("x", Some("1"), d + 1 + i, "a")).collect(),
    )
    .await;
    assert_eq!(rewritten, key);
    store.put(&key, other.get(&key).await.unwrap().unwrap()).await.unwrap();
    // A fresh engine (no cached footer) must cope with the wrong size in the manifest.
    let engine = engine_with(&store, test_config()).await;
    assert_eq!(triples(&collect(&cx(&engine, DAY1, DAY1).await, &req).await, d).len(), 50);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn files_outside_a_person_filter_are_not_opened() {
    // A big day split into two L2 files by person range (design §10.1).
    let store = Store::memory();
    let d = day_ms(DAY1);
    add_file(&store, FileSpec::l0(P, DAY1, "00000001-000").level(2), vec![ev("a1", Some("alice"), d + 1, "x"), ev("a2", Some("bob"), d + 2, "x")])
        .await;
    add_file(&store, FileSpec::l0(P, DAY1, "00000001-001").level(2), vec![ev("a3", Some("carol"), d + 3, "x"), ev("a4", Some("dave"), d + 4, "x")])
        .await;
    let engine = engine(&store).await;
    let run = |filter: serde_json::Value| {
        let engine = engine.clone();
        async move {
            let cx = cx(&engine, DAY1, DAY1).await;
            let f = Filter::parse(&filter).unwrap();
            let req = ScanRequest { exprs: cols(&["person_key", "ts", "event"]), filters: vec![cx.compiler.compile(&f)], ordered: true };
            let rows = triples(&collect(&cx, &req).await, d);
            (rows, cx.stats.files.load(std::sync::atomic::Ordering::Relaxed))
        }
    };
    let (rows, files) = run(serde_json::json!({"prop": "person_key", "op": "eq", "value": "u:carol"})).await;
    assert_eq!(rows, [t("u:carol", 3, "x")]);
    assert_eq!(files, 1, "only the file whose person range contains the key is read");
    let (rows, files) = run(serde_json::json!({"prop": "person_key", "op": "in", "value": ["u:alice", "u:bob"]})).await;
    assert_eq!((rows.len(), files), (2, 1));
    let (rows, files) = run(serde_json::json!({"prop": "person_key", "op": "in", "value": ["u:alice", "u:dave"]})).await;
    assert_eq!((rows.len(), files), (2, 2));
    let (rows, files) = run(serde_json::json!({"prop": "person_key", "op": "eq", "value": "u:zed"})).await;
    assert_eq!((rows.len(), files), (0, 0));
    // A negated filter cannot prune.
    let (rows, files) = run(serde_json::json!({"prop": "person_key", "op": "ne", "value": "u:carol"})).await;
    assert_eq!((rows.len(), files), (3, 2));
}
