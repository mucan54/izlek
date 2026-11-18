//! Benchmarks for the design's measured assumptions (§19):
//!
//! * (2) the streaming funnel operator vs. plain-SQL formulations,
//! * (4) trend queries on person-sorted files vs. a time-sorted copy,
//! * (3) the cost of query-time identity rewrite at several identify ratios.
//!
//! Data is synthetic: one L2 file per day (as after compaction), written with
//! the production writer settings.

use la_core::events::{rows_to_batch, EventRow};
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::manifest::{commit_day, FileEntry};
use la_core::parquet_io::{events_writer_props, write_events_file, write_parquet, EventsFileMeta};
use la_core::schema::{events_schema, DIM_COUNTRY, DIM_PLATFORM};
use la_core::{keys, time, Store};
use la_query::config::Config;
use la_query::service::{Kind, Service};
use rand::rngs::StdRng;
use rand::{Rng, SeedableRng};
use serde_json::{json, Value};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

#[derive(Debug, Clone)]
pub struct BenchConfig {
    pub events: u64,
    pub days: usize,
    pub persons: u64,
    pub dir: PathBuf,
    pub runs: usize,
    pub partitions: usize,
    /// Fractions of persons that identify mid-way (assumption 3).
    pub identify_ratios: Vec<f64>,
}

#[derive(Debug, Clone, Copy, PartialEq)]
enum Layout {
    /// Production layout: sorted by `(person_key, ts, uuid)`, identities resolved.
    PersonSorted,
    /// Same rows sorted by `ts` (what a time-ordered store would look like).
    TimeSorted,
    /// Person-sorted, but this fraction of persons has unresolved `a:` rows
    /// before their identify instant; the links arrive after the files.
    Identify(f64),
}

const FIRST_DAY: &str = "2026-10-01";
const EVENT_WEIGHTS: [(&str, u32); 5] = [("view", 60), ("cart", 15), ("other", 12), ("purchase", 8), ("signup", 5)];
const PLATFORMS: [&str; 3] = ["ios", "android", "web"];
const COUNTRIES: [&str; 6] = ["TR", "DE", "US", "GB", "FR", "NL"];

fn mix(mut x: u64) -> u64 {
    x ^= x >> 33;
    x = x.wrapping_mul(0xff51_afd7_ed55_8ccd);
    x ^= x >> 33;
    x = x.wrapping_mul(0xc4ce_b9fe_1a85_ec53);
    x ^ (x >> 33)
}

struct Written {
    rows: u64,
    bytes: u64,
    anon_rows: u64,
    links: u64,
}

/// Writes one project. Every layout draws the same events (same seed).
async fn write_project(store: &Store, project: u32, cfg: &BenchConfig, layout: Layout) -> Written {
    let first = time::day_start_ms(FIRST_DAY).expect("day");
    let span = cfg.days as i64 * time::DAY_MS;
    let per_day = cfg.events / cfg.days as u64;
    let ratio = match layout {
        Layout::Identify(r) => r,
        _ => 0.0,
    };
    // A person "identifies late" when its hash falls under the ratio; the
    // instant is spread over the whole range.
    let late = |p: u64| (mix(p ^ 0xabcd) % 1_000_000) < (ratio * 1_000_000.0) as u64;
    let identify_at = |p: u64| first + (mix(p ^ 0x1234) % span as u64) as i64;
    let total_weight: u32 = EVENT_WEIGHTS.iter().map(|w| w.1).sum();
    let mut out = Written { rows: 0, bytes: 0, anon_rows: 0, links: 0 };
    for d in 0..cfg.days {
        let day_start = first + d as i64 * time::DAY_MS;
        let day = time::day_of(day_start);
        let mut rng = StdRng::seed_from_u64(0xbe7c_0000 + d as u64);
        let mut rows: Vec<EventRow> = Vec::with_capacity(per_day as usize);
        for i in 0..per_day {
            // Mildly skewed activity: u^2 concentrates events on low ids.
            let u: f64 = rng.random();
            let p = ((u * u) * cfg.persons as f64) as u64;
            let ts = day_start + rng.random_range(0..time::DAY_MS);
            let mut pick = rng.random_range(0..total_weight);
            let event = EVENT_WEIGHTS.iter().find(|(_, w)| {
                if pick < *w {
                    true
                } else {
                    pick -= *w;
                    false
                }
            });
            let event = event.map_or("view", |e| e.0);
            let anon = format!("dev{p:07}");
            let identified = !(late(p) && ts < identify_at(p));
            let user = identified.then(|| format!("{p:07}"));
            let mut uuid = [0u8; 16];
            uuid[..8].copy_from_slice(&mix(d as u64 * 1_000_000_007 + i).to_be_bytes());
            uuid[8..].copy_from_slice(&(d as u64 * per_day + i).to_be_bytes());
            let props = if event == "purchase" {
                format!(r#"{{"amount":{},"path":"/p/{}"}}"#, rng.random_range(1..500), rng.random_range(0..2000))
            } else {
                format!(r#"{{"path":"/p/{}"}}"#, rng.random_range(0..2000))
            };
            let mut row = EventRow {
                uuid,
                ts,
                recv_ts: ts + 40,
                event: event.to_owned(),
                person_key: la_core::events::person_key(user.as_deref(), &anon),
                anon_id: anon,
                user_id: user,
                props,
                ..Default::default()
            };
            row.dims[DIM_PLATFORM] = Some(PLATFORMS[(mix(p) % 3) as usize].to_owned());
            row.dims[DIM_COUNTRY] = Some(COUNTRIES[(mix(p ^ 7) % 6) as usize].to_owned());
            out.anon_rows += u64::from(!identified);
            rows.push(row);
        }
        let key = keys::level_file(project, &day, 2, 1, 0);
        let entry = match layout {
            Layout::TimeSorted => {
                rows.sort_by(|a, b| (a.ts, a.uuid).cmp(&(b.ts, b.uuid)));
                let schema = events_schema(&[]);
                let batches: Vec<_> = rows.chunks(8192).map(|c| rows_to_batch(c, &[]).expect("batch")).collect();
                let bytes = write_parquet(&schema, &batches, events_writer_props(&schema, vec![]).expect("props")).expect("parquet");
                let mut people: Vec<&str> = rows.iter().map(|r| r.person_key.as_str()).collect();
                people.sort_unstable();
                let entry = FileEntry {
                    key: key.clone(),
                    level: 2,
                    rows: rows.len() as u64,
                    bytes: bytes.len() as u64,
                    min_ts: rows.first().map_or(0, |r| r.ts),
                    max_ts: rows.last().map_or(0, |r| r.ts),
                    min_person: people.first().copied().unwrap_or_default().to_owned(),
                    max_person: people.last().copied().unwrap_or_default().to_owned(),
                    identity_version: 0,
                    schema_version: 0,
                };
                store.put(&key, bytes).await.expect("put");
                entry
            }
            _ => {
                rows.sort_by(|a, b| a.sort_key_cmp(b));
                let meta = EventsFileMeta { project, day: day.clone(), level: 2, schema_version: 0, identity_version: 0 };
                let (bytes, mut entry) = write_events_file(&rows, &[], &meta).expect("events file");
                entry.key = key.clone();
                store.put(&key, bytes).await.expect("put");
                entry
            }
        };
        out.rows += entry.rows;
        out.bytes += entry.bytes;
        commit_day(store, project, &day, |m| {
            m.files = vec![entry.clone()];
            Ok(true)
        })
        .await
        .expect("manifest");
    }
    if ratio > 0.0 {
        // The links arrive after every file was written (identity_version 0).
        let candidates: Vec<Candidate> = (0..cfg.persons)
            .filter(|p| late(*p))
            .map(|p| Candidate { anon_id: format!("dev{p:07}"), user_id: format!("{p:07}"), ts: identify_at(p), uuid: [0; 16] })
            .collect();
        let mut map = IdentityMap::default();
        out.links = identity::append_delta(store, project, "1-0", 1, candidates, &mut map).await.expect("links").added as u64;
    }
    out
}

fn bench_service_config(partitions: usize) -> Config {
    Config {
        result_cache_bytes: 0,
        timeout: Duration::from_secs(900),
        memory_bytes: 6 << 30,
        partitions,
        slow_query: Duration::from_secs(3600),
        ..Config::default()
    }
}

struct Measured {
    median_ms: f64,
    min_ms: f64,
    bytes_scanned: u64,
    result: Value,
}

async fn measure(svc: &Service, project: u32, kind: Kind, body: &Value, runs: usize) -> Result<Measured, String> {
    let mut times = Vec::with_capacity(runs);
    let mut last = Value::Null;
    for _ in 0..runs.max(1) {
        let t = Instant::now();
        last = svc.query(project, kind, body).await.map_err(|e| e.to_string())?;
        times.push(t.elapsed().as_secs_f64() * 1000.0);
    }
    times.sort_by(f64::total_cmp);
    Ok(Measured {
        median_ms: times[times.len() / 2],
        min_ms: times[0],
        bytes_scanned: last["meta"]["bytes_scanned"].as_u64().unwrap_or(0),
        result: last,
    })
}

fn row(out: &mut String, name: &str, m: &Result<Measured, String>, note: &str) {
    match m {
        Ok(m) => out.push_str(&format!(
            "| {name} | {:.0} | {:.0} | {:.1} | {note} |\n",
            m.median_ms,
            m.min_ms,
            m.bytes_scanned as f64 / 1e6
        )),
        Err(e) => out.push_str(&format!("| {name} | failed | – | – | {e} |\n")),
    }
}

const HEADER: &str = "| query | median ms | min ms | MB scanned | note |\n|---|---:|---:|---:|---|\n";

/// Generates the data sets under `cfg.dir`, runs the measurements and
/// returns a Markdown report.
pub async fn run(cfg: &BenchConfig) -> String {
    let store = Store::local(&cfg.dir).expect("bench dir");
    let mut layouts: Vec<(u32, Layout)> = vec![(1, Layout::PersonSorted), (2, Layout::TimeSorted)];
    for (i, r) in cfg.identify_ratios.iter().enumerate() {
        layouts.push((10 + i as u32, Layout::Identify(*r)));
    }
    let mut report = String::new();
    report.push_str(&format!(
        "events per project: {} over {} days ({} persons), {} runs per query, {} partitions\n\n",
        cfg.events, cfg.days, cfg.persons, cfg.runs, cfg.partitions
    ));
    let mut written = Vec::new();
    for (project, layout) in &layouts {
        let t = Instant::now();
        let w = write_project(&store, *project, cfg, *layout).await;
        report.push_str(&format!(
            "- project {project} ({layout:?}): {} rows, {:.0} MB parquet, {} anonymous rows ({:.1} %), {} links, written in {:.0} s\n",
            w.rows,
            w.bytes as f64 / 1e6,
            w.anon_rows,
            100.0 * w.anon_rows as f64 / w.rows.max(1) as f64,
            w.links,
            t.elapsed().as_secs_f64()
        ));
        written.push(w);
    }
    let svc: Arc<Service> = crate::runner::service_for(&store, bench_service_config(cfg.partitions)).await;
    let first = time::day_start_ms(FIRST_DAY).expect("day");
    let last_day = time::day_of(first + (cfg.days as i64 - 1) * time::DAY_MS);
    let range = json!({"from": FIRST_DAY, "to": last_day});
    let with = |extra: Value| {
        let mut b = range.clone();
        for (k, v) in extra.as_object().expect("object") {
            b[k] = v.clone();
        }
        b
    };
    let week = 7 * 86_400;

    // Warm the footer cache and the page cache so every variant is measured hot.
    for (project, _) in &layouts {
        let _ = svc.query(*project, Kind::Sql, &with(json!({"sql": "SELECT count(*), count(DISTINCT event) FROM events"}))).await;
    }

    // ---- (2) funnel operator vs plain SQL -----------------------------------
    report.push_str("\n### (2) Funnel: operator vs plain SQL (person-sorted project)\n\n");
    report.push_str(HEADER);
    let funnel = with(json!({"steps": [{"event": "signup"}, {"event": "cart"}, {"event": "purchase"}], "window_seconds": week}));
    let op = measure(&svc, 1, Kind::Funnel, &funnel, cfg.runs).await;
    let op_counts: Vec<u64> = op
        .as_ref()
        .map(|m| m.result["steps"].as_array().map(|a| a.iter().map(|s| s["count"].as_u64().unwrap_or(0)).collect()).unwrap_or_default())
        .unwrap_or_default();
    row(&mut report, "funnel operator, 3 steps, 7 d window", &op, &format!("counts {op_counts:?}"));
    let with_bd = with(json!({"steps": [{"event": "signup"}, {"event": "cart"}, {"event": "purchase"}], "window_seconds": week, "breakdown": "platform"}));
    row(&mut report, "funnel operator + breakdown by platform", &measure(&svc, 1, Kind::Funnel, &with_bd, cfg.runs).await, "");
    let strict = with(json!({"steps": [{"event": "signup"}, {"event": "cart"}], "window_seconds": week, "order": "strict"}));
    row(&mut report, "funnel operator, strict, 2 steps (reads every row)", &measure(&svc, 1, Kind::Funnel, &strict, cfg.runs).await, "");
    // Exact semantics in SQL: chains via self-joins on the person.
    let exact = "SELECT count(DISTINCT e1.person_key), count(DISTINCT e2.person_key), count(DISTINCT e3.person_key) \
                 FROM events e1 \
                 LEFT JOIN events e2 ON e2.person_key = e1.person_key AND e2.event = 'cart' AND e2.ts > e1.ts AND e2.ts <= e1.ts + INTERVAL '7 days' \
                 LEFT JOIN events e3 ON e3.person_key = e2.person_key AND e3.event = 'purchase' AND e3.ts > e2.ts AND e3.ts <= e1.ts + INTERVAL '7 days' \
                 WHERE e1.event = 'signup'";
    let sql_exact = measure(&svc, 1, Kind::Sql, &with(json!({"sql": exact})), cfg.runs.min(2)).await;
    let note = sql_exact.as_ref().map(|m| format!("counts {}", m.result["rows"][0])).unwrap_or_default();
    row(&mut report, "plain SQL, exact semantics (self-joins)", &sql_exact, &note);
    // The usual cheaper approximation: only the first signup starts a chain.
    let first_touch = "WITH a AS (SELECT person_key, min(ts) AS t1 FROM events WHERE event = 'signup' GROUP BY 1), \
                       b AS (SELECT a.person_key, a.t1, min(e.ts) AS t2 FROM a JOIN events e ON e.person_key = a.person_key \
                             AND e.event = 'cart' AND e.ts > a.t1 AND e.ts <= a.t1 + INTERVAL '7 days' GROUP BY 1, 2), \
                       c AS (SELECT b.person_key, min(e.ts) AS t3 FROM b JOIN events e ON e.person_key = b.person_key \
                             AND e.event = 'purchase' AND e.ts > b.t2 AND e.ts <= b.t1 + INTERVAL '7 days' GROUP BY 1) \
                       SELECT (SELECT count(*) FROM a), (SELECT count(*) FROM b), (SELECT count(*) FROM c)";
    let sql_first = measure(&svc, 1, Kind::Sql, &with(json!({"sql": first_touch})), cfg.runs).await;
    let note = sql_first.as_ref().map(|m| format!("first-signup-only approximation; counts {}", m.result["rows"][0])).unwrap_or_default();
    row(&mut report, "plain SQL, first-touch approximation (3 aggregations + 2 joins)", &sql_first, &note);

    // ---- (4) trends on person-sorted vs time-sorted files --------------------
    report.push_str("\n### (4) Trends: person-sorted vs time-sorted files\n\n");
    report.push_str(HEADER);
    let hour_from = first + 3 * time::DAY_MS + 10 * 3_600_000;
    let iso = |ms: i64| chrono::TimeZone::timestamp_millis_opt(&chrono::Utc, ms).single().expect("ts").to_rfc3339();
    let trend_cases: Vec<(&str, Value)> = vec![
        ("daily count of `purchase`", with(json!({"interval": "day", "series": [{"event": "purchase", "agg": "count"}]}))),
        ("daily count of all events by platform", with(json!({"interval": "day", "series": [{"agg": "count"}], "breakdown": "platform"}))),
        ("daily sum(props.amount) of `purchase`", with(json!({"interval": "day", "series": [{"event": "purchase", "agg": "sum", "prop": "props.amount"}]}))),
        (
            "hourly count, 2-hour window inside one day",
            json!({"from": iso(hour_from), "to": iso(hour_from + 2 * 3_600_000), "interval": "hour", "series": [{"agg": "count"}]}),
        ),
    ];
    for (name, body) in &trend_cases {
        row(&mut report, &format!("{name} — person-sorted"), &measure(&svc, 1, Kind::Trends, body, cfg.runs).await, "");
        row(&mut report, &format!("{name} — time-sorted"), &measure(&svc, 2, Kind::Trends, body, cfg.runs).await, "");
    }
    let unique_day = with(json!({"interval": "day", "series": [{"agg": "unique"}]}));
    row(&mut report, "daily unique persons — person-sorted (streaming, exact)", &measure(&svc, 1, Kind::Trends, &unique_day, cfg.runs).await, "");
    let unique_week = with(json!({"interval": "week", "series": [{"agg": "unique"}]}));
    row(&mut report, "weekly unique persons — person-sorted (merged stream)", &measure(&svc, 1, Kind::Trends, &unique_week, cfg.runs).await, "");
    let sql_unique = with(json!({"sql": "SELECT date_trunc('day', ts), count(DISTINCT person_key) FROM events GROUP BY 1"}));
    row(&mut report, "daily unique persons — SQL count(DISTINCT) on the same files", &measure(&svc, 1, Kind::Sql, &sql_unique, cfg.runs).await, "hash-based");

    // ---- (3) identity rewrite -------------------------------------------------
    report.push_str("\n### (3) Query-time identity rewrite\n\n");
    report.push_str(HEADER);
    let count_all = with(json!({"interval": "day", "series": [{"agg": "count"}]}));
    let mut variants: Vec<(u32, String)> = vec![(1, "no pending links".to_owned())];
    for (i, r) in cfg.identify_ratios.iter().enumerate() {
        let w = &written[2 + i];
        variants.push((
            10 + i as u32,
            format!("{:.0} % of persons linked late, {:.1} % of rows rewritten", r * 100.0, 100.0 * w.anon_rows as f64 / w.rows.max(1) as f64),
        ));
    }
    for (name, kind, body) in [
        ("funnel 3 steps", Kind::Funnel, &funnel),
        ("daily unique persons", Kind::Trends, &unique_day),
        ("daily count (order-insensitive)", Kind::Trends, &count_all),
    ] {
        for (project, label) in &variants {
            row(&mut report, &format!("{name} — {label}"), &measure(&svc, *project, kind, body, cfg.runs).await, "");
        }
    }
    report
}
