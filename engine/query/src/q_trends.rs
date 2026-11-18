//! `POST /q/trends` (`spec/query.md` §3, design §11.6).
//!
//! One pass over the scan computes every series. Counts and sums are plain
//! grouping; `unique` relies on the person order: within one person the
//! bucket index never decreases, so "first time this person shows up in this
//! (bucket, breakdown)" needs only the current bucket and the breakdown
//! values seen in it. For `hour`/`day` buckets a person's rows for a bucket
//! live in a single day, so day partitions are processed in parallel and
//! added up; `week`/`month` buckets use the merged stream.

use crate::error::{ApiError, ApiResult};
use crate::filter::{Filter, Ref};
use crate::intern::Interner;
use crate::request::{buckets, iso, Interval, Obj};
use crate::scan::ScanRequest;
use crate::session::{fold_partitions, merged_stream, MemBudget, QueryCx};
use crate::udf::BucketFn;
use arrow::array::{Array, AsArray, RecordBatch, StringArray};
use arrow::datatypes::{Float64Type, Int32Type};
use datafusion::logical_expr::{Expr, ScalarUDF};
use datafusion::prelude::ident;
use futures::StreamExt;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::sync::Arc;

pub const MAX_SERIES: usize = 20;
pub const MAX_BUCKETS: usize = 10_000;
const DEFAULT_BREAKDOWN_LIMIT: usize = 25;
const MAX_BREAKDOWN_LIMIT: usize = 1_000;
pub const OTHER: &str = "$other";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Agg {
    Count,
    Unique,
    Sum,
    Avg,
    Min,
    Max,
}

impl Agg {
    fn parse(s: &str) -> ApiResult<Self> {
        Ok(match s {
            "count" => Agg::Count,
            "unique" => Agg::Unique,
            "sum" => Agg::Sum,
            "avg" => Agg::Avg,
            "min" => Agg::Min,
            "max" => Agg::Max,
            other => return Err(ApiError::bad_request(format!("unknown aggregation {other:?}"))),
        })
    }

    fn needs_value(self) -> bool {
        matches!(self, Agg::Sum | Agg::Avg | Agg::Min | Agg::Max)
    }
}

#[derive(Debug, Clone)]
pub struct Series {
    pub event: Option<String>,
    pub agg: Agg,
    pub label: Option<String>,
    pub prop: Option<Ref>,
    pub filter: Option<Filter>,
}

#[derive(Debug, Clone)]
pub struct TrendsRequest {
    pub from_ms: i64,
    pub to_ms: i64,
    pub interval: Interval,
    pub series: Vec<Series>,
    pub filter: Option<Filter>,
    pub breakdown: Option<Ref>,
    pub breakdown_limit: usize,
}

impl TrendsRequest {
    pub fn parse(body: &Value) -> ApiResult<Self> {
        let mut o = Obj::new(body, "trends request")?;
        let (from_ms, to_ms) = o.range()?;
        let interval = Interval::parse(o.str("interval")?.unwrap_or("day"), true)?;
        let raw = o.array("series")?.ok_or_else(|| ApiError::bad_request("trends needs `series`"))?;
        if raw.is_empty() || raw.len() > MAX_SERIES {
            return Err(ApiError::bad_request(format!("between 1 and {MAX_SERIES} series are allowed")));
        }
        let mut series = Vec::with_capacity(raw.len());
        for s in raw {
            let mut so = Obj::new(s, "series")?;
            let agg = Agg::parse(so.str("agg")?.unwrap_or("count"))?;
            let item = Series {
                event: so.str("event")?.map(str::to_owned),
                agg,
                label: so.str("label")?.map(str::to_owned),
                prop: so.reference("prop")?,
                filter: so.filter("filter")?,
            };
            so.finish()?;
            if agg.needs_value() && item.prop.is_none() {
                return Err(ApiError::bad_request("sum/avg/min/max series need `prop`"));
            }
            if !agg.needs_value() && item.prop.is_some() {
                return Err(ApiError::bad_request("`prop` is only valid for sum/avg/min/max series"));
            }
            series.push(item);
        }
        let limit = o.u64("breakdown_limit")?.map(|n| n as usize).unwrap_or(DEFAULT_BREAKDOWN_LIMIT);
        if limit == 0 || limit > MAX_BREAKDOWN_LIMIT {
            return Err(ApiError::bad_request(format!("`breakdown_limit` must be between 1 and {MAX_BREAKDOWN_LIMIT}")));
        }
        let req = Self {
            from_ms,
            to_ms,
            interval,
            series,
            filter: o.filter("filter")?,
            breakdown: o.reference("breakdown")?,
            breakdown_limit: limit,
        };
        o.finish()?;
        if buckets(from_ms, to_ms, interval).len() > MAX_BUCKETS {
            return Err(ApiError::bad_request(format!("more than {MAX_BUCKETS} buckets; use a coarser interval")));
        }
        Ok(req)
    }

    pub fn uses_person(&self) -> bool {
        self.filter.as_ref().is_some_and(Filter::uses_person)
            || self.breakdown.as_ref().is_some_and(Ref::uses_person)
            || self.series.iter().any(|s| {
                s.filter.as_ref().is_some_and(Filter::uses_person) || s.prop.as_ref().is_some_and(Ref::uses_person)
            })
    }

    pub fn has_unique(&self) -> bool {
        self.series.iter().any(|s| s.agg == Agg::Unique)
    }

    pub fn layout(&self) -> Layout {
        let mut next = 0usize;
        let mut take = |on: bool| {
            if on {
                next += 1;
                Some(next - 1)
            } else {
                None
            }
        };
        let person = take(self.has_unique());
        let bucket = take(true).unwrap_or(0);
        let breakdown = take(self.breakdown.is_some());
        let matched = self.series.iter().map(|_| take(true).unwrap_or(0)).collect();
        let value = self.series.iter().map(|s| take(s.agg.needs_value())).collect();
        Layout { person, bucket, breakdown, matched, value, aggs: self.series.iter().map(|s| s.agg).collect() }
    }

    pub fn scan(&self, cx: &QueryCx, starts: &Arc<Vec<i64>>) -> ScanRequest {
        let bucket = Arc::new(ScalarUDF::new_from_impl(BucketFn::new(Arc::clone(starts), self.to_ms)));
        let conds: Vec<Expr> =
            self.series.iter().map(|s| cx.compiler.event_condition(s.event.as_deref(), s.filter.as_ref())).collect();
        let mut exprs = Vec::new();
        if self.has_unique() {
            exprs.push((ident("person_key"), "person_key".to_owned()));
        }
        exprs.push((bucket.call(vec![ident("ts")]), "b".to_owned()));
        if let Some(b) = &self.breakdown {
            exprs.push((cx.compiler.text(b), "bd".to_owned()));
        }
        for (i, c) in conds.iter().enumerate() {
            exprs.push((c.clone(), format!("m{i}")));
        }
        for (i, s) in self.series.iter().enumerate() {
            if let Some(p) = s.prop.as_ref().filter(|_| s.agg.needs_value()) {
                exprs.push((cx.compiler.number(p), format!("v{i}")));
            }
        }
        let mut filters: Vec<Expr> = self.filter.iter().map(|f| cx.compiler.compile(f)).collect();
        let unconditional = self.series.iter().any(|s| s.event.is_none() && s.filter.is_none());
        if !unconditional {
            if let Some(any) = conds.into_iter().reduce(Expr::or) {
                filters.push(any);
            }
        }
        // Unique counting needs each partition in person order.
        ScanRequest { exprs, filters, ordered: self.has_unique() }
    }
}

/// Column positions in the scan output.
#[derive(Debug, Clone)]
pub struct Layout {
    person: Option<usize>,
    bucket: usize,
    breakdown: Option<usize>,
    matched: Vec<usize>,
    value: Vec<Option<usize>>,
    aggs: Vec<Agg>,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Cell {
    /// Matching rows.
    pub n: u64,
    pub sum: f64,
    /// Rows with a numeric value.
    pub cnt: u64,
    pub min: f64,
    pub max: f64,
    /// Distinct persons (unique series).
    pub uniq: u64,
}

impl Default for Cell {
    fn default() -> Self {
        Self { n: 0, sum: 0.0, cnt: 0, min: f64::INFINITY, max: f64::NEG_INFINITY, uniq: 0 }
    }
}

impl Cell {
    fn add(&mut self, o: &Cell) {
        self.n += o.n;
        self.sum += o.sum;
        self.cnt += o.cnt;
        self.min = self.min.min(o.min);
        self.max = self.max.max(o.max);
        self.uniq += o.uniq;
    }
}

/// Aggregation state for one stream of rows.
pub struct TrendsAcc {
    layout: Layout,
    n_series: usize,
    /// `(group << 32 | bucket)` → offset of its `n_series` cells in `cells`.
    index: HashMap<u64, u32>,
    cells: Vec<Cell>,
    values: Interner,
    last_key: u64,
    last_slot: u32,
    new_bytes: usize,
    // unique-count state of the current person
    person: String,
    has_person: bool,
    cur_bucket: Vec<i32>,
    seen: Vec<Vec<u32>>,
}

impl TrendsAcc {
    pub fn new(layout: Layout) -> Self {
        let n = layout.aggs.len();
        let mut acc = Self {
            layout,
            n_series: n,
            index: HashMap::new(),
            cells: Vec::new(),
            values: Interner::default(),
            last_key: u64::MAX,
            last_slot: 0,
            new_bytes: 0,
            person: String::new(),
            has_person: false,
            cur_bucket: vec![-1; n],
            seen: vec![Vec::new(); n],
        };
        if acc.layout.breakdown.is_none() {
            acc.values.intern(None);
        }
        acc
    }

    pub fn take_new_bytes(&mut self) -> usize {
        std::mem::take(&mut self.new_bytes)
    }

    fn group(&mut self, value: Option<&str>) -> u32 {
        let (id, created) = self.values.intern(value);
        if created {
            self.new_bytes += 96 + value.map_or(0, str::len) * 2;
        }
        id
    }

    fn slot(&mut self, group: u32, bucket: u32) -> usize {
        let key = (group as u64) << 32 | bucket as u64;
        if key == self.last_key {
            return self.last_slot as usize;
        }
        let slot = match self.index.get(&key) {
            Some(s) => *s,
            None => {
                let s = self.cells.len() as u32;
                self.cells.resize(self.cells.len() + self.n_series, Cell::default());
                self.index.insert(key, s);
                self.new_bytes += 24 + self.n_series * std::mem::size_of::<Cell>();
                s
            }
        };
        self.last_key = key;
        self.last_slot = slot;
        slot as usize
    }

    pub fn push_batch(&mut self, batch: &RecordBatch) {
        let n = batch.num_rows();
        let l = self.layout.clone();
        let persons: Option<&StringArray> = l.person.map(|i| batch.column(i).as_string::<i32>());
        let bucket = batch.column(l.bucket).as_primitive::<Int32Type>();
        let bd: Option<&StringArray> = l.breakdown.map(|i| batch.column(i).as_string::<i32>());
        let matched: Vec<_> = l.matched.iter().map(|i| batch.column(*i).as_boolean()).collect();
        let values: Vec<_> = l.value.iter().map(|v| v.map(|i| batch.column(i).as_primitive::<Float64Type>())).collect();
        for row in 0..n {
            if bucket.is_null(row) {
                continue;
            }
            let b = bucket.value(row);
            if let Some(p) = persons {
                let pk = p.value(row);
                if !self.has_person || self.person != pk {
                    self.person.clear();
                    self.person.push_str(pk);
                    self.has_person = true;
                    self.cur_bucket.fill(-1);
                }
            }
            let mut slot: Option<(u32, usize)> = None;
            for s in 0..self.n_series {
                let m = matched[s];
                if !(m.is_valid(row) && m.value(row)) {
                    continue;
                }
                let (group, at) = match slot {
                    Some(x) => x,
                    None => {
                        let g = match bd {
                            Some(a) => self.group((!a.is_null(row)).then(|| a.value(row))),
                            None => 0,
                        };
                        let x = (g, self.slot(g, b as u32));
                        slot = Some(x);
                        x
                    }
                };
                let agg = l.aggs[s];
                if agg == Agg::Unique {
                    if self.cur_bucket[s] != b {
                        self.cur_bucket[s] = b;
                        self.seen[s].clear();
                    }
                    if !self.seen[s].contains(&group) {
                        self.seen[s].push(group);
                        self.cells[at + s].uniq += 1;
                    }
                }
                let cell = &mut self.cells[at + s];
                cell.n += 1;
                if let Some(v) = values[s] {
                    if v.is_valid(row) {
                        let x = v.value(row);
                        if x.is_finite() {
                            cell.sum += x;
                            cell.cnt += 1;
                            cell.min = cell.min.min(x);
                            cell.max = cell.max.max(x);
                        }
                    }
                }
            }
        }
    }

    pub fn finish(self) -> TrendsResult {
        let mut cells: HashMap<(Option<String>, u32), Vec<Cell>> = HashMap::with_capacity(self.index.len());
        for (key, slot) in &self.index {
            let (g, b) = ((key >> 32) as usize, *key as u32);
            let s = *slot as usize;
            cells.insert((self.values.name(g as u32).clone(), b), self.cells[s..s + self.n_series].to_vec());
        }
        TrendsResult { cells }
    }
}

/// `(breakdown value, bucket index)` → one cell per series.
#[derive(Debug, Clone, Default)]
pub struct TrendsResult {
    pub cells: HashMap<(Option<String>, u32), Vec<Cell>>,
}

impl TrendsResult {
    pub fn merge(&mut self, other: TrendsResult) {
        for (k, v) in other.cells {
            match self.cells.get_mut(&k) {
                Some(mine) => mine.iter_mut().zip(&v).for_each(|(a, b)| a.add(b)),
                None => {
                    self.cells.insert(k, v);
                }
            }
        }
    }
}

pub async fn compute(cx: &QueryCx, req: &TrendsRequest) -> ApiResult<(Vec<i64>, TrendsResult)> {
    let starts = Arc::new(buckets(req.from_ms, req.to_ms, req.interval));
    let plan = cx.plan(&req.scan(cx, &starts))?;
    let layout = req.layout();
    let budget = Arc::new(cx.reserve("trends"));
    let merged_needed = req.has_unique() && matches!(req.interval, Interval::Week | Interval::Month);
    let mut result = TrendsResult::default();
    if merged_needed {
        // A person's rows for one bucket span several days: merge on
        // (person_key, bucket) — valid because the bucket is monotone in ts.
        let mut stream = merged_stream(plan, &["person_key", "b"], cx.task_ctx())?;
        let mut acc = TrendsAcc::new(layout);
        while let Some(batch) = stream.next().await {
            acc.push_batch(&batch?);
            budget.grow(acc.take_new_bytes())?;
            tokio::task::consume_budget().await;
        }
        result = acc.finish();
    } else {
        let b: Arc<MemBudget> = Arc::clone(&budget);
        let parts = fold_partitions(
            plan,
            cx.task_ctx(),
            cx.engine.cfg.partitions,
            || TrendsAcc::new(layout.clone()),
            move |acc: &mut TrendsAcc, batch| {
                acc.push_batch(batch);
                b.grow(acc.take_new_bytes())
            },
        )
        .await?;
        for acc in parts {
            result.merge(acc.finish());
        }
    }
    Ok((starts.as_ref().clone(), result))
}

fn number(x: f64) -> Value {
    if x.is_finite() {
        json!(x)
    } else {
        Value::Null
    }
}

fn cell_value(agg: Agg, c: Option<&Cell>) -> Value {
    match (agg, c) {
        (Agg::Count, c) => json!(c.map_or(0, |c| c.n)),
        (Agg::Unique, c) => json!(c.map_or(0, |c| c.uniq)),
        (Agg::Sum, c) => number(c.map_or(0.0, |c| c.sum)),
        (Agg::Avg, Some(c)) if c.cnt > 0 => number(c.sum / c.cnt as f64),
        (Agg::Min, Some(c)) if c.cnt > 0 => number(c.min),
        (Agg::Max, Some(c)) if c.cnt > 0 => number(c.max),
        _ => Value::Null,
    }
}

pub fn render(req: &TrendsRequest, starts: &[i64], result: &TrendsResult) -> Value {
    let n_buckets = starts.len();
    // Group the sparse cells by breakdown value.
    let mut by_group: HashMap<&Option<String>, Vec<(u32, &Vec<Cell>)>> = HashMap::new();
    for ((g, b), cells) in &result.cells {
        by_group.entry(g).or_default().push((*b, cells));
    }
    let mut series_out = Vec::new();
    for (si, s) in req.series.iter().enumerate() {
        let row = |cells: &HashMap<u32, Cell>| -> Vec<Value> {
            (0..n_buckets as u32).map(|b| cell_value(s.agg, cells.get(&b))).collect()
        };
        if req.breakdown.is_none() {
            let cells: HashMap<u32, Cell> =
                by_group.get(&None).map(|v| v.iter().map(|(b, c)| (*b, c[si])).collect()).unwrap_or_default();
            series_out.push(json!({ "index": si, "label": s.label, "values": row(&cells) }));
            continue;
        }
        // Rank breakdown values for this series: by total count (or total sum
        // for `sum`), then by value so ties are stable.
        let mut ranked: Vec<(f64, &Option<String>, HashMap<u32, Cell>)> = Vec::new();
        for (g, entries) in &by_group {
            let cells: HashMap<u32, Cell> = entries.iter().map(|(b, c)| (*b, c[si])).filter(|(_, c)| c.n > 0).collect();
            if cells.is_empty() {
                continue;
            }
            let total: f64 = match s.agg {
                Agg::Sum => cells.values().map(|c| c.sum).sum(),
                _ => cells.values().map(|c| c.n as f64).sum(),
            };
            ranked.push((total, *g, cells));
        }
        ranked.sort_by(|a, b| {
            b.0.total_cmp(&a.0).then_with(|| match (a.1, b.1) {
                (Some(x), Some(y)) => x.cmp(y),
                (None, Some(_)) => std::cmp::Ordering::Greater,
                (Some(_), None) => std::cmp::Ordering::Less,
                (None, None) => std::cmp::Ordering::Equal,
            })
        });
        let rest = if ranked.len() > req.breakdown_limit { ranked.split_off(req.breakdown_limit) } else { Vec::new() };
        for (_, g, cells) in &ranked {
            series_out.push(json!({ "index": si, "label": s.label, "breakdown": g, "values": row(cells) }));
        }
        // `unique` cannot be added up across values, so it gets no `$other`.
        if !rest.is_empty() && s.agg != Agg::Unique {
            let mut other: HashMap<u32, Cell> = HashMap::new();
            for (_, _, cells) in &rest {
                for (b, c) in cells {
                    other.entry(*b).or_default().add(c);
                }
            }
            series_out.push(json!({ "index": si, "label": s.label, "breakdown": OTHER, "values": row(&other) }));
        }
    }
    json!({ "buckets": starts.iter().map(|s| iso(*s)).collect::<Vec<_>>(), "series": series_out })
}

pub async fn run(cx: &QueryCx, req: &TrendsRequest) -> ApiResult<Value> {
    let (starts, result) = compute(cx, req).await?;
    Ok(render(req, &starts, &result))
}
