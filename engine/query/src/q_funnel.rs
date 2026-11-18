//! `POST /q/funnel` (`spec/query.md` §4).

use crate::error::{ApiError, ApiResult};
use crate::filter::{Filter, Ref};
use crate::funnel::{FunnelAcc, FunnelResult, FunnelSpec, Order, StepStat};
use crate::request::{EventSel, Obj};
use crate::scan::ScanRequest;
use crate::session::QueryCx;
use arrow::array::{Array, AsArray, BooleanArray, RecordBatch};
use arrow::datatypes::TimestampMillisecondType;
use datafusion::logical_expr::Expr;
use datafusion::prelude::ident;
use serde_json::{json, Value};
use std::sync::Arc;

pub const MAX_STEPS: usize = 20;
const DEFAULT_WINDOW_SECONDS: u64 = 7 * 86_400;
const MAX_WINDOW_SECONDS: u64 = 400 * 86_400;
const DEFAULT_BREAKDOWN_LIMIT: usize = 1_000;

#[derive(Debug, Clone)]
pub struct FunnelRequest {
    pub from_ms: i64,
    pub to_ms: i64,
    pub steps: Vec<EventSel>,
    pub window_ms: i64,
    pub order: Order,
    pub filter: Option<Filter>,
    pub breakdown: Option<Ref>,
    pub breakdown_limit: usize,
}

impl FunnelRequest {
    pub fn parse(body: &Value) -> ApiResult<Self> {
        let mut o = Obj::new(body, "funnel request")?;
        let (from_ms, to_ms) = o.range()?;
        let steps = o.array("steps")?.ok_or_else(|| ApiError::bad_request("funnel needs `steps`"))?;
        if steps.len() < 2 || steps.len() > MAX_STEPS {
            return Err(ApiError::bad_request(format!("a funnel has between 2 and {MAX_STEPS} steps")));
        }
        let steps = steps.iter().map(|s| EventSel::parse(s, "funnel step")).collect::<ApiResult<Vec<_>>>()?;
        let window = o.u64("window_seconds")?.unwrap_or(DEFAULT_WINDOW_SECONDS);
        if window == 0 || window > MAX_WINDOW_SECONDS {
            return Err(ApiError::bad_request("`window_seconds` must be between 1 and 34560000"));
        }
        let req = Self {
            from_ms,
            to_ms,
            steps,
            window_ms: window as i64 * 1000,
            order: Order::parse(o.str("order")?)?,
            filter: o.filter("filter")?,
            breakdown: o.reference("breakdown")?,
            breakdown_limit: o.u64("breakdown_limit")?.map(|n| n as usize).unwrap_or(DEFAULT_BREAKDOWN_LIMIT).max(1),
        };
        o.finish()?;
        Ok(req)
    }

    pub fn uses_person(&self) -> bool {
        self.filter.as_ref().is_some_and(Filter::uses_person)
            || self.steps.iter().any(EventSel::uses_person)
            || self.breakdown.as_ref().is_some_and(Ref::uses_person)
    }

    pub fn spec(&self) -> FunnelSpec {
        FunnelSpec {
            steps: self.steps.len(),
            window_ms: self.window_ms,
            order: self.order,
            breakdown: self.breakdown.is_some(),
        }
    }

    /// The scan feeding the operator: `person_key, ts, s0..sN[, bd]`, ordered.
    pub fn scan(&self, cx: &QueryCx) -> ScanRequest {
        let conds: Vec<Expr> =
            self.steps.iter().map(|s| cx.compiler.event_condition(s.event.as_deref(), s.filter.as_ref())).collect();
        let mut exprs = vec![(ident("person_key"), "person_key".to_owned()), (ident("ts"), "ts".to_owned())];
        for (i, c) in conds.iter().enumerate() {
            exprs.push((c.clone(), format!("s{i}")));
        }
        if let Some(b) = &self.breakdown {
            exprs.push((cx.compiler.text(b), "bd".to_owned()));
        }
        let mut filters: Vec<Expr> = self.filter.iter().map(|f| cx.compiler.compile(f)).collect();
        if self.order != Order::Strict {
            // Only rows that can advance a chain matter. In `strict` mode any
            // row passing the global filter can break one, so all are kept.
            if let Some(any_step) = conds.into_iter().reduce(Expr::or) {
                filters.push(any_step);
            }
        }
        ScanRequest { exprs, filters, ordered: true }
    }
}

/// Feeds one batch of the scan produced by [`FunnelRequest::scan`].
pub fn feed(acc: &mut FunnelAcc, batch: &RecordBatch, steps: usize, breakdown: bool) -> ApiResult<()> {
    let persons = batch.column(0).as_string::<i32>();
    let ts = batch.column(1).as_primitive::<TimestampMillisecondType>();
    let flags: Vec<&BooleanArray> = (0..steps).map(|j| batch.column(2 + j).as_boolean()).collect();
    let bd = breakdown.then(|| batch.column(2 + steps).as_string::<i32>());
    let n = batch.num_rows();
    let mut masks = vec![0u32; n];
    for (j, f) in flags.iter().enumerate() {
        if f.null_count() == 0 {
            for i in f.values().set_indices() {
                masks[i] |= 1 << j;
            }
        } else {
            for (i, m) in masks.iter_mut().enumerate() {
                if f.is_valid(i) && f.value(i) {
                    *m |= 1 << j;
                }
            }
        }
    }
    for i in 0..n {
        let value = bd.and_then(|b| (!b.is_null(i)).then(|| b.value(i)));
        acc.push(persons.value(i), ts.value(i), masks[i], value)?;
    }
    Ok(())
}

pub async fn compute(cx: &QueryCx, req: &FunnelRequest) -> ApiResult<FunnelResult> {
    let budget = Arc::new(cx.reserve("funnel"));
    let spec = req.spec();
    let parts = cx
        .fold_ordered(
            &req.scan(cx),
            &["person_key", "ts"],
            || FunnelAcc::new(spec),
            move |acc: &mut FunnelAcc, batch| {
                feed(acc, batch, spec.steps, spec.breakdown)?;
                budget.grow(acc.take_new_bytes())
            },
        )
        .await?;
    let mut result = FunnelResult::empty(spec.steps);
    for acc in parts {
        result.merge(acc.finish());
    }
    Ok(result)
}

fn steps_json(req: &FunnelRequest, stats: &[StepStat]) -> Value {
    Value::Array(
        req.steps
            .iter()
            .zip(stats)
            .enumerate()
            .map(|(i, (step, st))| {
                let median = if i == 0 { None } else { st.durations.median() };
                json!({ "event": step.event, "count": st.count, "median_seconds": median })
            })
            .collect(),
    )
}

pub fn render(req: &FunnelRequest, mut result: FunnelResult) -> Value {
    let mut out = json!({ "steps": steps_json(req, &result.total) });
    if req.breakdown.is_some() {
        // Largest first; ties by value so the order is stable. The null bucket sorts last among ties.
        result.groups.sort_by(|a, b| b.1[0].count.cmp(&a.1[0].count).then_with(|| match (&a.0, &b.0) {
            (Some(x), Some(y)) => x.cmp(y),
            (None, Some(_)) => std::cmp::Ordering::Greater,
            (Some(_), None) => std::cmp::Ordering::Less,
            (None, None) => std::cmp::Ordering::Equal,
        }));
        result.groups.truncate(req.breakdown_limit);
        let groups: Vec<Value> = result
            .groups
            .iter()
            .filter(|(_, stats)| stats[0].count > 0)
            .map(|(value, stats)| json!({ "value": value, "steps": steps_json(req, stats) }))
            .collect();
        out["breakdowns"] = Value::Array(groups);
    }
    out
}

pub async fn run(cx: &QueryCx, req: &FunnelRequest) -> ApiResult<Value> {
    Ok(render(req, compute(cx, req).await?))
}
