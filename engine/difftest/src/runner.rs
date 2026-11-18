//! Differential runner: random requests against the real service, compared
//! with the references. Any mismatch panics with the seed and the request.

use crate::gen::{generate, Dataset, GenConfig, EVENTS, WINDOWS};
use crate::model::{self, hex, PVal, Row};
use crate::reference::{self, Columns, TrendCell};
use chrono::{Datelike, TimeZone, Utc};
use la_core::control::{sha256_hex, Keys, QueryKey};
use la_core::{keys, time, Store};
use la_query::config::Config;
use la_query::service::{Kind, Service};
use la_query::state::Engine;
use rand::rngs::StdRng;
use rand::{Rng, SeedableRng};
use serde_json::{json, Value};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

#[derive(Debug, Default, Clone, Copy)]
pub struct Tally {
    pub seeds: u64,
    pub funnels: u64,
    pub retentions: u64,
    pub trends: u64,
    pub event_walks: u64,
    pub scans: u64,
    pub logical_events: u64,
    pub physical_rows: u64,
    pub files: u64,
    pub duplicate_copies: u64,
    pub late_events: u64,
    pub identity_links: u64,
    pub anonymous_rows_at_write: u64,
    pub tombstoned_rows: u64,
    pub compacted_days: u64,
}

impl Tally {
    pub fn add(&mut self, o: &Tally) {
        self.seeds += o.seeds;
        self.funnels += o.funnels;
        self.retentions += o.retentions;
        self.trends += o.trends;
        self.event_walks += o.event_walks;
        self.scans += o.scans;
        self.logical_events += o.logical_events;
        self.physical_rows += o.physical_rows;
        self.files += o.files;
        self.duplicate_copies += o.duplicate_copies;
        self.late_events += o.late_events;
        self.identity_links += o.identity_links;
        self.anonymous_rows_at_write += o.anonymous_rows_at_write;
        self.tombstoned_rows += o.tombstoned_rows;
        self.compacted_days += o.compacted_days;
    }
}

pub fn service_config() -> Config {
    Config {
        manifest_ttl: Duration::ZERO,
        tombstone_ttl: Duration::ZERO,
        result_cache_bytes: 0,
        partitions: 3,
        ..Config::default()
    }
}

pub async fn service_for(store: &Store, cfg: Config) -> Arc<Service> {
    let engine = Engine::new(store.clone(), cfg).expect("engine");
    engine.refresh_control().await.expect("control");
    Service::new(engine)
}

pub async fn register_key(store: &Store, secret: &str, project: u32) {
    let mut k = Keys::load(store).await.expect("keys");
    k.keys.insert(sha256_hex(secret), QueryKey { project_id: project, scopes: vec!["query".into()] });
    store.put_json(keys::KEYS, &k).await.expect("keys");
}

fn pick<'a, T>(rng: &mut StdRng, items: &'a [T]) -> &'a T {
    &items[rng.random_range(0..items.len())]
}

fn iso_ms(ms: i64) -> String {
    Utc.timestamp_millis_opt(ms).single().unwrap().format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string()
}

/// A random `[from, to)` inside (or equal to) the data range, as request
/// strings plus the effective instants.
fn random_range(rng: &mut StdRng, ds: &Dataset) -> (Value, Value, i64, i64) {
    match rng.random_range(0..10) {
        // Whole range as days (`to` inclusive).
        0..=4 => (json!(ds.days[0]), json!(ds.days[ds.days.len() - 1]), ds.from_ms, ds.to_ms),
        // A sub-range of whole days.
        5..=6 => {
            let a = rng.random_range(0..ds.days.len());
            let b = rng.random_range(a..ds.days.len());
            (json!(ds.days[a]), json!(ds.days[b]), ds.from_ms + a as i64 * time::DAY_MS, ds.from_ms + (b as i64 + 1) * time::DAY_MS)
        }
        // Arbitrary instants (`to` exclusive), often on an event's timestamp.
        _ => {
            let edge = |rng: &mut StdRng| match (rng.random_bool(0.6), ds.rows.is_empty()) {
                (true, false) => ds.rows[rng.random_range(0..ds.rows.len())].ts + *pick(rng, &[0i64, 0, 1]),
                _ => rng.random_range(ds.from_ms..ds.to_ms),
            };
            let (a, b) = (edge(rng), edge(rng));
            let (a, b) = (a.min(b), a.max(b) + 1);
            (json!(iso_ms(a)), json!(iso_ms(b)), a, b)
        }
    }
}

fn random_leaf(rng: &mut StdRng, ds: &Dataset) -> Value {
    let row = (!ds.rows.is_empty()).then(|| &ds.rows[rng.random_range(0..ds.rows.len())]);
    match rng.random_range(0..26) {
        0 => json!({"prop": "event", "op": "eq", "value": pick(rng, &EVENTS)}),
        1 => json!({"prop": "event", "op": "in", "value": [pick(rng, &EVENTS), pick(rng, &EVENTS)]}),
        2 => json!({"prop": "event", "op": "ne", "value": pick(rng, &EVENTS)}),
        3 => json!({"prop": "platform", "op": "eq", "value": pick(rng, &["ios", "web", "android", "x"])}),
        4 => json!({"prop": "platform", "op": pick(rng, &["is_set", "ne", "not_in", "in"]), "value": ["ios", "web"]}).pipe(fix_list),
        5 => json!({"prop": "country", "op": pick(rng, &["eq", "ne", "contains"]), "value": pick(rng, &["TR", "DE", "T", ""])}),
        6 => json!({"prop": "props.amount", "op": pick(rng, &["gt", "gte", "lt", "lte"]), "value": rng.random_range(0..40)}),
        7 => json!({"prop": "props.amount", "op": pick(rng, &["gt", "lte"]), "value": pick(rng, &["10", "7.5", "x", "1e3"])}),
        8 => json!({"prop": "props.amount", "op": pick(rng, &["eq", "ne"]), "value": rng.random_range(0..40)}),
        9 => json!({"prop": "props.amount", "op": "eq", "value": pick(rng, &[json!(7.5), json!("7.5"), json!("12"), json!(true), json!("oops"), json!(12.0)])}),
        10 => json!({"prop": "props.amount", "op": pick(rng, &["in", "not_in"]), "value": [3, "4", 5.5, true, "oops", null]}),
        11 => json!({"prop": "props.plan", "op": pick(rng, &["eq", "ne"]), "value": pick(rng, &[json!("pro"), json!("free"), json!(3), json!("3")])}),
        12 => json!({"prop": "props.plan", "op": pick(rng, &["in", "not_in"]), "value": ["pro", "team", 3]}),
        13 => json!({"prop": "props.plan", "op": pick(rng, &["contains", "is_set"]), "value": pick(rng, &["e", "pr", "3"])}),
        14 => json!({"prop": "props.n", "op": pick(rng, &["eq", "gte", "lt", "ne"]), "value": pick(rng, &[json!(3), json!("3"), json!(2.0)])}),
        15 => json!({"prop": "props.flag", "op": pick(rng, &["eq", "ne", "is_set"]), "value": pick(rng, &[json!(true), json!("true"), json!(false), json!(1)])}),
        16 => json!({"prop": "props.missing", "op": pick(rng, &["is_set", "ne", "eq", "not_in", "gt"]), "value": [1]}).pipe(fix_list),
        17 => json!({"prop": "props.we\"ird key", "op": pick(rng, &["is_set", "contains"]), "value": "uo\\te"}),
        18 => json!({"prop": "person.tier", "op": pick(rng, &["eq", "ne"]), "value": pick(rng, &[json!("gold"), json!("silver"), json!(7), json!("7")])}),
        19 => json!({"prop": "person.tier", "op": pick(rng, &["is_set", "in", "not_in"]), "value": ["gold", 7]}).pipe(fix_list),
        20 => json!({"prop": "person.score", "op": pick(rng, &["gt", "lte", "eq"]), "value": rng.random_range(0..10)}),
        21 => json!({"prop": "user_id", "op": pick(rng, &["is_set", "eq", "ne"]), "value": row.and_then(|r| r.user_id.clone()).unwrap_or_else(|| "nobody".into())}),
        22 => json!({"prop": "person_key", "op": pick(rng, &["eq", "ne", "contains"]), "value": row.map_or("u:none".into(), |r| r.person_key.clone())}),
        23 => json!({"prop": "anon_id", "op": pick(rng, &["eq", "in"]), "value": [row.map_or("none".into(), |r| r.anon_id.clone()), "anon-01"]}).pipe(fix_list),
        24 => json!({"prop": "session_id", "op": pick(rng, &["is_set", "eq", "ne", "in"]), "value": [row.and_then(|r| r.session_id).map_or("00".repeat(16), |s| hex(&s)), "zz"]}).pipe(fix_list),
        _ => json!({"prop": pick(rng, &["os", "screen", "app_version"]), "op": pick(rng, &["is_set", "ne", "eq"]), "value": "x"}),
    }
}

trait Pipe: Sized {
    fn pipe(self, f: impl FnOnce(Self) -> Self) -> Self {
        f(self)
    }
}
impl Pipe for Value {}

/// Scalar operators take the first element of a list value.
fn fix_list(mut leaf: Value) -> Value {
    let op = leaf["op"].as_str().unwrap().to_owned();
    if !matches!(op.as_str(), "in" | "not_in") {
        if let Some(first) = leaf["value"].as_array().and_then(|a| a.first().cloned()) {
            leaf["value"] = first;
        }
    }
    leaf
}

fn random_filter(rng: &mut StdRng, ds: &Dataset, depth: usize) -> Value {
    if depth == 0 || rng.random_bool(0.55) {
        return random_leaf(rng, ds);
    }
    match rng.random_range(0..3) {
        0 => json!({"and": (0..rng.random_range(0..3)).map(|_| random_filter(rng, ds, depth - 1)).collect::<Vec<_>>()}),
        1 => json!({"or": (0..rng.random_range(0..3)).map(|_| random_filter(rng, ds, depth - 1)).collect::<Vec<_>>()}),
        _ => json!({"not": random_filter(rng, ds, depth - 1)}),
    }
}

fn maybe_filter(rng: &mut StdRng, ds: &Dataset, p: f64) -> Option<Value> {
    rng.random_bool(p).then(|| random_filter(rng, ds, 2))
}

fn random_selector(rng: &mut StdRng, ds: &Dataset, null_event: f64) -> Value {
    let mut sel = json!({"event": if rng.random_bool(null_event) { Value::Null } else { json!(pick(rng, &EVENTS)) }});
    if let Some(f) = maybe_filter(rng, ds, 0.25) {
        sel["filter"] = f;
    }
    sel
}

fn random_breakdown(rng: &mut StdRng) -> Option<&'static str> {
    rng.random_bool(0.5).then(|| *pick(rng, &["platform", "country", "props.plan", "props.amount", "person.tier", "event", "props.flag", "props.missing"]))
}

struct Case<'a> {
    seed: u64,
    kind: &'static str,
    request: &'a Value,
}

impl Case<'_> {
    fn fail(&self, what: &str, expected: &dyn std::fmt::Debug, actual: &dyn std::fmt::Debug) -> ! {
        panic!(
            "DIFFERENTIAL MISMATCH (seed {}, {}): {what}\n  request: {}\n  expected: {expected:?}\n  actual:   {actual:?}",
            self.seed, self.kind, self.request
        );
    }
    fn check<T: PartialEq + std::fmt::Debug>(&self, what: &str, expected: &T, actual: &T) {
        if expected != actual {
            self.fail(what, expected, actual);
        }
    }
}

fn in_range(ds: &Dataset, from: i64, to: i64) -> Vec<Row> {
    ds.rows.iter().filter(|r| r.ts >= from && r.ts < to).cloned().collect()
}

async fn run(svc: &Service, project: u32, kind: Kind, body: &Value, seed: u64) -> Value {
    svc.query(project, kind, body).await.unwrap_or_else(|e| panic!("seed {seed}: {} failed: {e}\n  request: {body}", kind.name()))
}

fn close(a: f64, b: f64) -> bool {
    (a - b).abs() <= 1e-9 * (1.0 + a.abs().max(b.abs()))
}

// ---------------------------------------------------------------------------
// Funnel
// ---------------------------------------------------------------------------

async fn check_funnel(svc: &Service, ds: &Dataset, rng: &mut StdRng, seed: u64, order: &str, with_breakdown: bool) {
    let (from, to, from_ms, to_ms) = random_range(rng, ds);
    let n_steps = rng.random_range(2..=4);
    let steps: Vec<Value> = (0..n_steps).map(|_| random_selector(rng, ds, 0.1)).collect();
    let window_s = match rng.random_range(0..10) {
        0..=6 => *pick(rng, &WINDOWS),
        7 => 7 * 86_400,
        _ => rng.random_range(1..200_000),
    };
    let breakdown = if with_breakdown { Some(random_breakdown(rng).unwrap_or("platform")) } else { None };
    let filter = maybe_filter(rng, ds, 0.3);
    let mut body = json!({"from": from, "to": to, "steps": steps, "window_seconds": window_s, "order": order});
    if let Some(b) = breakdown {
        body["breakdown"] = json!(b);
    }
    if let Some(f) = &filter {
        body["filter"] = f.clone();
    }
    let case = Case { seed, kind: "funnel", request: &body };
    let actual = run(svc, ds.project, Kind::Funnel, &body, seed).await;

    let rows = in_range(ds, from_ms, to_ms);
    let g: Vec<bool> = rows.iter().map(|r| model::passes(filter.as_ref(), r, &ds.persons)).collect();
    let names: Vec<String> = (0..n_steps).map(|j| format!("s{j}")).collect();
    let selectors: Vec<(&str, &Value)> = names.iter().map(String::as_str).zip(steps.iter()).collect();
    let flags = reference::flags_for(&rows, &ds.persons, &selectors);
    let bd: Option<Vec<Option<String>>> = breakdown.map(|b| rows.iter().map(|r| model::text_of(r, &ds.persons, b)).collect());

    // Reference 1: plain SQL (counts).
    let ctx = reference::context(&rows, Columns { g: g.clone(), flags: flags.clone(), bd: bd.clone(), nums: vec![] }).await;
    let window_ms = window_s * 1000;
    let sql_total = reference::funnel_sql(&ctx, n_steps, window_ms, order, false).await;
    let sql_total = sql_total.get(&None).cloned().unwrap_or_else(|| vec![0; n_steps]);
    // Reference 2: brute force (counts again, plus durations).
    let kept: Vec<usize> = (0..rows.len()).filter(|i| g[*i]).collect();
    let kept_rows: Vec<&Row> = kept.iter().map(|i| &rows[*i]).collect();
    let masks: Vec<u32> = kept.iter().map(|i| (0..n_steps).fold(0u32, |m, j| m | (u32::from(flags[j].1[*i]) << j))).collect();
    let kept_bd: Option<Vec<Option<String>>> = bd.as_ref().map(|b| kept.iter().map(|i| b[*i].clone()).collect());
    let (brute_total, brute_groups) = reference::brute_funnel(&kept_rows, &masks, kept_bd.as_deref(), n_steps, window_ms, order);
    case.check("SQL reference vs brute force (total counts)", &sql_total, &brute_total.counts);

    let read = |steps_json: &Value| -> (Vec<u64>, Vec<Option<f64>>) {
        let arr = steps_json.as_array().expect("steps array");
        (arr.iter().map(|s| s["count"].as_u64().unwrap()).collect(), arr.iter().map(|s| s["median_seconds"].as_f64()).collect())
    };
    let (counts, medians) = read(&actual["steps"]);
    case.check("total step counts", &sql_total, &counts);
    for (i, s) in steps.iter().enumerate() {
        case.check("step event echo", &s["event"], &actual["steps"][i]["event"]);
    }
    let expect_medians = |b: &reference::BruteFunnel| -> Vec<Option<f64>> {
        b.durations.iter().enumerate().map(|(k, d)| if k == 0 { None } else { reference::exact_median(d) }).collect()
    };
    let medians_match = |want: &[Option<f64>], got: &[Option<f64>]| {
        want.iter().zip(got).all(|(w, g)| match (w, g) {
            (None, None) => true,
            (Some(a), Some(b)) => close(*a, *b),
            _ => false,
        })
    };
    let want = expect_medians(&brute_total);
    if !medians_match(&want, &medians) {
        case.fail("total medians", &want, &medians);
    }

    match (breakdown, actual.get("breakdowns")) {
        (None, None) => {}
        (Some(_), Some(Value::Array(groups))) => {
            let sql_groups = reference::funnel_sql(&ctx, n_steps, window_ms, order, true).await;
            let brute_counts: BTreeMap<Option<String>, Vec<u64>> = brute_groups.iter().map(|(k, v)| (k.clone(), v.counts.clone())).collect();
            case.check("SQL reference vs brute force (breakdown counts)", &sql_groups, &brute_counts);
            let mut got: BTreeMap<Option<String>, Vec<u64>> = BTreeMap::new();
            let mut last_count = u64::MAX;
            for grp in groups {
                let key = grp["value"].as_str().map(str::to_owned);
                let (c, m) = read(&grp["steps"]);
                if c[0] > last_count {
                    case.fail("breakdowns are sorted by step-1 count", &last_count, &c[0]);
                }
                last_count = c[0];
                let want = expect_medians(&brute_groups[&key]);
                if !medians_match(&want, &m) {
                    case.fail(&format!("medians of breakdown {key:?}"), &want, &m);
                }
                if got.insert(key.clone(), c).is_some() {
                    case.fail("duplicate breakdown value", &"unique", &key);
                }
            }
            case.check("breakdown step counts", &sql_groups, &got);
        }
        (b, got) => case.fail("presence of `breakdowns`", &b, &got),
    }
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

fn period_start(ms: i64, period: &str) -> i64 {
    let d = Utc.timestamp_millis_opt(ms).single().unwrap().date_naive();
    let start = match period {
        "day" => d,
        "week" => d - chrono::Duration::days(d.weekday().num_days_from_monday() as i64),
        _ => d.with_day(1).unwrap(),
    };
    Utc.from_utc_datetime(&start.and_hms_opt(0, 0, 0).unwrap()).timestamp_millis()
}

fn next_period(start: i64, period: &str) -> i64 {
    let d = Utc.timestamp_millis_opt(start).single().unwrap().date_naive();
    let next = match period {
        "day" => d + chrono::Duration::days(1),
        "week" => d + chrono::Duration::days(7),
        _ => {
            if d.month() == 12 {
                d.with_year(d.year() + 1).unwrap().with_month(1).unwrap()
            } else {
                d.with_month(d.month() + 1).unwrap()
            }
        }
    };
    Utc.from_utc_datetime(&next.and_hms_opt(0, 0, 0).unwrap()).timestamp_millis()
}

fn hour_start(ms: i64) -> i64 {
    ms.div_euclid(3_600_000) * 3_600_000
}

/// Period starts covering `[from, to)`, computed with calendar arithmetic.
fn period_list(from: i64, to: i64, period: &str) -> Vec<i64> {
    let mut out = Vec::new();
    let mut p = if period == "hour" { hour_start(from) } else { period_start(from, period) };
    while p < to {
        out.push(p);
        p = if period == "hour" { p + 3_600_000 } else { next_period(p, period) };
    }
    out
}

fn iso_s(ms: i64) -> String {
    Utc.timestamp_millis_opt(ms).single().unwrap().format("%Y-%m-%dT%H:%M:%SZ").to_string()
}

async fn check_retention(svc: &Service, ds: &Dataset, rng: &mut StdRng, seed: u64) {
    let (from, to, from_ms, to_ms) = random_range(rng, ds);
    let period = *pick(rng, &["day", "day", "week", "month"]);
    let periods = rng.random_range(1..=8usize);
    let start = random_selector(rng, ds, 0.15);
    let ret = if rng.random_bool(0.3) { start.clone() } else { random_selector(rng, ds, 0.5) };
    let filter = maybe_filter(rng, ds, 0.3);
    let mut body = json!({"from": from, "to": to, "period": period, "periods": periods, "start": start, "return": ret});
    if let Some(f) = &filter {
        body["filter"] = f.clone();
    }
    let case = Case { seed, kind: "retention", request: &body };
    let actual = run(svc, ds.project, Kind::Retention, &body, seed).await;

    let rows = in_range(ds, from_ms, to_ms);
    let g = rows.iter().map(|r| model::passes(filter.as_ref(), r, &ds.persons)).collect();
    let flags = reference::flags_for(&rows, &ds.persons, &[("is_start", &start), ("is_return", &ret)]);
    let ctx = reference::context(&rows, Columns { g, flags, bd: None, nums: vec![] }).await;
    let expected = reference::retention_sql(&ctx, period).await;

    let starts = period_list(from_ms, to_ms, period);
    let cohorts = actual["cohorts"].as_array().expect("cohorts");
    case.check("number of cohorts", &starts.len(), &cohorts.len());
    for (c, cohort) in cohorts.iter().enumerate() {
        case.check("cohort period_start", &json!(iso_s(starts[c])), &cohort["period_start"]);
        let (size, returns) = expected.get(&starts[c]).cloned().unwrap_or_default();
        case.check(&format!("size of cohort {c}"), &size, &cohort["size"].as_u64().unwrap());
        // Row length: `periods`, shortened by the periods past `to`.
        let want: Vec<u64> =
            (0..periods.min(starts.len() - c)).map(|k| returns.get(&starts[c + k]).copied().unwrap_or(0)).collect();
        let got: Vec<u64> = cohort["returned"].as_array().unwrap().iter().map(|v| v.as_u64().unwrap()).collect();
        case.check(&format!("returned of cohort {c}"), &want, &got);
    }
    // Nothing in the reference falls outside the listed cohorts.
    for c in expected.keys() {
        if !starts.contains(c) {
            case.fail("reference cohort outside the range", c, &starts);
        }
    }
}

// ---------------------------------------------------------------------------
// Trends
// ---------------------------------------------------------------------------

async fn check_trends(svc: &Service, ds: &Dataset, rng: &mut StdRng, seed: u64, force_unique: bool) {
    let (from, to, from_ms, to_ms) = random_range(rng, ds);
    let interval = *pick(rng, &["hour", "day", "day", "week", "month"]);
    let n_series = rng.random_range(1..=4usize);
    let mut series: Vec<Value> = Vec::new();
    for i in 0..n_series {
        let agg = if force_unique && i == 0 { "unique" } else { *pick(rng, &["count", "unique", "unique", "sum", "avg", "min", "max"]) };
        let mut s = random_selector(rng, ds, 0.4);
        s["agg"] = json!(agg);
        if matches!(agg, "sum" | "avg" | "min" | "max") {
            s["prop"] = json!(pick(rng, &["props.amount", "props.n", "person.score", "props.plan"]));
        }
        if rng.random_bool(0.3) {
            s["label"] = json!(format!("series {i}"));
        }
        series.push(s);
    }
    let breakdown = random_breakdown(rng);
    let limit = if rng.random_bool(0.3) { rng.random_range(1..4usize) } else { 1000 };
    let filter = maybe_filter(rng, ds, 0.3);
    let mut body = json!({"from": from, "to": to, "interval": interval, "series": series, "breakdown_limit": limit});
    if let Some(b) = breakdown {
        body["breakdown"] = json!(b);
    }
    if let Some(f) = &filter {
        body["filter"] = f.clone();
    }
    let case = Case { seed, kind: "trends", request: &body };
    let actual = run(svc, ds.project, Kind::Trends, &body, seed).await;

    let starts = period_list(from_ms, to_ms, interval);
    let want_buckets: Vec<Value> = starts.iter().map(|s| json!(iso_s(*s))).collect();
    case.check("buckets", &Value::Array(want_buckets), &actual["buckets"]);

    let rows = in_range(ds, from_ms, to_ms);
    let g = rows.iter().map(|r| model::passes(filter.as_ref(), r, &ds.persons)).collect();
    let names: Vec<String> = (0..n_series).map(|i| format!("m{i}")).collect();
    let selectors: Vec<(&str, &Value)> = names.iter().map(String::as_str).zip(series.iter()).collect();
    let flags = reference::flags_for(&rows, &ds.persons, &selectors);
    let nums = (0..n_series)
        .map(|i| {
            let values = match series[i].get("prop").and_then(Value::as_str) {
                Some(p) => rows.iter().map(|r| model::number_of(r, &ds.persons, p)).collect(),
                None => vec![None; rows.len()],
            };
            (format!("v{i}"), values)
        })
        .collect();
    let bd = breakdown.map(|b| rows.iter().map(|r| model::text_of(r, &ds.persons, b)).collect());
    let ctx = reference::context(&rows, Columns { g, flags, bd, nums }).await;

    let out = actual["series"].as_array().expect("series");
    for (i, s) in series.iter().enumerate() {
        let agg = s["agg"].as_str().unwrap();
        let cells = reference::trends_sql(&ctx, i, interval, breakdown.is_some()).await;
        let value_of = |c: Option<&TrendCell>| -> Option<f64> {
            match agg {
                "count" => Some(c.map_or(0.0, |c| c.n as f64)),
                "unique" => Some(c.map_or(0.0, |c| c.unique as f64)),
                "sum" => Some(c.and_then(|c| c.sum).unwrap_or(0.0)),
                "avg" => c.and_then(|c| c.avg),
                "min" => c.and_then(|c| c.min),
                _ => c.and_then(|c| c.max),
            }
        };
        let mine: Vec<&Value> = out.iter().filter(|o| o["index"] == json!(i)).collect();
        let read = |o: &Value| -> Vec<Option<f64>> { o["values"].as_array().unwrap().iter().map(Value::as_f64).collect() };
        let same = |want: &[Option<f64>], got: &[Option<f64>]| {
            want.len() == got.len()
                && want.iter().zip(got).all(|(w, g)| match (w, g) {
                    (None, None) => true,
                    (Some(a), Some(b)) => close(*a, *b),
                    _ => false,
                })
        };
        for o in &mine {
            case.check("series label", &s.get("label").cloned().unwrap_or(Value::Null), &o["label"]);
        }
        if breakdown.is_none() {
            if mine.len() != 1 || mine[0].get("breakdown").is_some() {
                case.fail("exactly one un-broken-down entry per series", &1, &mine);
            }
            let want: Vec<Option<f64>> = starts.iter().map(|b| value_of(cells.get(&(None, *b)))).collect();
            if !same(&want, &read(mine[0])) {
                case.fail(&format!("series {i} ({agg})"), &want, &read(mine[0]));
            }
            continue;
        }
        // Expected ranking: by matching rows (by total sum for `sum`), ties by
        // value, null bucket last among ties.
        let mut by_value: BTreeMap<Option<String>, Vec<(i64, TrendCell)>> = BTreeMap::new();
        for ((v, b), c) in &cells {
            by_value.entry(v.clone()).or_default().push((*b, *c));
        }
        let mut ranked: Vec<(f64, Option<String>)> = by_value
            .iter()
            .map(|(v, cs)| {
                let total = if agg == "sum" { cs.iter().map(|(_, c)| c.sum.unwrap_or(0.0)).sum() } else { cs.iter().map(|(_, c)| c.n as f64).sum() };
                (total, v.clone())
            })
            .collect();
        ranked.sort_by(|a, b| {
            b.0.total_cmp(&a.0).then_with(|| match (&a.1, &b.1) {
                (Some(x), Some(y)) => x.cmp(y),
                (None, Some(_)) => std::cmp::Ordering::Greater,
                (Some(_), None) => std::cmp::Ordering::Less,
                _ => std::cmp::Ordering::Equal,
            })
        });
        // Float sums can tie-break differently by rounding; only compare the
        // ranking when the totals at the cut are clearly apart.
        let clear_cut = ranked.len() <= limit || !close(ranked[limit - 1].0, ranked[limit].0);
        let top: Vec<Option<String>> = ranked.iter().take(limit).map(|(_, v)| v.clone()).collect();
        let rest: BTreeSet<Option<String>> = ranked.iter().skip(limit).map(|(_, v)| v.clone()).collect();
        let mut seen: BTreeSet<Option<String>> = BTreeSet::new();
        let mut other: Option<Vec<Option<f64>>> = None;
        for o in &mine {
            if o["breakdown"] == json!("$other") && !by_value.contains_key(&Some("$other".to_owned())) {
                other = Some(read(o));
                continue;
            }
            let key = o["breakdown"].as_str().map(str::to_owned);
            let want: Vec<Option<f64>> = starts.iter().map(|b| value_of(cells.get(&(key.clone(), *b)))).collect();
            if !by_value.contains_key(&key) {
                case.fail(&format!("series {i}: unexpected breakdown value"), &by_value.keys().collect::<Vec<_>>(), &key);
            }
            if !same(&want, &read(o)) {
                case.fail(&format!("series {i} ({agg}) breakdown {key:?}"), &want, &read(o));
            }
            seen.insert(key);
        }
        if clear_cut {
            let got: Vec<Option<String>> = mine.iter().filter(|o| o["breakdown"] != json!("$other")).map(|o| o["breakdown"].as_str().map(str::to_owned)).collect();
            let got_set: BTreeSet<_> = got.iter().cloned().collect();
            let top_set: BTreeSet<_> = top.iter().cloned().collect();
            case.check(&format!("series {i}: top breakdown values"), &top_set, &got_set);
            // `$other` exists iff values were cut off and the series is additive.
            let want_other = !rest.is_empty() && agg != "unique";
            case.check(&format!("series {i}: presence of $other"), &want_other, &other.is_some());
            if let Some(got_other) = other {
                let want: Vec<Option<f64>> = starts
                    .iter()
                    .map(|b| {
                        let cs: Vec<&TrendCell> = rest.iter().filter_map(|v| cells.get(&(v.clone(), *b))).collect();
                        let nums: Vec<f64> = cs.iter().filter_map(|c| c.sum).collect();
                        match agg {
                            "count" => Some(cs.iter().map(|c| c.n as f64).sum()),
                            "sum" => Some(nums.iter().sum()),
                            "avg" => {
                                // Weighted by the number of numeric values: avg × count per cell.
                                let (s, n) = cs.iter().fold((0.0, 0.0), |(s, n), c| match (c.sum, c.avg) {
                                    (Some(sum), Some(avg)) if avg != 0.0 => (s + sum, n + sum / avg),
                                    (Some(_), Some(_)) => (s, n + f64::NAN),
                                    _ => (s, n),
                                });
                                if n.is_nan() {
                                    return Some(f64::NAN);
                                }
                                (n > 0.0).then(|| s / n)
                            }
                            "min" => cs.iter().filter_map(|c| c.min).min_by(f64::total_cmp),
                            _ => cs.iter().filter_map(|c| c.max).max_by(f64::total_cmp),
                        }
                    })
                    .collect();
                let ok = want.len() == got_other.len()
                    && want.iter().zip(&got_other).all(|(w, g)| match (w, g) {
                        (Some(a), _) if a.is_nan() => true, // undeterminable from the SQL output
                        (None, None) => true,
                        (Some(a), Some(b)) => close(*a, *b),
                        _ => false,
                    });
                if !ok {
                    case.fail(&format!("series {i} ({agg}) $other"), &want, &got_other);
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Events and scans
// ---------------------------------------------------------------------------

fn numbers_equal(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(x), Value::Number(y)) => x.as_f64() == y.as_f64(),
        (Value::Object(x), Value::Object(y)) => x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| numbers_equal(v, w))),
        _ => a == b,
    }
}

fn row_json(r: &Row) -> Value {
    let props: serde_json::Map<String, Value> = r.props.iter().map(|(k, v): (&String, &PVal)| (k.clone(), v.to_json())).collect();
    json!({
        "uuid": hex(&r.uuid), "ts": r.ts, "recv_ts": r.recv_ts, "event": r.event, "person_key": r.person_key,
        "anon_id": r.anon_id, "user_id": r.user_id, "session_id": r.session_id.as_ref().map(|s| hex(s)),
        "platform": r.platform, "os": null, "os_version": null, "device_model": null, "device_type": null,
        "app_version": null, "locale": null, "country": r.country, "sdk": null, "url_path": null,
        "referrer_host": null, "screen": null, "flags": 0, "props": props,
    })
}

async fn check_events(svc: &Service, ds: &Dataset, rng: &mut StdRng, seed: u64) {
    let (from, to, from_ms, to_ms) = random_range(rng, ds);
    let filter = maybe_filter(rng, ds, 0.6);
    let order = *pick(rng, &["asc", "desc"]);
    let limit = *pick(rng, &[1usize, 2, 3, 7, 50, 1000]);
    let mut base = json!({"from": from, "to": to, "order": order, "limit": limit});
    if let Some(f) = &filter {
        base["filter"] = f.clone();
    }
    let case = Case { seed, kind: "events", request: &base };
    let mut expected: Vec<Row> =
        in_range(ds, from_ms, to_ms).into_iter().filter(|r| model::passes(filter.as_ref(), r, &ds.persons)).collect();
    expected.sort_by(|a, b| (a.ts, a.uuid).cmp(&(b.ts, b.uuid)));
    if order == "desc" {
        expected.reverse();
    }
    let mut got: Vec<Value> = Vec::new();
    let mut cursor: Option<String> = None;
    for page in 0.. {
        let mut body = base.clone();
        if let Some(c) = &cursor {
            body["cursor"] = json!(c);
        }
        let v = run(svc, ds.project, Kind::Events, &body, seed).await;
        let events = v["events"].as_array().expect("events");
        if events.len() > limit || (events.is_empty() && page > 0 && !expected.is_empty()) {
            case.fail("page size", &limit, &events.len());
        }
        got.extend(events.iter().cloned());
        match v["next_cursor"].as_str() {
            Some(c) => cursor = Some(c.to_owned()),
            None => break,
        }
        if got.len() > expected.len() + limit {
            break;
        }
    }
    let ids = |v: &[Value]| v.iter().map(|e| e["uuid"].as_str().unwrap().to_owned()).collect::<Vec<_>>();
    let want_ids: Vec<String> = expected.iter().map(|r| hex(&r.uuid)).collect();
    case.check("uuid sequence across pages", &want_ids, &ids(&got));
    for (r, e) in expected.iter().zip(&got) {
        let want = row_json(r);
        for (k, w) in want.as_object().unwrap() {
            if !numbers_equal(w, &e[k]) {
                case.fail(&format!("field `{k}` of event {}", hex(&r.uuid)), w, &e[k]);
            }
        }
        case.check("no extra fields", &want.as_object().unwrap().len(), &e.as_object().unwrap().len());
    }
}

/// The scan layer on its own, through `/q/sql`: the table must equal the
/// logical truth, and the declared order must be real (DataFusion drops the
/// sort it believes redundant).
async fn check_scan(svc: &Service, ds: &Dataset, seed: u64) {
    let range = json!({"from": ds.days[0], "to": ds.days[ds.days.len() - 1]});
    let sql = |q: &str| {
        let mut b = range.clone();
        b["sql"] = json!(q);
        b
    };
    let body = sql("SELECT hex(uuid), person_key, ts FROM events ORDER BY person_key, ts, uuid");
    let case = Case { seed, kind: "sql/scan", request: &body };
    let v = run(svc, ds.project, Kind::Sql, &body, seed).await;
    let got: Vec<(String, String, i64)> = v["rows"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| (r[0].as_str().unwrap().to_owned(), r[1].as_str().unwrap().to_owned(), r[2].as_i64().unwrap()))
        .collect();
    let want: Vec<(String, String, i64)> = ds.rows.iter().map(|r| (hex(&r.uuid), r.person_key.clone(), r.ts)).collect();
    case.check("ordered scan equals the logical table", &want, &got);

    // Unordered path and aggregation over it.
    let body = sql("SELECT person_key, count(*), count(DISTINCT uuid), min(ts), max(ts) FROM events GROUP BY 1");
    let case = Case { seed, kind: "sql/agg", request: &body };
    let v = run(svc, ds.project, Kind::Sql, &body, seed).await;
    let mut got: BTreeMap<String, (i64, i64, i64, i64)> = BTreeMap::new();
    for r in v["rows"].as_array().unwrap() {
        got.insert(r[0].as_str().unwrap().to_owned(), (r[1].as_i64().unwrap(), r[2].as_i64().unwrap(), r[3].as_i64().unwrap(), r[4].as_i64().unwrap()));
    }
    let mut want: BTreeMap<String, (i64, i64, i64, i64)> = BTreeMap::new();
    for r in &ds.rows {
        let e = want.entry(r.person_key.clone()).or_insert((0, 0, i64::MAX, i64::MIN));
        e.0 += 1;
        e.1 += 1;
        e.2 = e.2.min(r.ts);
        e.3 = e.3.max(r.ts);
    }
    case.check("per-person aggregates", &want, &got);

    // A window function relying on the (person_key, ts) order.
    let body = sql(
        "SELECT hex(uuid), row_number() OVER (PARTITION BY person_key ORDER BY ts, uuid) FROM events ORDER BY person_key, ts, uuid",
    );
    let case = Case { seed, kind: "sql/window", request: &body };
    let v = run(svc, ds.project, Kind::Sql, &body, seed).await;
    let got: Vec<(String, i64)> = v["rows"].as_array().unwrap().iter().map(|r| (r[0].as_str().unwrap().to_owned(), r[1].as_i64().unwrap())).collect();
    let mut want = Vec::with_capacity(ds.rows.len());
    let mut rn = 0i64;
    for (i, r) in ds.rows.iter().enumerate() {
        rn = if i > 0 && ds.rows[i - 1].person_key == r.person_key { rn + 1 } else { 1 };
        want.push((hex(&r.uuid), rn));
    }
    case.check("row numbers per person", &want, &got);

    // Person properties through the `persons` table.
    let body = sql("SELECT person_key, prop(props, 'tier'), prop_num(props, 'score') FROM persons ORDER BY 1");
    let case = Case { seed, kind: "sql/persons", request: &body };
    let v = run(svc, ds.project, Kind::Sql, &body, seed).await;
    let got: Vec<(String, Option<String>, Option<f64>)> = v["rows"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| (r[0].as_str().unwrap().to_owned(), r[1].as_str().map(str::to_owned), r[2].as_f64()))
        .collect();
    let mut want: Vec<(String, Option<String>, Option<f64>)> = ds
        .persons
        .iter()
        .map(|(k, p)| (k.clone(), p.get("tier").map(PVal::text), p.get("score").and_then(PVal::number)))
        .collect();
    want.sort_by(|a, b| a.0.cmp(&b.0));
    case.check("persons table", &want, &got);
}

/// How many random queries of each kind to run per seed.
#[derive(Debug, Clone, Copy)]
pub struct Battery {
    pub funnels_per_mode: usize,
    pub retentions: usize,
    pub trends: usize,
    pub events: usize,
}

impl Battery {
    pub const QUICK: Battery = Battery { funnels_per_mode: 2, retentions: 3, trends: 4, events: 3 };
    pub const FULL: Battery = Battery { funnels_per_mode: 4, retentions: 6, trends: 8, events: 5 };
}

/// Generates one project for `seed` and runs the battery against it.
pub async fn run_seed(seed: u64, battery: Battery) -> Tally {
    let store = Store::memory();
    let ds = generate(&store, &GenConfig::small(seed)).await;
    let mut cfg = service_config();
    // Vary the execution strategy with the seed: person-range parallelism for
    // the ordered operators, and row filters inside vs. after the decoder.
    if seed % 2 == 0 {
        cfg.person_split = vec!["a:anon-04".into(), "u:user03".into(), "u:user09".into()];
    }
    cfg.pushdown_filters = seed % 3 != 0;
    let svc = service_for(&store, cfg).await;
    let mut rng = StdRng::seed_from_u64(seed ^ 0x5eed_cafe);
    let mut tally = Tally {
        seeds: 1,
        logical_events: ds.stats.logical_events as u64,
        physical_rows: ds.stats.physical_rows as u64,
        files: ds.stats.files as u64,
        duplicate_copies: ds.stats.duplicate_copies as u64,
        late_events: ds.stats.late_events as u64,
        identity_links: ds.stats.links as u64,
        anonymous_rows_at_write: ds.stats.rows_needing_rewrite as u64,
        tombstoned_rows: ds.stats.tombstoned_rows as u64,
        compacted_days: ds.stats.compacted_days as u64,
        ..Default::default()
    };
    check_scan(&svc, &ds, seed).await;
    tally.scans += 4;
    for order in ["ordered", "strict", "any"] {
        for with_breakdown in [false, true] {
            for _ in 0..battery.funnels_per_mode {
                check_funnel(&svc, &ds, &mut rng, seed, order, with_breakdown).await;
                tally.funnels += 1;
            }
        }
    }
    for _ in 0..battery.retentions {
        check_retention(&svc, &ds, &mut rng, seed).await;
        tally.retentions += 1;
    }
    for i in 0..battery.trends {
        check_trends(&svc, &ds, &mut rng, seed, i % 2 == 0).await;
        tally.trends += 1;
    }
    for _ in 0..battery.events {
        check_events(&svc, &ds, &mut rng, seed).await;
        tally.event_walks += 1;
    }
    tally
}
