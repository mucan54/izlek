//! HTTP-level tests of every endpoint against `spec/query.md`: shapes,
//! status codes, limits and the result cache.

mod common;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use common::*;
use la_core::events::Cell;
use la_core::schema::PropType;
use la_core::{keys, Store};
use serde_json::{json, Value};
use std::time::Duration;

const A: u32 = 1017;
const KEY: &str = "sk_test_a";
const H: i64 = 3_600_000;

/// Three people over two days, one promoted column introduced on day 2.
async fn seed(store: &Store) {
    let (d1, d2) = (day_ms(DAY1), day_ms(DAY2));
    let promoted = [col("amount", PropType::Float, 2)];
    add_file(
        store,
        FileSpec::l0(A, DAY1, "a").level(2),
        vec![
            ev("devA", Some("alice"), d1 + H, "signup").platform("ios").country("TR").uuid(101).session(9),
            ev("devA", Some("alice"), d1 + 2 * H, "purchase").platform("ios").props(r#"{"amount":10,"sku":"x1"}"#).uuid(102),
            ev("devB", Some("bob"), d1 + 3 * H, "signup").platform("web").uuid(103),
        ],
    )
    .await;
    add_file(
        store,
        FileSpec::l0(A, DAY2, "a").schema(2, &promoted),
        vec![
            ev("devA", Some("alice"), d2 + H, "purchase").platform("web").promoted(vec![Some(Cell::Float(5.5))]).uuid(201),
            ev("devB", Some("bob"), d2 + 2 * H, "view").platform("web").uuid(202),
            ev("devC", None, d2 + 3 * H, "signup").platform("web").uuid(203),
        ],
    )
    .await;
    save_schema(store, A, 2, &promoted).await;
    set_person(store, A, 1, &[("u:alice", "plan", "\"pro\"", 10), ("u:bob", "plan", "\"free\"", 10)]).await;
    add_key(store, KEY, A, &["query"]).await;
    add_key(store, "sk_noscope", A, &["ingest"]).await;
}

fn range() -> Value {
    json!({"from": DAY1, "to": DAY2})
}

fn with(mut base: Value, extra: Value) -> Value {
    for (k, v) in extra.as_object().unwrap() {
        base[k] = v.clone();
    }
    base
}

fn assert_meta(v: &Value) {
    let m = &v["meta"];
    assert!(m["elapsed_ms"].is_u64(), "{m}");
    assert!(m["bytes_scanned"].is_u64(), "{m}");
    assert!(m["files"].is_u64(), "{m}");
    assert!(m["cached"].is_boolean(), "{m}");
    assert!(m["manifests"].is_object(), "{m}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn authentication_and_error_envelope() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let body = with(range(), json!({"series": [{"event": null, "agg": "count"}]}));

    // No credentials / unknown key / malformed header → 401.
    let r = app.send(Request::post("/q/trends").body(Body::from(body.to_string())).unwrap()).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::UNAUTHORIZED, "unauthorized"));
    assert!(r.json()["error"]["message"].is_string());
    assert_eq!(app.post("sk_wrong", "/q/trends", body.clone()).await.status, StatusCode::UNAUTHORIZED);
    assert_eq!(app.post("pk_live_abc", "/q/trends", body.clone()).await.status, StatusCode::UNAUTHORIZED);
    let basic = Request::post("/q/trends").header("authorization", "Basic abc").body(Body::from(body.to_string())).unwrap();
    assert_eq!(app.send(basic).await.status, StatusCode::UNAUTHORIZED);
    // The stored hash is not a credential.
    let hash = la_core::control::sha256_hex(KEY);
    assert_eq!(app.post(&hash, "/q/trends", body.clone()).await.status, StatusCode::UNAUTHORIZED);
    // Valid key without the query scope → 403.
    let r = app.post("sk_noscope", "/q/trends", body.clone()).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::FORBIDDEN, "forbidden"));
    // Valid.
    assert_meta(&app.ok(KEY, "/q/trends", body.clone()).await);

    // 400s: broken JSON, non-object, unknown field, bad range, >400 days.
    let raw = Request::post("/q/trends").header("authorization", format!("Bearer {KEY}")).body(Body::from("{nope")).unwrap();
    let r = app.send(raw).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::BAD_REQUEST, "bad_request"));
    for bad in [
        json!([1, 2]),
        with(body.clone(), json!({"project": 2})),
        with(body.clone(), json!({"from": "2026-11-05"})),
        with(body.clone(), json!({"from": "2025-01-01"})),
        with(body.clone(), json!({"to": "tomorrow"})),
        json!({"series": [{"event": null}]}),
        with(range(), json!({"series": []})),
        with(range(), json!({"series": [{"agg": "median"}]})),
        with(range(), json!({"series": [{"agg": "sum"}]})),
        with(body.clone(), json!({"interval": "minute"})),
        with(body.clone(), json!({"breakdown": "nope"})),
        with(body.clone(), json!({"filter": {"prop": "event", "op": "regex", "value": "x"}})),
    ] {
        let r = app.post(KEY, "/q/trends", bad.clone()).await;
        assert_eq!(r.status, StatusCode::BAD_REQUEST, "{bad}");
        assert_eq!(r.code(), "bad_request");
    }
    // Unknown path / wrong method.
    assert_eq!(app.get(KEY, "/q/nope").await.status, StatusCode::NOT_FOUND);
    assert_eq!(app.get(KEY, "/q/trends").await.status, StatusCode::METHOD_NOT_ALLOWED);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn internal_header_path() {
    let store = Store::memory();
    seed(&store).await;
    let body = with(range(), json!({"series": [{"event": null, "agg": "count"}]}));
    let internal = |secret: &str, project: &str| {
        Request::post("/q/trends").header("x-la-internal", secret).header("x-la-project", project).body(Body::from(body.to_string())).unwrap()
    };
    // Secret not configured: the path is closed, whatever is presented.
    let closed = app(&store).await;
    for secret in ["", "anything", "null", "undefined"] {
        assert_eq!(closed.send(internal(secret, "1017")).await.status, StatusCode::UNAUTHORIZED, "{secret:?}");
    }
    // A project header alone selects nothing, even next to a valid key.
    let sneaky = Request::post("/q/trends")
        .header("authorization", format!("Bearer {KEY}"))
        .header("x-la-project", "2")
        .body(Body::from(body.to_string()))
        .unwrap();
    assert_eq!(closed.send(sneaky).await.status, StatusCode::UNAUTHORIZED);

    let cfg = la_query::config::Config { internal_secret: Some("s3cret".into()), ..test_config() };
    let open = app_with(&store, cfg).await;
    assert_eq!(open.send(internal("wrong", "1017")).await.status, StatusCode::UNAUTHORIZED);
    assert_eq!(open.send(internal("", "1017")).await.status, StatusCode::UNAUTHORIZED);
    assert_eq!(open.send(internal("s3cret", "abc")).await.status, StatusCode::BAD_REQUEST);
    assert_eq!(open.send(internal("s3cret", "0")).await.status, StatusCode::BAD_REQUEST);
    assert_eq!(open.send(internal("s3cret", "1017/../2")).await.status, StatusCode::BAD_REQUEST);
    let r = open.send(internal("s3cret", "1017")).await;
    assert_eq!(r.status, StatusCode::OK);
    assert_eq!(r.json()["series"][0]["values"], json!([3, 3]));
    // A wrong internal secret is not rescued by a valid bearer key.
    let both = Request::post("/q/trends")
        .header("authorization", format!("Bearer {KEY}"))
        .header("x-la-internal", "wrong")
        .header("x-la-project", "1017")
        .body(Body::from(body.to_string()))
        .unwrap();
    assert_eq!(open.send(both).await.status, StatusCode::UNAUTHORIZED);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn trends() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let v = app
        .ok(
            KEY,
            "/q/trends",
            with(
                range(),
                json!({"interval": "day", "series": [
                    {"event": "purchase", "agg": "count", "label": "Satın alma"},
                    {"event": null, "agg": "unique"},
                    {"event": "purchase", "agg": "sum", "prop": "props.amount"},
                    {"event": "purchase", "agg": "avg", "prop": "props.amount", "filter": {"prop": "platform", "op": "eq", "value": "web"}},
                    {"event": "nothing", "agg": "max", "prop": "props.amount"}
                ]}),
            ),
        )
        .await;
    assert_eq!(v["buckets"], json!(["2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"]));
    let s = v["series"].as_array().unwrap();
    assert_eq!(s.len(), 5);
    assert_eq!(s[0], json!({"index": 0, "label": "Satın alma", "values": [1, 1]}));
    assert_eq!(s[1]["values"], json!([2, 3]));
    // Day 1 amount lives in props JSON, day 2 in the promoted column.
    assert_eq!(s[2]["values"], json!([10.0, 5.5]));
    assert_eq!(s[3]["values"], json!([null, 5.5]));
    assert_eq!(s[4]["values"], json!([null, null]));
    assert_meta(&v);
    assert_eq!(v["meta"]["manifests"], json!({DAY1: 1, DAY2: 1}));
    assert_eq!(v["meta"]["files"], 2);

    // Breakdown with limit: top value plus `$other` (none for unique).
    let v = app
        .ok(
            KEY,
            "/q/trends",
            with(
                range(),
                json!({"series": [{"event": null, "agg": "count"}, {"event": null, "agg": "unique"}],
                       "breakdown": "platform", "breakdown_limit": 1}),
            ),
        )
        .await;
    let s = v["series"].as_array().unwrap();
    assert_eq!(
        s,
        &[
            json!({"index": 0, "label": null, "breakdown": "web", "values": [1, 3]}),
            json!({"index": 0, "label": null, "breakdown": "$other", "values": [2, 0]}),
            json!({"index": 1, "label": null, "breakdown": "web", "values": [1, 3]}),
        ]
    );
    // Null breakdown bucket, person property breakdown, week/month/hour buckets.
    let v = app
        .ok(KEY, "/q/trends", with(range(), json!({"series": [{"event": null, "agg": "unique"}], "breakdown": "person.plan", "interval": "week"})))
        .await;
    assert_eq!(v["buckets"], json!(["2026-10-26T00:00:00Z", "2026-11-02T00:00:00Z"]));
    let by: std::collections::BTreeMap<String, Value> =
        v["series"].as_array().unwrap().iter().map(|s| (s["breakdown"].to_string(), s["values"].clone())).collect();
    assert_eq!(by["\"pro\""], json!([1, 1]));
    assert_eq!(by["\"free\""], json!([1, 1]));
    assert_eq!(by["null"], json!([0, 1]));
    let v = app.ok(KEY, "/q/trends", with(range(), json!({"series": [{"agg": "unique"}], "interval": "month"}))).await;
    assert_eq!(v["series"][0]["values"], json!([3]));
    let v = app
        .ok(KEY, "/q/trends", json!({"from": "2026-11-01T00:30:00Z", "to": "2026-11-01T03:00:00Z", "series": [{"agg": "count"}], "interval": "hour"}))
        .await;
    assert_eq!(v["buckets"].as_array().unwrap().len(), 3);
    assert_eq!(v["series"][0]["values"], json!([0, 1, 1]), "RFC 3339 `to` is exclusive");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn funnel() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let base = with(range(), json!({"steps": [{"event": "signup"}, {"event": "purchase"}], "window_seconds": 604800}));
    let v = app.ok(KEY, "/q/funnel", base.clone()).await;
    assert_eq!(
        v["steps"],
        json!([
            {"event": "signup", "count": 3, "median_seconds": null},
            {"event": "purchase", "count": 1, "median_seconds": 3600.0}
        ])
    );
    assert!(v.get("breakdowns").is_none());
    assert_meta(&v);
    // Window too small for alice's purchase (1 h later).
    let v = app.ok(KEY, "/q/funnel", with(base.clone(), json!({"window_seconds": 3599}))).await;
    assert_eq!(v["steps"][1]["count"], 0);
    assert_eq!(v["steps"][1]["median_seconds"], Value::Null);
    // Breakdown by the step-1 event's platform; step filter on a property.
    let v = app
        .ok(
            KEY,
            "/q/funnel",
            with(
                range(),
                json!({"steps": [{"event": "signup"}, {"event": "purchase", "filter": {"prop": "props.amount", "op": "gt", "value": 7}}],
                       "breakdown": "platform", "order": "ordered"}),
            ),
        )
        .await;
    assert_eq!(
        v["breakdowns"],
        json!([
            {"value": "web", "steps": [{"event": "signup", "count": 2, "median_seconds": null}, {"event": "purchase", "count": 0, "median_seconds": null}]},
            {"value": "ios", "steps": [{"event": "signup", "count": 1, "median_seconds": null}, {"event": "purchase", "count": 1, "median_seconds": 3600.0}]}
        ])
    );
    for order in ["strict", "any"] {
        let v = app.ok(KEY, "/q/funnel", with(base.clone(), json!({"order": order}))).await;
        assert_eq!(v["steps"][0]["count"], 3, "{order}");
        assert_eq!(v["steps"][1]["count"], 1, "{order}");
    }
    // Global filter on a person property.
    let v = app
        .ok(KEY, "/q/funnel", with(base.clone(), json!({"filter": {"prop": "person.plan", "op": "eq", "value": "free"}})))
        .await;
    assert_eq!(v["steps"][0]["count"], 1);
    for bad in [
        with(range(), json!({"steps": [{"event": "signup"}]})),
        with(base.clone(), json!({"order": "sideways"})),
        with(base.clone(), json!({"window_seconds": 0})),
        with(base.clone(), json!({"steps": [{"event": "a", "evnt": "b"}, {"event": "c"}]})),
        with(range(), json!({"steps": (0..21).map(|_| json!({"event": "x"})).collect::<Vec<_>>()})),
    ] {
        assert_eq!(app.post(KEY, "/q/funnel", bad.clone()).await.status, StatusCode::BAD_REQUEST, "{bad}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn retention() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let v = app
        .ok(KEY, "/q/retention", with(range(), json!({"period": "day", "periods": 8, "start": {"event": "signup"}, "return": {"event": null}})))
        .await;
    assert_eq!(
        v["cohorts"],
        json!([
            {"period_start": "2026-11-01T00:00:00Z", "size": 2, "returned": [2, 2]},
            {"period_start": "2026-11-02T00:00:00Z", "size": 1, "returned": [1]}
        ])
    );
    assert_meta(&v);
    let v = app
        .ok(KEY, "/q/retention", with(range(), json!({"period": "day", "periods": 1, "start": {"event": "signup"}, "return": {"event": "purchase"}})))
        .await;
    assert_eq!(v["cohorts"][0]["returned"], json!([1]));
    for bad in [
        with(range(), json!({"period": "day"})),
        with(range(), json!({"period": "hour", "start": {"event": "signup"}})),
        with(range(), json!({"periods": 65, "start": {"event": "signup"}})),
    ] {
        assert_eq!(app.post(KEY, "/q/retention", bad.clone()).await.status, StatusCode::BAD_REQUEST, "{bad}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn events_shape_and_pagination() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let v = app.ok(KEY, "/q/events", with(range(), json!({"limit": 2}))).await;
    let page = v["events"].as_array().unwrap();
    assert_eq!(page.len(), 2);
    assert_eq!(page[0]["event"], "signup", "newest first by default");
    assert_eq!(page[0]["person_key"], "a:devC");
    assert_eq!(page[0]["user_id"], Value::Null);
    assert!(v["next_cursor"].is_string());
    assert_meta(&v);

    // Walk both directions with different page sizes: same sequence, no gaps or repeats.
    let mut reference: Vec<String> = Vec::new();
    for (order, limit) in [("asc", 1), ("asc", 2), ("asc", 6), ("asc", 1000), ("desc", 1), ("desc", 4), ("desc", 5)] {
        let mut seen: Vec<String> = Vec::new();
        let mut cursor: Option<String> = None;
        let mut pages = 0;
        loop {
            let mut body = with(range(), json!({"limit": limit, "order": order}));
            if let Some(c) = &cursor {
                body["cursor"] = json!(c);
            }
            let v = app.ok(KEY, "/q/events", body).await;
            let events = v["events"].as_array().unwrap();
            assert!(events.len() <= limit);
            seen.extend(events.iter().map(|e| e["uuid"].as_str().unwrap().to_owned()));
            pages += 1;
            match v["next_cursor"].as_str() {
                Some(c) => cursor = Some(c.to_owned()),
                None => break,
            }
            assert!(pages < 20);
        }
        assert_eq!(seen.len(), 6, "{order}/{limit}");
        if order == "desc" {
            seen.reverse();
        }
        if reference.is_empty() {
            reference = seen;
        } else {
            assert_eq!(seen, reference, "{order}/{limit}");
        }
    }
    let ts_order: Vec<String> = [101u64, 102, 103, 201, 202, 203].iter().map(|n| la_codec::ids::hex32(&uuid(*n))).collect();
    assert_eq!(reference, ts_order);

    // Full shape of one event; props is the logical view (promoted folded in).
    let v = app
        .ok(KEY, "/q/events", with(range(), json!({"order": "asc", "filter": {"prop": "event", "op": "eq", "value": "purchase"}})))
        .await;
    let e = &v["events"];
    assert_eq!(e.as_array().unwrap().len(), 2);
    assert_eq!(v["next_cursor"], Value::Null);
    let d1 = day_ms(DAY1);
    assert_eq!(
        e[0],
        json!({
            "uuid": la_codec::ids::hex32(&uuid(102)), "ts": d1 + 2 * H, "recv_ts": d1 + 2 * H + 5, "event": "purchase",
            "person_key": "u:alice", "anon_id": "devA", "user_id": "alice", "session_id": null,
            "platform": "ios", "os": null, "os_version": null, "device_model": null, "device_type": null,
            "app_version": null, "locale": null, "country": null, "sdk": null, "url_path": null,
            "referrer_host": null, "screen": null, "flags": 0, "props": {"amount": 10, "sku": "x1"}
        })
    );
    assert_eq!(e[1]["props"], json!({"amount": 5.5}));
    // Session filter (hex32) and its shape.
    let sid = la_codec::ids::hex32(&uuid(9));
    let v = app.ok(KEY, "/q/events", with(range(), json!({"filter": {"prop": "session_id", "op": "eq", "value": sid}}))).await;
    assert_eq!(v["events"].as_array().unwrap().len(), 1);
    assert_eq!(v["events"][0]["session_id"], json!(sid));
    assert_eq!(v["events"][0]["country"], "TR");

    for bad in [
        with(range(), json!({"limit": 0})),
        with(range(), json!({"limit": 1001})),
        with(range(), json!({"order": "sideways"})),
        with(range(), json!({"cursor": "!!!"})),
        with(range(), json!({"cursor": "e30"})),
    ] {
        assert_eq!(app.post(KEY, "/q/events", bad.clone()).await.status, StatusCode::BAD_REQUEST, "{bad}");
    }
    // A cursor is bound to the query that produced it.
    let v = app.ok(KEY, "/q/events", with(range(), json!({"limit": 1}))).await;
    let c = v["next_cursor"].as_str().unwrap();
    for other in [
        with(range(), json!({"limit": 1, "cursor": c, "order": "asc"})),
        with(range(), json!({"limit": 1, "cursor": c, "filter": {"prop": "event", "op": "eq", "value": "view"}})),
        json!({"from": DAY2, "to": DAY2, "limit": 1, "cursor": c}),
    ] {
        assert_eq!(app.post(KEY, "/q/events", other.clone()).await.status, StatusCode::BAD_REQUEST, "{other}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sql() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let q = |sql: &str| with(range(), json!({"sql": sql}));
    let v = app.ok(KEY, "/q/sql", q("SELECT event, count(*) AS n FROM events GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 10")).await;
    assert_eq!(v["columns"], json!([{"name": "event", "type": "Utf8"}, {"name": "n", "type": "Int64"}]));
    assert_eq!(v["rows"], json!([["signup", 3], ["purchase", 2], ["view", 1]]));
    assert_meta(&v);
    // Type mapping: timestamp → ms, binary → hex, big ints → string, floats, nulls.
    let v = app
        .ok(
            KEY,
            "/q/sql",
            q("SELECT ts, uuid, hex(uuid) AS h, session_id, 9007199254740993 AS big, 1.5 AS f, flags, user_id IS NULL AS anon, \
               prop(props, 'sku') AS sku, prop_num(props, 'amount') AS amount, p_amount \
               FROM events WHERE event = 'purchase' ORDER BY ts"),
        )
        .await;
    let d1 = day_ms(DAY1);
    let u = la_codec::ids::hex32(&uuid(102));
    assert_eq!(v["rows"][0], json!([d1 + 2 * H, u, u, null, "9007199254740993", 1.5, 0, "false", "x1", 10.0, null]));
    assert_eq!(v["rows"][1][9], Value::Null, "promoted value is in the column, not in props");
    assert_eq!(v["rows"][1][10], json!(5.5));
    // Persons table, joins, CTEs, subqueries, window functions over the declared order.
    let v = app
        .ok(
            KEY,
            "/q/sql",
            q("WITH buyers AS (SELECT DISTINCT person_key FROM events WHERE event = 'purchase') \
               SELECT p.person_key, prop(p.props, 'plan') AS plan, \
                      (SELECT count(*) FROM events e WHERE e.person_key = p.person_key) AS n \
               FROM persons p JOIN buyers b ON b.person_key = p.person_key ORDER BY 1"),
        )
        .await;
    assert_eq!(v["rows"], json!([["u:alice", "pro", 3]]));
    let v = app
        .ok(
            KEY,
            "/q/sql",
            q("SELECT person_key, event, row_number() OVER (PARTITION BY person_key ORDER BY ts) AS rn FROM events ORDER BY person_key, ts"),
        )
        .await;
    assert_eq!(v["rows"].as_array().unwrap().len(), 6);
    assert_eq!(v["rows"][1], json!(["u:alice", "signup", 1]));
    // The time range bounds the table.
    let v = app.ok(KEY, "/q/sql", json!({"from": DAY2, "to": DAY2, "sql": "SELECT count(*) FROM events"})).await;
    assert_eq!(v["rows"], json!([[3]]));

    for (bad, why) in [
        ("SELECT 1; SELECT 2", "stacked"),
        ("DROP TABLE events", "ddl"),
        ("CREATE TABLE t AS SELECT * FROM events", "ctas"),
        ("CREATE EXTERNAL TABLE t STORED AS PARQUET LOCATION 'data/'", "external"),
        ("INSERT INTO events SELECT * FROM events", "dml"),
        ("DELETE FROM events", "dml"),
        ("COPY events TO 'out.parquet'", "copy"),
        ("SET datafusion.execution.batch_size = 1", "set"),
        ("EXPLAIN SELECT * FROM events", "explain"),
        ("EXPLAIN ANALYZE SELECT * FROM events", "explain analyze"),
        ("SHOW TABLES", "show"),
        ("DESCRIBE events", "describe"),
        ("SELECT * FROM information_schema.tables", "catalog"),
        ("SELECT * FROM nope", "unknown table"),
        ("SELECT * FROM read_parquet('data/p=1017/d=2026-11-01/L2-a.parquet')", "file function"),
        ("SELECT * FROM 'data/p=1017/d=2026-11-01/L2-a.parquet'", "path table"),
        ("SELECT nope FROM events", "unknown column"),
        ("SELECT * FROM events WHERE event = $1", "placeholder"),
        ("SELEC", "syntax"),
        ("", "empty"),
    ] {
        let r = app.post(KEY, "/q/sql", q(bad)).await;
        assert_eq!(r.status, StatusCode::BAD_REQUEST, "{why}: {}", String::from_utf8_lossy(&r.bytes));
        assert_eq!(r.code(), "bad_request", "{why}");
    }
    assert_eq!(app.post(KEY, "/q/sql", range()).await.status, StatusCode::BAD_REQUEST);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sql_row_cap() {
    let store = Store::memory();
    seed(&store).await;
    let cfg = la_query::config::Config { sql_max_rows: 4, ..test_config() };
    let app = app_with(&store, cfg).await;
    let v = app.ok(KEY, "/q/sql", with(range(), json!({"sql": "SELECT event FROM events"}))).await;
    assert_eq!(v["rows"].as_array().unwrap().len(), 4);
    assert_eq!(v["truncated"], true);
    let v = app.ok(KEY, "/q/sql", with(range(), json!({"sql": "SELECT event FROM events LIMIT 3"}))).await;
    assert_eq!(v["rows"].as_array().unwrap().len(), 3);
    assert!(v.get("truncated").is_none());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn schema_replay_health_metrics() {
    let store = Store::memory();
    seed(&store).await;
    // Schema statistics as the shipper would have left them.
    let mut schema = la_core::schema::ProjectSchema::load(&store, A).await.unwrap();
    let day = schema.stats.days.entry(DAY2.into()).or_default();
    day.events = 6;
    day.names.insert("signup".into(), 3);
    day.names.insert("purchase".into(), 2);
    for (k, t, n) in [("sku", "string", 1u64), ("amount", "float", 2), ("mixed", "int", 5)] {
        let st = day.keys.entry(k.into()).or_default();
        st.n = n;
        st.types.insert(t.into(), n);
    }
    schema.save(&store).await.unwrap();
    let s = "0123456789abcdef0123456789abcdef";
    let chunk_key = format!("replay/p={A}/d={DAY1}/{s}/000000.bin.zst");
    store.put(&chunk_key, &b"\x28\xb5\x2f\xfdchunk"[..]).await.unwrap();
    store.put(&chunk_key.replace(".zst", ".gz").replace("000000", "000001"), &b"\x1f\x8bgz"[..]).await.unwrap();

    let app = app(&store).await;
    let v = app.ok(KEY, "/q/schema", json!({})).await;
    assert_eq!(v["events"], json!(["signup", "purchase"]));
    assert_eq!(
        v["props"],
        json!([
            {"key": "mixed", "type": "int", "promoted": false},
            {"key": "amount", "type": "float", "promoted": true},
            {"key": "sku", "type": "string", "promoted": false}
        ])
    );
    assert_eq!(v["person_props"], json!(["plan"]));
    assert_eq!(app.post("sk_wrong", "/q/schema", json!({})).await.status, StatusCode::UNAUTHORIZED);
    assert_eq!(app.post(KEY, "/q/schema", json!({"project": 2})).await.status, StatusCode::BAD_REQUEST);

    let r = app.get(KEY, &format!("/q/replay?key={chunk_key}")).await;
    assert_eq!(r.status, StatusCode::OK);
    assert_eq!(r.headers["content-encoding"], "zstd");
    assert_eq!(r.bytes, b"\x28\xb5\x2f\xfdchunk");
    let r = app.get(KEY, &format!("/q/replay?key=replay/p={A}/d={DAY1}/{s}/000001.bin.gz")).await;
    assert_eq!((r.status, r.headers["content-encoding"].to_str().unwrap()), (StatusCode::OK, "gzip"));
    assert_eq!(app.get(KEY, &format!("/q/replay?key=replay/p={A}/d={DAY1}/{s}/000009.bin.zst")).await.status, StatusCode::NOT_FOUND);
    assert_eq!(app.get(KEY, "/q/replay").await.status, StatusCode::BAD_REQUEST);
    assert_eq!(app.get(KEY, &format!("/q/replay?key=replay/p=2/d={DAY1}/{s}/000000.bin.zst")).await.status, StatusCode::FORBIDDEN);
    assert_eq!(app.get("sk_wrong", &format!("/q/replay?key={chunk_key}")).await.status, StatusCode::UNAUTHORIZED);

    // Unauthenticated operational endpoints.
    let plain = |p: &str| Request::get(p).body(Body::empty()).unwrap();
    assert_eq!(app.send(plain("/healthz")).await.status, StatusCode::OK);
    assert_eq!(app.send(plain("/readyz")).await.status, StatusCode::OK);
    app.ok(KEY, "/q/trends", with(range(), json!({"series": [{"agg": "count"}]}))).await;
    app.ok(KEY, "/q/trends", with(range(), json!({"series": [{"agg": "count"}]}))).await;
    let text = String::from_utf8(app.send(plain("/metrics")).await.bytes).unwrap();
    for needle in [
        "la_query_queries_total{type=\"trends\",status=\"ok\"} 2",
        "la_query_query_seconds_count{type=\"trends\"} 2",
        "la_query_bytes_scanned_total ",
        "la_query_cache_hit_ratio 0.5",
        "la_query_queue_depth 0",
        "la_query_memory_pool_bytes ",
    ] {
        assert!(text.contains(needle), "missing {needle:?} in\n{text}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn readiness_requires_control_files() {
    // A store whose control file is unreadable JSON: the service must come up
    // not-ready and reject keys instead of crashing.
    let store = Store::memory();
    store.put(keys::KEYS, &b"{broken"[..]).await.unwrap();
    let engine = la_query::state::Engine::new(store.clone(), test_config()).unwrap();
    assert!(engine.refresh_control().await.is_err());
    let svc = la_query::service::Service::new(engine);
    let app = App { router: la_query::http::router(svc.clone()), svc };
    let r = app.send(Request::get("/readyz").body(Body::empty()).unwrap()).await;
    assert_eq!(r.status, StatusCode::SERVICE_UNAVAILABLE);
    // Once the file is fixed the next refresh picks it up; a later outage
    // keeps the last good copy.
    add_key_raw(&store, KEY, A).await;
    app.svc.engine.refresh_tick().await;
    assert_eq!(app.send(Request::get("/readyz").body(Body::empty()).unwrap()).await.status, StatusCode::OK);
    store.put(keys::KEYS, &b"{broken again"[..]).await.unwrap();
    app.svc.engine.refresh_tick().await;
    let r = app.post(KEY, "/q/trends", with(range(), json!({"series": [{"agg": "count"}]}))).await;
    assert_eq!(r.status, StatusCode::OK, "last good keys still authenticate");
}

async fn add_key_raw(store: &Store, sk: &str, project: u32) {
    let hash = la_core::control::sha256_hex(sk);
    store
        .put(keys::KEYS, format!(r#"{{"version":1,"keys":{{"{hash}":{{"project_id":{project},"scopes":["query"]}}}}}}"#).into_bytes())
        .await
        .unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn result_cache_follows_manifest_versions() {
    let store = Store::memory();
    seed(&store).await;
    let app = app(&store).await;
    let body = with(range(), json!({"series": [{"event": "signup", "agg": "count"}]}));
    let first = app.ok(KEY, "/q/trends", body.clone()).await;
    assert_eq!(first["meta"]["cached"], false);
    assert_eq!(first["series"][0]["values"], json!([2, 1]));
    assert!(first["meta"]["bytes_scanned"].as_u64().unwrap() > 0);
    // Same request, different key order and whitespace → same cache entry.
    let reordered: Value = serde_json::from_str(&format!(
        r#"{{ "series": [ {{"agg": "count", "event": "signup"}} ], "to": "{DAY2}", "from": "{DAY1}" }}"#
    ))
    .unwrap();
    let second = app.ok(KEY, "/q/trends", reordered).await;
    assert_eq!(second["meta"]["cached"], true);
    assert_eq!(second["meta"]["bytes_scanned"], 0);
    assert_eq!(second["series"], first["series"]);
    assert_eq!(second["meta"]["manifests"], first["meta"]["manifests"]);
    // A different request is a different entry.
    let other = app.ok(KEY, "/q/trends", with(range(), json!({"series": [{"event": "view", "agg": "count"}]}))).await;
    assert_eq!(other["meta"]["cached"], false);

    // New file → new manifest version → the old entry no longer matches.
    add_file(&store, FileSpec::l0(A, DAY2, "late"), vec![ev("devD", None, day_ms(DAY2) + 5, "signup")]).await;
    let third = app.ok(KEY, "/q/trends", body.clone()).await;
    assert_eq!(third["meta"]["cached"], false);
    assert_eq!(third["series"][0]["values"], json!([2, 2]));
    assert_eq!(third["meta"]["manifests"][DAY2], 2);
    assert_eq!(app.ok(KEY, "/q/trends", body.clone()).await["meta"]["cached"], true);

    // Identity links and tombstones are part of the key as well.
    link(&store, A, 1, &[("devD", "dora")]).await;
    assert_eq!(app.ok(KEY, "/q/trends", body.clone()).await["meta"]["cached"], false);
    assert_eq!(app.ok(KEY, "/q/trends", body.clone()).await["meta"]["cached"], true);
    tombstone(&store, A, 99, &["u:dora"], &[]).await;
    let after = app.ok(KEY, "/q/trends", body.clone()).await;
    assert_eq!(after["meta"]["cached"], false);
    assert_eq!(after["series"][0]["values"], json!([2, 1]), "deleted person is gone immediately");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn limits_scan_bytes_timeout_and_queue() {
    let store = Store::memory();
    seed(&store).await;
    let body = with(range(), json!({"series": [{"agg": "count"}]}));

    // 422: per-project scan budget from projects.json.
    store
        .put(keys::PROJECTS, format!(r#"{{"version":1,"projects":{{"{A}":{{"scan_limit_bytes":16,"max_concurrent_queries":1}}}}}}"#).into_bytes())
        .await
        .unwrap();
    let tight = app(&store).await;
    let r = tight.post(KEY, "/q/trends", body.clone()).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::UNPROCESSABLE_ENTITY, "scan_limit"));
    let r = tight.post(KEY, "/q/sql", with(range(), json!({"sql": "SELECT count(DISTINCT uuid) FROM events"}))).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::UNPROCESSABLE_ENTITY, "scan_limit"));
    // …and the service-wide default when the project has none.
    store.delete(keys::PROJECTS).await.unwrap();
    let cfg = la_query::config::Config { scan_limit_bytes: 16, ..test_config() };
    let r = app_with(&store, cfg).await.post(KEY, "/q/funnel", with(range(), json!({"steps": [{"event": "signup"}, {"event": "purchase"}]}))).await;
    assert_eq!(r.status, StatusCode::UNPROCESSABLE_ENTITY);
    // Row groups pruned by statistics are never fetched, so they cost nothing.
    let cfg = la_query::config::Config { scan_limit_bytes: 16, ..test_config() };
    let r = app_with(&store, cfg).await.post(KEY, "/q/funnel", with(range(), json!({"steps": [{"event": "a"}, {"event": "b"}]}))).await;
    assert_eq!(r.status, StatusCode::OK);
    assert_eq!(r.json()["meta"]["bytes_scanned"], 0);

    // 429: one slot, no queue. Hold the slot, the next query is rejected.
    store
        .put(keys::PROJECTS, format!(r#"{{"version":2,"projects":{{"{A}":{{"max_concurrent_queries":1}}}}}}"#).into_bytes())
        .await
        .unwrap();
    let cfg = la_query::config::Config { queue_max: 0, ..test_config() };
    let busy = app_with(&store, cfg).await;
    let permit = busy.svc.limiter.acquire(A, 1).await.unwrap();
    let r = busy.post(KEY, "/q/trends", body.clone()).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::TOO_MANY_REQUESTS, "queue_full"));
    drop(permit);
    assert_eq!(busy.post(KEY, "/q/trends", body.clone()).await.status, StatusCode::OK);

    // 408: with a queue, a query stuck behind the held slot times out.
    let cfg = la_query::config::Config { queue_max: 4, timeout: Duration::from_millis(80), ..test_config() };
    let slow = app_with(&store, cfg).await;
    let permit = slow.svc.limiter.acquire(A, 1).await.unwrap();
    let r = slow.post(KEY, "/q/trends", body.clone()).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::REQUEST_TIMEOUT, "timeout"));
    assert_eq!(slow.svc.limiter.queued.load(std::sync::atomic::Ordering::Relaxed), 0, "timed-out waiter left the queue");
    drop(permit);
    assert_eq!(slow.post(KEY, "/q/trends", body.clone()).await.status, StatusCode::OK);
    let text = slow.svc.render_metrics();
    assert!(text.contains("la_query_queries_total{type=\"trends\",status=\"timeout\"} 1"), "{text}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn memory_budget_is_enforced() {
    let store = Store::memory();
    let d = day_ms(DAY1);
    let events: Vec<Ev> = (0..30_000).map(|i| ev(&format!("dev{i:06}"), None, d + i, "e").props(&format!(r#"{{"k":"value-{i:06}"}}"#))).collect();
    add_file(&store, FileSpec::l0(A, DAY1, "big").level(2), events).await;
    add_key(&store, KEY, A, &["query"]).await;
    // 16 MB is the floor Config::from_env enforces; go below it directly.
    let cfg = la_query::config::Config { memory_bytes: 600_000, ..test_config() };
    let app = app_with(&store, cfg).await;
    let r = app
        .post(KEY, "/q/trends", json!({"from": DAY1, "to": DAY1, "series": [{"agg": "count"}], "breakdown": "props.k", "interval": "hour"}))
        .await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::UNPROCESSABLE_ENTITY, "memory_limit"), "{}", String::from_utf8_lossy(&r.bytes));
    let r = app.post(KEY, "/q/sql", json!({"from": DAY1, "to": DAY1, "sql": "SELECT props, count(*) FROM events GROUP BY 1 ORDER BY 1"})).await;
    assert_eq!((r.status, r.code().as_str()), (StatusCode::UNPROCESSABLE_ENTITY, "memory_limit"), "{}", String::from_utf8_lossy(&r.bytes));
    // The same queries fit in the default budget.
    let roomy = app_with(&store, test_config()).await;
    let v = roomy.ok(KEY, "/q/trends", json!({"from": DAY1, "to": DAY1, "series": [{"agg": "count"}], "breakdown": "props.k", "breakdown_limit": 3})).await;
    assert_eq!(v["series"].as_array().unwrap().len(), 4);
    assert_eq!(v["series"][3]["breakdown"], "$other");
    assert_eq!(v["series"][3]["values"], json!([29_997]));
}
