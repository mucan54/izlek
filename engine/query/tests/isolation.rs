//! Isolation suite (design §17.6): two projects share one store; every
//! avenue from project A's key towards project B's data must end in an empty
//! result, `400` or `403` — never B's data.

mod common;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use base64::Engine as _;
use common::*;
use la_core::Store;
use serde_json::{json, Value};

const A: u32 = 1;
const B: u32 = 2;
const KEY_A: &str = "sk_project_a";
const KEY_B: &str = "sk_project_b";
/// Substrings that only ever occur in project B's data.
const B_MARKERS: [&str; 6] = ["SECRET_B", "b_only_event", "bob_b", "devB_only", "plan_b_only", "0b0b0b0b"];
const SESSION_B: &str = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b";
const SESSION_A: &str = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a";

fn session_b() -> [u8; 16] {
    [0x0b; 16]
}

async fn seed() -> Store {
    let store = Store::memory();
    let d = day_ms(DAY1);
    add_file(
        &store,
        FileSpec::l0(A, DAY1, "a").level(2),
        vec![
            ev("devA1", Some("alice"), d + 1000, "signup").props(r#"{"k":"a-value"}"#).uuid(11),
            ev("devA1", Some("alice"), d + 2000, "purchase").uuid(12),
            ev("devA2", None, d + 3000, "signup").uuid(13),
        ],
    )
    .await;
    let mut b_event = ev("devB_only", Some("bob_b"), d + 1500, "b_only_event").props(r#"{"k":"SECRET_B"}"#).uuid(21);
    b_event.0.session_id = Some(session_b());
    add_file(
        &store,
        FileSpec::l0(B, DAY1, "b").level(2),
        vec![b_event, ev("devB_only", Some("bob_b"), d + 2500, "signup").props(r#"{"k":"SECRET_B"}"#).uuid(22)],
    )
    .await;
    set_person(&store, A, 1, &[("u:alice", "plan", "\"pro\"", 1)]).await;
    set_person(&store, B, 1, &[("u:bob_b", "plan_b_only", "\"SECRET_B\"", 1)]).await;
    link(&store, B, 1, &[("devB_only", "bob_b")]).await;
    store.put(&format!("replay/p={A}/d={DAY1}/{SESSION_A}/000000.bin.zst"), &b"chunk-of-a"[..]).await.unwrap();
    store.put(&format!("replay/p={B}/d={DAY1}/{SESSION_B}/000000.bin.zst"), &b"SECRET_B chunk"[..]).await.unwrap();
    add_key(&store, KEY_A, A, &["query"]).await;
    add_key(&store, KEY_B, B, &["query"]).await;
    store
}

/// The invariant: `400`/`401`/`403`/`404`, or a success that carries none of B's markers.
fn assert_no_leak(what: &str, r: &Reply) {
    let text = String::from_utf8_lossy(&r.bytes);
    for m in B_MARKERS {
        assert!(!text.contains(m), "{what}: response leaks {m:?}: {text}");
    }
    assert!(
        matches!(r.status.as_u16(), 200 | 400 | 401 | 403 | 404),
        "{what}: unexpected status {} ({text})",
        r.status
    );
}

fn range() -> Value {
    json!({"from": DAY1, "to": DAY1})
}

fn with(mut base: Value, extra: Value) -> Value {
    for (k, v) in extra.as_object().unwrap() {
        base[k] = v.clone();
    }
    base
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn each_key_sees_exactly_its_own_project() {
    let store = seed().await;
    let app = app(&store).await;
    let count = |v: &Value| v["rows"][0][0].as_i64().unwrap();
    let a = app.ok(KEY_A, "/q/sql", with(range(), json!({"sql": "SELECT count(*) FROM events"}))).await;
    let b = app.ok(KEY_B, "/q/sql", with(range(), json!({"sql": "SELECT count(*) FROM events"}))).await;
    assert_eq!((count(&a), count(&b)), (3, 2));
    // The control: B's own key does see the markers, so the checks below are meaningful.
    let b_rows = app.post(KEY_B, "/q/events", range()).await;
    let text = String::from_utf8_lossy(&b_rows.bytes).into_owned();
    for m in ["SECRET_B", "b_only_event", "bob_b", "devB_only", "0b0b0b0b"] {
        assert!(text.contains(m), "{m}");
    }
    let b_persons = app.post(KEY_B, "/q/sql", with(range(), json!({"sql": "SELECT * FROM persons"}))).await;
    assert!(String::from_utf8_lossy(&b_persons.bytes).contains("plan_b_only"));
    // Results are cached per project: the same request body from A must not hit B's entry.
    let body = with(range(), json!({"sql": "SELECT event FROM events ORDER BY 1"}));
    let from_b = app.ok(KEY_B, "/q/sql", body.clone()).await;
    let from_a = app.ok(KEY_A, "/q/sql", body.clone()).await;
    assert_eq!(from_a["meta"]["cached"], false);
    assert_ne!(from_a["rows"], from_b["rows"]);
    assert_eq!(app.ok(KEY_A, "/q/sql", body).await["meta"]["cached"], true);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sql_cannot_reach_another_project() {
    let store = seed().await;
    let app = app(&store).await;
    let b_file = format!("data/p={B}/d={DAY1}/L2-b.parquet");
    let attempts: Vec<String> = vec![
        // Plain and qualified names only ever resolve to the caller's tables.
        "SELECT * FROM events".into(),
        "SELECT * FROM persons".into(),
        "SELECT * FROM la.q.events".into(),
        "SELECT * FROM q.events".into(),
        "SELECT * FROM public.events".into(),
        "SELECT * FROM datafusion.public.events".into(),
        "SELECT * FROM p2.events".into(),
        "SELECT * FROM \"p=2\".events".into(),
        "SELECT * FROM \"2\".q.events".into(),
        "SELECT * FROM events_2".into(),
        "SELECT * FROM \"events\" e2 JOIN persons p ON true".into(),
        // Catalog / metadata.
        "SELECT * FROM information_schema.tables".into(),
        "SELECT * FROM information_schema.columns".into(),
        "SELECT * FROM information_schema.df_settings".into(),
        "SHOW TABLES".into(),
        "SHOW ALL".into(),
        "SHOW COLUMNS FROM events".into(),
        // File / URL access in every spelling.
        format!("SELECT * FROM '{b_file}'"),
        format!("SELECT * FROM \"{b_file}\""),
        format!("SELECT * FROM 'la://store/{b_file}'"),
        format!("SELECT * FROM 'file:///{b_file}'"),
        format!("SELECT * FROM 's3://bucket/{b_file}'"),
        format!("SELECT * FROM read_parquet('{b_file}')"),
        format!("SELECT * FROM parquet_scan('{b_file}')"),
        format!("SELECT * FROM read_csv('{b_file}')"),
        format!("SELECT * FROM read_json('control/keys.json')"),
        format!("SELECT * FROM parquet_metadata('{b_file}')"),
        format!("CREATE EXTERNAL TABLE b STORED AS PARQUET LOCATION 'la://store/data/p={B}/'"),
        format!("CREATE EXTERNAL TABLE b STORED AS PARQUET LOCATION '{b_file}'; SELECT * FROM b"),
        format!("COPY (SELECT 1) TO 'data/p={A}/x.parquet'"),
        format!("COPY events FROM '{b_file}'"),
        // DDL / DML / session.
        "CREATE VIEW v AS SELECT * FROM events".into(),
        "CREATE TABLE t AS SELECT 1".into(),
        "INSERT INTO events SELECT * FROM events".into(),
        "UPDATE events SET event = 'x'".into(),
        "DELETE FROM events".into(),
        "TRUNCATE TABLE events".into(),
        "SET datafusion.catalog.information_schema = true".into(),
        "SET datafusion.catalog.default_catalog = 'x'".into(),
        "PREPARE p AS SELECT * FROM events".into(),
        "EXPLAIN SELECT * FROM events".into(),
        "EXPLAIN ANALYZE SELECT * FROM events".into(),
        // Stacked statements and comment tricks.
        "SELECT 1; SELECT * FROM events".into(),
        "SELECT 1; SET datafusion.catalog.information_schema = true; SELECT * FROM information_schema.tables".into(),
        "SELECT * FROM events -- ; DROP TABLE events".into(),
        "SELECT * FROM events /* ; SELECT * FROM information_schema.tables */".into(),
        "SELECT * FROM events; -- trailing".into(),
        "/* x */ SELECT * /* y */ FROM /* z */ events".into(),
        "SELECT * FROM events WHERE event = 'x' OR 1 = 1 --'".into(),
        "SELECT * FROM events WHERE event = 'x'; DROP TABLE events; --'".into(),
        // CTE / subquery shadowing and set operations.
        "WITH events AS (SELECT * FROM persons) SELECT * FROM events".into(),
        "WITH information_schema AS (SELECT 1) SELECT * FROM information_schema".into(),
        "WITH RECURSIVE r AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM r WHERE n < 3) SELECT * FROM r".into(),
        format!("SELECT * FROM events UNION ALL SELECT * FROM '{b_file}'"),
        "SELECT (SELECT count(*) FROM information_schema.tables)".into(),
        "SELECT * FROM (SELECT * FROM (SELECT * FROM events) x) y".into(),
        "SELECT * FROM events WHERE person_key IN (SELECT person_key FROM persons)".into(),
        // Columns that do not exist must not be invented.
        "SELECT * FROM events WHERE project = 2".into(),
        "SELECT * FROM events WHERE \"la.project\" = '2'".into(),
        // Filters aimed at B's values simply match nothing.
        "SELECT * FROM events WHERE person_key = 'u:bob_b'".into(),
        "SELECT * FROM events WHERE anon_id = 'devB_only' OR event = 'b_only_event'".into(),
        "SELECT * FROM persons WHERE person_key = 'u:bob_b'".into(),
        "SELECT prop(props, 'k'), prop(props, '../../p=2'), hex(uuid) FROM events".into(),
        "SELECT * FROM events WHERE event = $1".into(),
    ];
    for sql in &attempts {
        let r = app.post(KEY_A, "/q/sql", with(range(), json!({ "sql": sql }))).await;
        assert_no_leak(sql, &r);
        if r.status == StatusCode::OK {
            // Whatever succeeded returned at most A's own rows.
            let rows = r.json()["rows"].as_array().map(Vec::len).unwrap_or(0);
            assert!(rows <= 3, "{sql}: {rows} rows");
        }
    }
    // None of the attempts changed anything.
    assert!(store.list(&format!("data/p={A}")).await.unwrap().iter().all(|o| !o.key.ends_with("x.parquet")));
    let after = app.ok(KEY_B, "/q/sql", with(range(), json!({"sql": "SELECT count(*) FROM events"}))).await;
    assert_eq!(after["rows"], json!([[2]]));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn filter_trees_and_references_cannot_cross_projects() {
    let store = seed().await;
    let app = app(&store).await;
    let filters = vec![
        json!({"prop": "person_key", "op": "eq", "value": "u:bob_b"}),
        json!({"prop": "anon_id", "op": "in", "value": ["devB_only"]}),
        json!({"prop": "event", "op": "eq", "value": "b_only_event"}),
        json!({"prop": "props.k", "op": "contains", "value": "SECRET"}),
        json!({"prop": "session_id", "op": "eq", "value": SESSION_B}),
        json!({"prop": "person.plan_b_only", "op": "is_set"}),
        json!({"or": [{"prop": "event", "op": "is_set"}, {"prop": "person_key", "op": "eq", "value": "u:bob_b"}]}),
        json!({"not": {"prop": "event", "op": "eq", "value": "nothing"}}),
        json!({"prop": "event", "op": "eq", "value": "x' OR '1'='1"}),
        json!({"prop": "event", "op": "eq", "value": "x'; DROP TABLE events; --"}),
        json!({"prop": "props.k' OR 1=1 --", "op": "is_set"}),
        json!({"prop": "props.k\") OR true --", "op": "eq", "value": 1}),
        json!({"prop": "props.../../p=2", "op": "is_set"}),
        json!({"prop": "person.x', 'y') OR true --", "op": "is_set"}),
        json!({"prop": "events.event", "op": "is_set"}),
        json!({"prop": "la.q.events.event", "op": "is_set"}),
        json!({"prop": "project", "op": "eq", "value": 2}),
        json!({"prop": "p=2", "op": "is_set"}),
        json!({"prop": "event) OR (true", "op": "is_set"}),
        json!({"prop": "event", "op": "in", "value": ["x", {"sql": "1=1"}]}),
        json!({"prop": "event", "op": "eq", "value": "x", "project": 2}),
        json!({"sql": "1=1"}),
    ];
    for f in &filters {
        for (path, body) in [
            ("/q/events", with(range(), json!({"filter": f}))),
            ("/q/trends", with(range(), json!({"filter": f, "series": [{"agg": "count"}], "breakdown": "props.k"}))),
            ("/q/funnel", with(range(), json!({"steps": [{"event": "signup", "filter": f}, {"event": null, "filter": f}], "breakdown": "person_key"}))),
            ("/q/retention", with(range(), json!({"start": {"event": null, "filter": f}, "filter": f}))),
        ] {
            let r = app.post(KEY_A, path, body).await;
            assert_no_leak(&format!("{path} {f}"), &r);
        }
    }
    // Filters aimed squarely at B's rows match nothing in A.
    for f in &filters[..6] {
        let v = app.ok(KEY_A, "/q/events", with(range(), json!({"filter": f}))).await;
        assert_eq!(v["events"], json!([]), "{f}");
    }
    // Breakdown / aggregation references are data, not SQL.
    for reference in ["props.k' UNION SELECT * FROM persons --", "person.plan_b_only", "props.k)) OR ((1=1", "events.event", "person_key; DROP"] {
        let r = app.post(KEY_A, "/q/trends", with(range(), json!({"series": [{"agg": "count"}], "breakdown": reference}))).await;
        assert_no_leak(reference, &r);
        let r = app.post(KEY_A, "/q/trends", with(range(), json!({"series": [{"agg": "sum", "prop": reference}]}))).await;
        assert_no_leak(reference, &r);
    }
    // No body field selects a project.
    for field in ["project", "project_id", "pid", "p", "tenant", "X-LA-Project"] {
        let mut body = with(range(), json!({"series": [{"agg": "count"}]}));
        body[field] = json!(B);
        let r = app.post(KEY_A, "/q/trends", body).await;
        assert_eq!(r.status, StatusCode::BAD_REQUEST, "{field}");
    }
    // Neither does the query string.
    let v = app.ok(KEY_A, &format!("/q/events?project={B}&p={B}&X-LA-Project={B}"), range()).await;
    assert_eq!(v["events"].as_array().unwrap().len(), 3);
    assert_no_leak("query string", &app.post(KEY_A, &format!("/q/events?project={B}"), range()).await);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn range_and_cursor_tampering() {
    let store = seed().await;
    let app = app(&store).await;
    for (from, to) in [
        (json!("2026-11-01/../../p=2"), json!(DAY1)),
        (json!(DAY1), json!("2026-11-01/../../../data/p=2/d=2026-11-01")),
        (json!("p=2"), json!("p=2")),
        (json!("../p=2/d=2026-11-01"), json!("../p=2/d=2026-11-01")),
        (json!("2026-11-01%2F..%2Fp%3D2"), json!(DAY1)),
        (json!(["2026-11-01"]), json!(DAY1)),
        (json!({"p": 2}), json!(DAY1)),
        (json!(20261101), json!(20261101)),
        (json!("0000-01-01"), json!("9999-12-31")),
        (json!("1970-01-01T00:00:00Z"), json!("2026-11-02T00:00:00Z")),
        (json!(""), json!("")),
    ] {
        let body = json!({"from": from, "to": to});
        for path in ["/q/events", "/q/sql"] {
            let mut b = body.clone();
            if path == "/q/sql" {
                b["sql"] = json!("SELECT * FROM events");
            }
            let r = app.post(KEY_A, path, b.clone()).await;
            assert_eq!(r.status, StatusCode::BAD_REQUEST, "{path} {b}");
        }
    }

    // A real cursor from B's session is useless with A's key…
    let page_b = app.ok(KEY_B, "/q/events", with(range(), json!({"limit": 1}))).await;
    let cursor_b = page_b["next_cursor"].as_str().unwrap().to_owned();
    let r = app.post(KEY_A, "/q/events", with(range(), json!({"limit": 1, "cursor": cursor_b}))).await;
    assert_eq!(r.status, StatusCode::BAD_REQUEST);
    // …and so is every hand-made variation of A's own cursor.
    let page_a = app.ok(KEY_A, "/q/events", with(range(), json!({"limit": 1}))).await;
    let cursor_a = page_a["next_cursor"].as_str().unwrap().to_owned();
    let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
    let decoded: Value = serde_json::from_slice(&b64.decode(&cursor_a).unwrap()).unwrap();
    let uuid_b = la_codec::ids::hex32(&uuid(21));
    let ts_b = day_ms(DAY1) + 1500;
    let forged: Vec<Value> = vec![
        json!({"t": ts_b, "u": uuid_b, "k": decoded["k"]}),
        json!({"t": ts_b, "u": uuid_b, "k": decoded["k"], "p": B}),
        json!({"t": decoded["t"], "u": decoded["u"], "k": decoded["k"], "project": B}),
        json!({"t": decoded["t"], "u": decoded["u"], "k": "0000000000000000"}),
        json!({"t": decoded["t"], "u": "../../p=2", "k": decoded["k"]}),
        json!({"t": "0 OR 1=1", "u": decoded["u"], "k": decoded["k"]}),
        json!({"t": -1, "u": decoded["u"], "k": decoded["k"]}),
        json!({"t": i64::MAX, "u": decoded["u"], "k": decoded["k"]}),
        json!([decoded["t"], decoded["u"], decoded["k"]]),
        json!("x"),
    ];
    for f in forged {
        let c = b64.encode(f.to_string());
        let r = app.post(KEY_A, "/q/events", with(range(), json!({"limit": 1, "cursor": c}))).await;
        assert_no_leak(&f.to_string(), &r);
        if r.status == StatusCode::OK {
            // A syntactically valid position only moves within A's own rows.
            for e in r.json()["events"].as_array().unwrap() {
                assert!(["u:alice", "a:devA2"].contains(&e["person_key"].as_str().unwrap()));
            }
        }
    }
    for junk in ["", "AAAA", "e30", "bnVsbA", "%00", "../../p=2", &cursor_a[1..], &format!("{cursor_a}A")] {
        let r = app.post(KEY_A, "/q/events", with(range(), json!({"limit": 1, "cursor": junk}))).await;
        assert_no_leak(junk, &r);
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn replay_keys_are_confined_to_the_project_prefix() {
    let store = seed().await;
    let app = app(&store).await;
    let own = format!("replay/p={A}/d={DAY1}/{SESSION_A}/000000.bin.zst");
    let r = app.get(KEY_A, &format!("/q/replay?key={own}")).await;
    assert_eq!((r.status, r.bytes.as_slice()), (StatusCode::OK, &b"chunk-of-a"[..]));

    let foreign = format!("replay/p={B}/d={DAY1}/{SESSION_B}/000000.bin.zst");
    let tail = format!("d={DAY1}/{SESSION_B}/000000.bin.zst");
    let attempts: Vec<String> = vec![
        foreign.clone(),
        format!("/{foreign}"),
        format!("./{foreign}"),
        format!("replay/p={A}/../p={B}/{tail}"),
        format!("replay/p={A}/..%2Fp={B}/{tail}"),
        format!("replay/p={A}/%2e%2e/p={B}/{tail}"),
        format!("replay/p={A}/d={DAY1}/../../p={B}/{tail}"),
        format!("replay/p={A}/d={DAY1}/{SESSION_A}/../../../p={B}/{tail}"),
        format!("replay/p={A}/d={DAY1}/{SESSION_A}/..%2F..%2F..%2Fp={B}%2F{tail}"),
        format!("replay/p={A}/d={DAY1}/{SESSION_A}/000000.bin.zst/../../../../p={B}/{tail}"),
        format!("replay/p={A}//p={B}/{tail}"),
        format!("replay/p={A}\\..\\p={B}\\{tail}"),
        format!("replay/p={A}{B}/{tail}"),
        format!("replay/p=0{B}/{tail}"),
        format!("replay/p={B}%00/{tail}"),
        format!("replay/p={A}/d={DAY1}/{SESSION_B}/000000.bin.zst"),
        format!("replay/p=*/{tail}"),
        format!("replay/{tail}"),
        format!("replay/p={A}/"),
        format!("replay/p={A}"),
        "replay/".into(),
        format!("data/p={B}/d={DAY1}/L2-b.parquet"),
        format!("data/p={A}/d={DAY1}/L2-a.parquet"),
        "control/keys.json".into(),
        "control/projects.json".into(),
        format!("persons/p={B}/_m/00000001.json"),
        format!("replay/p={A}/../../control/keys.json"),
        format!("replay/p={A}/d={DAY1}/{SESSION_A}/..%2f..%2f..%2f..%2fcontrol%2fkeys.json"),
        "/etc/passwd".into(),
        "file:///etc/passwd".into(),
        format!("la://store/{foreign}"),
        String::new(),
    ];
    for key in &attempts {
        for encoded in [key.clone(), key.replace('/', "%2F")] {
            let r = app.get(KEY_A, &format!("/q/replay?key={encoded}")).await;
            assert_no_leak(&encoded, &r);
            assert!(
                matches!(r.status, StatusCode::FORBIDDEN | StatusCode::BAD_REQUEST | StatusCode::NOT_FOUND),
                "{encoded}: {}",
                r.status
            );
            assert_ne!(r.bytes, b"chunk-of-a");
            // Control files must not come out either.
            assert!(!String::from_utf8_lossy(&r.bytes).contains("project_id"), "{encoded}");
        }
    }
    // The foreign prefix itself is a clean 403.
    assert_eq!(app.get(KEY_A, &format!("/q/replay?key={foreign}")).await.status, StatusCode::FORBIDDEN);
    // Duplicate parameters do not confuse the check.
    let r = app.get(KEY_A, &format!("/q/replay?key={own}&key={foreign}")).await;
    assert_no_leak("dup key", &r);
    let r = app.get(KEY_A, &format!("/q/replay?key={foreign}&key={own}")).await;
    assert_no_leak("dup key", &r);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn forged_internal_headers_are_rejected() {
    let store = seed().await;
    let body = with(range(), json!({"sql": "SELECT * FROM events"}));
    let req = |internal: Option<&str>, project: Option<&str>, bearer: Option<&str>| {
        let mut r = Request::post("/q/sql");
        if let Some(v) = internal {
            r = r.header("x-la-internal", v);
        }
        if let Some(v) = project {
            r = r.header("x-la-project", v);
        }
        if let Some(v) = bearer {
            r = r.header("authorization", format!("Bearer {v}"));
        }
        r.body(Body::from(body.to_string())).unwrap()
    };
    // Secret unset: nothing presented in the header can open the internal path.
    let closed = app(&store).await;
    for secret in ["", " ", "null", "None", "undefined", "0", "true", "secret", "*"] {
        let r = closed.send(req(Some(secret), Some("2"), None)).await;
        assert_eq!(r.status, StatusCode::UNAUTHORIZED, "{secret:?}");
        assert_no_leak(secret, &r);
        // …not even alongside A's valid key.
        let r = closed.send(req(Some(secret), Some("2"), Some(KEY_A))).await;
        assert_eq!(r.status, StatusCode::UNAUTHORIZED, "{secret:?}");
        assert_no_leak(secret, &r);
    }
    // The project header without the secret never overrides the key's project.
    let r = closed.send(req(None, Some("2"), Some(KEY_A))).await;
    assert_eq!(r.status, StatusCode::UNAUTHORIZED);
    assert_no_leak("project header", &r);

    // Secret set: only the exact value works.
    let cfg = la_query::config::Config { internal_secret: Some("correct horse".into()), ..test_config() };
    let open = app_with(&store, cfg).await;
    for secret in ["", "correct", "correct horse ", "Correct horse", "correct horse\0", "correct%20horse"] {
        let Ok(value) = axum::http::HeaderValue::from_str(secret) else { continue };
        let mut r = Request::post("/q/sql").header("x-la-project", "2");
        r = r.header("x-la-internal", value);
        let r = open.send(r.body(Body::from(body.to_string())).unwrap()).await;
        assert_eq!(r.status, StatusCode::UNAUTHORIZED, "{secret:?}");
        assert_no_leak(secret, &r);
    }
    let r = open.send(req(Some("wrong"), Some("2"), Some(KEY_A))).await;
    assert_eq!(r.status, StatusCode::UNAUTHORIZED);
    for project in ["2 OR 1=1", "2;1", "-2", "02", "2.0", "0x2", "", "4294967296", "1,2"] {
        let r = open.send(req(Some("correct horse"), Some(project), None)).await;
        assert_eq!(r.status, StatusCode::BAD_REQUEST, "{project:?}");
    }
    // The legitimate internal call works and is scoped to the named project.
    let r = open.send(req(Some("correct horse"), Some("1"), None)).await;
    assert_eq!(r.status, StatusCode::OK);
    assert_no_leak("internal A", &r);
    assert_eq!(r.json()["rows"].as_array().unwrap().len(), 3);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn schema_and_person_data_stay_per_project() {
    let store = seed().await;
    let app = app(&store).await;
    let r = app.post(KEY_A, "/q/schema", json!({})).await;
    assert_eq!(r.status, StatusCode::OK);
    assert_no_leak("schema", &r);
    assert_eq!(r.json()["person_props"], json!(["plan"]));
    // B's identity links and person properties do not apply to A's rows.
    let v = app
        .ok(KEY_A, "/q/trends", with(range(), json!({"series": [{"agg": "unique"}], "breakdown": "person.plan_b_only"})))
        .await;
    assert_eq!(v["series"].as_array().unwrap().len(), 1);
    assert_eq!(v["series"][0]["breakdown"], Value::Null);
    // A tombstone in B does not delete in A (and vice versa).
    tombstone(&store, B, 5, &["u:alice"], &["devA2"]).await;
    let v = app.ok(KEY_A, "/q/sql", with(range(), json!({"sql": "SELECT count(*) FROM events"}))).await;
    assert_eq!(v["rows"], json!([[3]]));
}
