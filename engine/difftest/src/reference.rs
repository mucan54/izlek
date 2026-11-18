//! Reference implementations.
//!
//! Operator semantics (funnel in all modes, retention, trends incl. unique
//! counts) are expressed as **plain DataFusion SQL** over an in-memory copy of
//! the logical table: self-joins for chains, `row_number()` for adjacency,
//! `count(DISTINCT …)` for unique counts, `date_trunc` for periods. The
//! per-row predicates the SQL consumes (`g`, `s0…`, `bd`, `v…`) are computed
//! by the naive evaluator in [`crate::model`], so neither half shares code
//! with the query service.
//!
//! Step-to-step durations (medians) have no reasonable SQL form — they depend
//! on *which* chain first completes a step — so they come from an explicit
//! O(n²) dynamic program over each person's events ([`brute_funnel`]).

use crate::model::{self, hex, Persons, Row};
use arrow::array::{ArrayRef, BooleanArray, Float64Array, Int64Array, RecordBatch, StringArray};
use arrow::datatypes::{DataType, Field, Schema};
use datafusion::datasource::MemTable;
use datafusion::prelude::SessionContext;
use serde_json::Value;
use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

/// Per-row inputs of a reference query.
#[derive(Default)]
pub struct Columns {
    /// Global filter.
    pub g: Vec<bool>,
    /// Named boolean columns (`s0`, `s1`, `is_start`, `m0`, …).
    pub flags: Vec<(String, Vec<bool>)>,
    /// Breakdown value.
    pub bd: Option<Vec<Option<String>>>,
    /// Named numeric columns (`v0`, …).
    pub nums: Vec<(String, Vec<Option<f64>>)>,
}

/// Registers table `t(person_key, ts, uuid, g, <flags>, bd, <nums>)`.
pub async fn context(rows: &[Row], cols: Columns) -> SessionContext {
    let mut fields = vec![
        Field::new("person_key", DataType::Utf8, false),
        Field::new("ts", DataType::Int64, false),
        Field::new("uuid", DataType::Utf8, false),
        Field::new("g", DataType::Boolean, false),
    ];
    let mut arrays: Vec<ArrayRef> = vec![
        Arc::new(StringArray::from_iter_values(rows.iter().map(|r| r.person_key.as_str()))),
        Arc::new(Int64Array::from_iter_values(rows.iter().map(|r| r.ts))),
        Arc::new(StringArray::from_iter_values(rows.iter().map(|r| hex(&r.uuid)))),
        Arc::new(BooleanArray::from(cols.g)),
    ];
    for (name, values) in cols.flags {
        fields.push(Field::new(name, DataType::Boolean, false));
        arrays.push(Arc::new(BooleanArray::from(values)));
    }
    fields.push(Field::new("bd", DataType::Utf8, true));
    arrays.push(match cols.bd {
        Some(values) => Arc::new(StringArray::from(values)),
        None => Arc::new(StringArray::from(vec![None::<&str>; rows.len()])),
    });
    for (name, values) in cols.nums {
        fields.push(Field::new(name, DataType::Float64, true));
        arrays.push(Arc::new(Float64Array::from(values)));
    }
    let schema = Arc::new(Schema::new(fields));
    let batch = RecordBatch::try_new(schema.clone(), arrays).expect("reference batch");
    let ctx = SessionContext::new();
    ctx.register_table("t", Arc::new(MemTable::try_new(schema, vec![vec![batch]]).expect("mem table")))
        .expect("register");
    ctx
}

async fn query(ctx: &SessionContext, sql: &str) -> Vec<Vec<Value>> {
    let df = ctx.sql(sql).await.unwrap_or_else(|e| panic!("reference SQL failed to plan: {e}\n{sql}"));
    let batches = df.collect().await.unwrap_or_else(|e| panic!("reference SQL failed: {e}\n{sql}"));
    let mut out = Vec::new();
    for b in &batches {
        let cols: Vec<Vec<Value>> = b.columns().iter().map(column_values).collect();
        for i in 0..b.num_rows() {
            out.push(cols.iter().map(|c| c[i].clone()).collect());
        }
    }
    out
}

fn column_values(col: &ArrayRef) -> Vec<Value> {
    use arrow::array::{Array, AsArray};
    use arrow::datatypes::{Float64Type, Int64Type, UInt64Type};
    let n = col.len();
    (0..n)
        .map(|i| {
            if col.is_null(i) {
                return Value::Null;
            }
            match col.data_type() {
                DataType::Int64 => Value::from(col.as_primitive::<Int64Type>().value(i)),
                DataType::UInt64 => Value::from(col.as_primitive::<UInt64Type>().value(i)),
                DataType::Float64 => Value::from(col.as_primitive::<Float64Type>().value(i)),
                DataType::Utf8 => Value::from(col.as_string::<i32>().value(i)),
                DataType::Utf8View => Value::from(col.as_string_view().value(i)),
                DataType::Boolean => Value::from(col.as_boolean().value(i)),
                other => panic!("unexpected reference column type {other}"),
            }
        })
        .collect()
}

fn opt_string(v: &Value) -> Option<String> {
    v.as_str().map(str::to_owned)
}

const E_CTE: &str =
    "e AS (SELECT *, row_number() OVER (PARTITION BY person_key ORDER BY ts, uuid) AS rn FROM t WHERE g)";

// ---------------------------------------------------------------------------
// Funnel
// ---------------------------------------------------------------------------

/// `breakdown value → count per step` (`None` key = no breakdown / total).
pub type FunnelCounts = BTreeMap<Option<String>, Vec<u64>>;

/// Funnel counts via SQL. With `by_breakdown` the map is keyed by the
/// breakdown value (`Some(None)`-style null bucket is the `None` key).
pub async fn funnel_sql(
    ctx: &SessionContext,
    steps: usize,
    window_ms: i64,
    order: &str,
    by_breakdown: bool,
) -> FunnelCounts {
    let mut out: FunnelCounts = BTreeMap::new();
    let mut add = |bd: Option<String>, k: usize, c: u64| {
        out.entry(bd).or_insert_with(|| vec![0; steps])[k] += c;
    };
    if order == "any" {
        let distinct_steps: Vec<String> =
            (0..steps).map(|j| format!("max(CASE WHEN b.s{j} THEN 1 ELSE 0 END)")).collect();
        let any_step: Vec<String> = (0..steps).map(|j| format!("a.s{j}")).collect();
        let (bd_w, bd_p) = if by_breakdown { (", a.bd AS bd", ", bd") } else { ("", "") };
        let sql = format!(
            "WITH {E_CTE}, \
             w AS (SELECT a.person_key AS person_key, a.rn AS rn{bd_w}, {n} AS n \
                   FROM e a JOIN e b ON b.person_key = a.person_key AND b.rn >= a.rn AND b.ts <= a.ts + {window_ms} \
                   WHERE {any} GROUP BY a.person_key, a.rn{gb}), \
             p AS (SELECT person_key{bd_p}, max(n) AS best FROM w GROUP BY person_key{bd_p}) \
             SELECT {sel} best, count(*) FROM p GROUP BY {grp} best",
            n = distinct_steps.join(" + "),
            any = any_step.join(" OR "),
            gb = if by_breakdown { ", a.bd" } else { "" },
            sel = if by_breakdown { "bd," } else { "CAST(NULL AS VARCHAR)," },
            grp = if by_breakdown { "bd," } else { "" },
        );
        for row in query(ctx, &sql).await {
            let (bd, best, c) = (
                opt_string(&row[0]),
                row[1].as_i64().unwrap() as usize,
                row[2].as_u64().unwrap_or_else(|| row[2].as_i64().unwrap() as u64),
            );
            for k in 0..best.min(steps) {
                add(bd.clone(), k, c);
            }
        }
        return out;
    }
    for k in 1..=steps {
        let mut joins = String::new();
        for j in 2..=k {
            let follows = if order == "strict" {
                format!("e{j}.rn = e{p}.rn + 1", p = j - 1)
            } else {
                format!("(e{j}.ts > e{p}.ts OR (e{j}.ts = e{p}.ts AND e{j}.uuid > e{p}.uuid))", p = j - 1)
            };
            joins.push_str(&format!(
                " JOIN e e{j} ON e{j}.person_key = e1.person_key AND e{j}.s{s} AND {follows}",
                s = j - 1
            ));
        }
        let sql = format!(
            "WITH {E_CTE} SELECT {sel} count(DISTINCT e1.person_key) FROM e e1{joins} \
             WHERE e1.s0 AND e{k}.ts - e1.ts <= {window_ms} {grp}",
            sel = if by_breakdown { "e1.bd," } else { "CAST(NULL AS VARCHAR)," },
            grp = if by_breakdown { "GROUP BY e1.bd" } else { "" },
        );
        for row in query(ctx, &sql).await {
            let c = row[1].as_i64().unwrap() as u64;
            if c > 0 {
                add(opt_string(&row[0]), k - 1, c);
            }
        }
    }
    out
}

/// Counts and first-reach durations by brute force.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct BruteFunnel {
    pub counts: Vec<u64>,
    /// Seconds from the previous step, one entry per person that reached the step.
    pub durations: Vec<Vec<f64>>,
}

struct Ev<'a> {
    ts: i64,
    mask: u32,
    bd: Option<&'a str>,
}

/// Highest step reached after each prefix of `events` plus the durations
/// recorded on first reach, for chains that must start on a row accepted by
/// `starts`.
fn person_ordered(
    events: &[Ev],
    steps: usize,
    w: i64,
    strict: bool,
    starts: &dyn Fn(&Ev) -> bool,
) -> (usize, Vec<Option<f64>>) {
    let n = events.len();
    // f[k][i]: latest start among chains of length k+1 ending exactly at row i.
    let mut f = vec![vec![None::<i64>; n]; steps];
    let mut best = 0usize;
    let mut durations = vec![None; steps];
    for i in 0..n {
        for k in 0..steps {
            if events[i].mask >> k & 1 == 0 {
                continue;
            }
            if k == 0 {
                if starts(&events[i]) {
                    f[0][i] = Some(events[i].ts);
                }
                continue;
            }
            let candidates: Box<dyn Iterator<Item = usize>> =
                if strict { Box::new(i.checked_sub(1).into_iter()) } else { Box::new(0..i) };
            f[k][i] = candidates.filter_map(|j| f[k - 1][j]).filter(|s| events[i].ts - s <= w).max();
        }
        for k in 0..steps {
            if f[k][i].is_some() && best < k + 1 {
                // First time step k+1 is reached. Steps are reached in order
                // (a chain of length k+1 contains one of length k that ended
                // earlier), so `best == k` here.
                assert_eq!(best, k);
                best = k + 1;
                if k > 0 {
                    // The chain that got here has the latest start `m`. In
                    // strict mode its previous step is the previous row; in
                    // ordered mode it is the first row that reached step k
                    // with that start.
                    let m = f[k][i].unwrap();
                    let prev = if strict {
                        i - 1
                    } else {
                        (0..i).find(|j| f[k - 1][*j] == Some(m)).expect("previous step of the chain")
                    };
                    durations[k] = Some((events[i].ts - events[prev].ts) as f64 / 1000.0);
                }
            }
        }
    }
    (best, durations)
}

fn person_any(events: &[Ev], steps: usize, w: i64, anchor_ok: &dyn Fn(&Ev) -> bool) -> (usize, Vec<Option<f64>>) {
    let mut best = 0usize;
    let mut reached_at = vec![None::<i64>; steps + 1];
    let mut durations = vec![None; steps];
    // Evaluate the definition after every prefix: best over anchors a ≤ i of
    // the distinct steps satisfied by rows in [a, i] within the window of a.
    for i in 0..events.len() {
        let mut here = 0usize;
        for a in 0..=i {
            if events[a].mask == 0 || !anchor_ok(&events[a]) || events[i].ts > events[a].ts + w {
                continue;
            }
            let mut seen = 0u32;
            for b in &events[a..=i] {
                if b.ts <= events[a].ts + w {
                    seen |= b.mask;
                }
            }
            here = here.max(seen.count_ones() as usize);
        }
        while best < here.min(steps) {
            best += 1;
            reached_at[best] = Some(events[i].ts);
            if best >= 2 {
                durations[best - 1] = Some((events[i].ts - reached_at[best - 1].unwrap()) as f64 / 1000.0);
            }
        }
    }
    (best, durations)
}

/// Brute-force funnel over the logical rows (already filtered by the global
/// filter, in `(person_key, ts, uuid)` order). `masks[i]` bit `j` = row `i`
/// satisfies step `j`. Returns the total and the per-breakdown results.
pub fn brute_funnel(
    rows: &[&Row],
    masks: &[u32],
    bd: Option<&[Option<String>]>,
    steps: usize,
    window_ms: i64,
    order: &str,
) -> (BruteFunnel, BTreeMap<Option<String>, BruteFunnel>) {
    let new = || BruteFunnel { counts: vec![0; steps], durations: vec![Vec::new(); steps] };
    let mut total = new();
    let mut groups: BTreeMap<Option<String>, BruteFunnel> = BTreeMap::new();
    let record = |into: &mut BruteFunnel, best: usize, durations: Vec<Option<f64>>| {
        for k in 0..best {
            into.counts[k] += 1;
        }
        for (k, d) in durations.into_iter().enumerate() {
            if let Some(d) = d {
                into.durations[k].push(d);
            }
        }
    };
    let mut i = 0;
    while i < rows.len() {
        let mut j = i;
        while j < rows.len() && rows[j].person_key == rows[i].person_key {
            j += 1;
        }
        let events: Vec<Ev> =
            (i..j).map(|x| Ev { ts: rows[x].ts, mask: masks[x], bd: bd.and_then(|b| b[x].as_deref()) }).collect();
        let run = |accept: &dyn Fn(&Ev) -> bool| match order {
            "any" => person_any(&events, steps, window_ms, accept),
            "strict" => person_ordered(&events, steps, window_ms, true, accept),
            _ => person_ordered(&events, steps, window_ms, false, accept),
        };
        let (best, durations) = run(&|_| true);
        record(&mut total, best, durations);
        if bd.is_some() {
            // Each value the person could start a chain (or anchor) with.
            let mut values: Vec<Option<&str>> = events
                .iter()
                .filter(|e| if order == "any" { e.mask != 0 } else { e.mask & 1 != 0 })
                .map(|e| e.bd)
                .collect();
            values.sort();
            values.dedup();
            for v in values {
                let (best, durations) = run(&|e: &Ev| e.bd == v);
                if best > 0 {
                    record(groups.entry(v.map(str::to_owned)).or_insert_with(new), best, durations);
                }
            }
        }
        i = j;
    }
    (total, groups)
}

pub fn exact_median(values: &[f64]) -> Option<f64> {
    if values.is_empty() {
        return None;
    }
    let mut v = values.to_vec();
    v.sort_by(f64::total_cmp);
    let m = v.len() / 2;
    Some(if v.len() % 2 == 1 { v[m] } else { (v[m - 1] + v[m]) / 2.0 })
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

/// `cohort period start (ms) → (size, return period start (ms) → persons)`.
pub type RetentionRef = BTreeMap<i64, (u64, BTreeMap<i64, u64>)>;

/// Expects flags `is_start` and `is_return`.
pub async fn retention_sql(ctx: &SessionContext, period: &str) -> RetentionRef {
    let trunc = |col: &str| format!("CAST(date_trunc('{period}', to_timestamp_millis({col})) AS BIGINT)");
    let first = "f AS (SELECT person_key, min(ts) AS t0 FROM t WHERE g AND is_start GROUP BY person_key)";
    let mut out: RetentionRef = BTreeMap::new();
    let sizes = format!("WITH {first} SELECT {c} AS c, count(*) FROM f GROUP BY 1", c = trunc("t0"));
    for row in query(ctx, &sizes).await {
        out.entry(row[0].as_i64().unwrap()).or_default().0 = row[1].as_i64().unwrap() as u64;
    }
    let returns = format!(
        "WITH {first}, r AS (SELECT DISTINCT f.person_key AS person_key, {c} AS c, {rp} AS rp \
         FROM f JOIN t x ON x.person_key = f.person_key AND x.g AND x.is_return AND x.ts >= f.t0) \
         SELECT c, rp, count(*) FROM r GROUP BY c, rp",
        c = trunc("f.t0"),
        rp = trunc("x.ts"),
    );
    for row in query(ctx, &returns).await {
        out.entry(row[0].as_i64().unwrap())
            .or_default()
            .1
            .insert(row[1].as_i64().unwrap(), row[2].as_i64().unwrap() as u64);
    }
    out
}

// ---------------------------------------------------------------------------
// Trends
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct TrendCell {
    pub n: u64,
    pub unique: u64,
    pub sum: Option<f64>,
    pub avg: Option<f64>,
    pub min: Option<f64>,
    pub max: Option<f64>,
}

/// `(breakdown, bucket start ms) → cell` for one series; expects flag `m{i}`
/// and number `v{i}`.
pub async fn trends_sql(
    ctx: &SessionContext,
    series: usize,
    interval: &str,
    by_breakdown: bool,
) -> HashMap<(Option<String>, i64), TrendCell> {
    let sql = format!(
        "SELECT CAST(date_trunc('{interval}', to_timestamp_millis(ts)) AS BIGINT) AS b, {bd} AS bd, \
                count(*), count(DISTINCT person_key), sum(v{series}), avg(v{series}), min(v{series}), max(v{series}) \
         FROM t WHERE g AND m{series} GROUP BY 1, 2",
        bd = if by_breakdown { "bd" } else { "CAST(NULL AS VARCHAR)" },
    );
    let mut out = HashMap::new();
    for row in query(ctx, &sql).await {
        out.insert(
            (opt_string(&row[1]), row[0].as_i64().unwrap()),
            TrendCell {
                n: row[2].as_i64().unwrap() as u64,
                unique: row[3].as_i64().unwrap() as u64,
                sum: row[4].as_f64(),
                avg: row[5].as_f64(),
                min: row[6].as_f64(),
                max: row[7].as_f64(),
            },
        );
    }
    out
}

/// Evaluates the per-row inputs for a request with the naive evaluator.
pub fn flags_for(rows: &[Row], persons: &Persons, selectors: &[(&str, &Value)]) -> Vec<(String, Vec<bool>)> {
    selectors
        .iter()
        .map(|(name, sel)| (name.to_string(), rows.iter().map(|r| model::matches(sel, r, persons)).collect()))
        .collect()
}
