//! `POST /q/events` (`spec/query.md` §6): raw events in `(ts, uuid)` order
//! with keyset pagination.
//!
//! Files are sorted by person, not time, so each day is read with a top-N
//! sort — but days are disjoint in time, so they are visited one at a time
//! in the requested direction and the walk stops as soon as a page is full.

use crate::error::{ApiError, ApiResult};
use crate::filter::Filter;
use crate::json::canonical;
use crate::request::Obj;
use crate::scan::{day_bounds, table_schema, ts_lit, ScanRequest};
use crate::session::QueryCx;
use arrow::compute::SortOptions;
use base64::Engine as _;
use datafusion::common::ScalarValue;
use datafusion::logical_expr::{lit, Expr};
use datafusion::physical_expr::expressions::Column as PhysColumn;
use datafusion::physical_expr::{LexOrdering, PhysicalSortExpr};
use datafusion::physical_plan::sorts::sort::SortExec;
use datafusion::physical_plan::ExecutionPlan;
use datafusion::prelude::ident;
use la_codec::ids::{hex32, parse_uuid};
use la_core::events::{batch_to_rows, Cell, EventRow};
use la_core::schema::{PromotedColumn, DIM_COLUMNS};
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use std::sync::Arc;

pub const MAX_LIMIT: usize = 1000;
const DEFAULT_LIMIT: usize = 100;

#[derive(Debug, Clone)]
pub struct EventsRequest {
    pub from_ms: i64,
    pub to_ms: i64,
    pub filter: Option<Filter>,
    pub limit: usize,
    pub desc: bool,
    pub cursor: Option<String>,
    /// Canonical form of everything the cursor is bound to.
    binding: String,
}

/// Position of the last row of a page.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Position {
    ts: i64,
    uuid: [u8; 16],
}

impl EventsRequest {
    pub fn parse(body: &Value) -> ApiResult<Self> {
        let mut o = Obj::new(body, "events request")?;
        let (from_ms, to_ms) = o.range()?;
        let filter_raw = o.get("filter").cloned().unwrap_or(Value::Null);
        let filter = crate::filter::parse_optional(Some(&filter_raw))?;
        let limit = o.u64("limit")?.map(|n| n as usize).unwrap_or(DEFAULT_LIMIT);
        if limit == 0 || limit > MAX_LIMIT {
            return Err(ApiError::bad_request(format!("`limit` must be between 1 and {MAX_LIMIT}")));
        }
        let desc = match o.str("order")? {
            None | Some("desc") => true,
            Some("asc") => false,
            Some(other) => return Err(ApiError::bad_request(format!("unknown order {other:?}"))),
        };
        let cursor = o.str("cursor")?.map(str::to_owned);
        o.finish()?;
        let binding = format!("{from_ms}|{to_ms}|{}|{}", if desc { "desc" } else { "asc" }, canonical(&filter_raw));
        Ok(Self { from_ms, to_ms, filter, limit, desc, cursor, binding })
    }

    pub fn uses_person(&self) -> bool {
        self.filter.as_ref().is_some_and(Filter::uses_person)
    }

    fn tag(&self, project: u32) -> String {
        let d = Sha256::digest(format!("{project}|{}", self.binding).as_bytes());
        d[..8].iter().map(|b| format!("{b:02x}")).collect()
    }

    fn encode_cursor(&self, project: u32, p: Position) -> String {
        let payload = json!({ "t": p.ts, "u": hex32(&p.uuid), "k": self.tag(project) }).to_string();
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(payload)
    }

    /// A cursor is only valid for the project and query that produced it.
    fn decode_cursor(&self, project: u32) -> ApiResult<Option<Position>> {
        let Some(raw) = &self.cursor else { return Ok(None) };
        let bad = || ApiError::bad_request("invalid cursor");
        let bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(raw).map_err(|_| bad())?;
        let v: Value = serde_json::from_slice(&bytes).map_err(|_| bad())?;
        let m = v.as_object().ok_or_else(bad)?;
        if m.len() != 3 || m.get("k").and_then(Value::as_str) != Some(self.tag(project).as_str()) {
            return Err(bad());
        }
        let ts = m.get("t").and_then(Value::as_i64).ok_or_else(bad)?;
        let uuid = m.get("u").and_then(Value::as_str).filter(|s| s.len() == 32).and_then(parse_uuid).ok_or_else(bad)?;
        if ts < self.from_ms || ts >= self.to_ms {
            return Err(bad());
        }
        Ok(Some(Position { ts, uuid }))
    }
}

fn uuid_lit(u: &[u8; 16]) -> Expr {
    lit(ScalarValue::FixedSizeBinary(16, Some(u.to_vec())))
}

fn merged_props(row: &EventRow, promoted: &[PromotedColumn]) -> Value {
    let mut props: Map<String, Value> = match serde_json::from_str(&row.props) {
        Ok(Value::Object(m)) => m,
        _ => Map::new(),
    };
    for (c, cell) in promoted.iter().zip(&row.promoted) {
        let v = match cell {
            None => continue,
            Some(Cell::Str(s)) => json!(s),
            Some(Cell::Int(i)) => json!(i),
            Some(Cell::Float(f)) => json!(f),
            Some(Cell::Bool(b)) => json!(b),
        };
        props.insert(c.key.clone(), v);
    }
    Value::Object(props)
}

/// The response shape of one event (`props` is the logical view: promoted
/// column values are folded back in).
pub fn event_json(row: &EventRow, promoted: &[PromotedColumn]) -> Value {
    let mut m = Map::new();
    m.insert("uuid".into(), json!(hex32(&row.uuid)));
    m.insert("ts".into(), json!(row.ts));
    m.insert("recv_ts".into(), json!(row.recv_ts));
    m.insert("event".into(), json!(row.event));
    m.insert("person_key".into(), json!(row.person_key));
    m.insert("anon_id".into(), json!(row.anon_id));
    m.insert("user_id".into(), json!(row.user_id));
    m.insert("session_id".into(), json!(row.session_id.as_ref().map(hex32)));
    for (name, value) in DIM_COLUMNS.iter().zip(&row.dims) {
        m.insert((*name).into(), json!(value));
    }
    m.insert("flags".into(), json!(row.flags));
    m.insert("props".into(), merged_props(row, promoted));
    Value::Object(m)
}

pub async fn run(cx: &QueryCx, req: &EventsRequest) -> ApiResult<Value> {
    let project = cx.snap.project;
    let cursor = req.decode_cursor(project)?;
    let promoted = cx.snap.schema.columns.clone();
    let table = table_schema(&cx.snap);
    let exprs: Vec<(Expr, String)> =
        table.fields().iter().map(|f| (ident(f.name().as_str()), f.name().clone())).collect();

    let mut filters: Vec<Expr> = req.filter.iter().map(|f| cx.compiler.compile(f)).collect();
    let (mut from_ms, mut to_ms) = (req.from_ms, req.to_ms);
    if let Some(c) = cursor {
        let (ts, id) = (ident("ts"), ident("uuid"));
        if req.desc {
            to_ms = to_ms.min(c.ts + 1);
            filters.push(ts.clone().lt(ts_lit(c.ts)).or(ts.eq(ts_lit(c.ts)).and(id.lt(uuid_lit(&c.uuid)))));
        } else {
            from_ms = from_ms.max(c.ts);
            filters.push(ts.clone().gt(ts_lit(c.ts)).or(ts.eq(ts_lit(c.ts)).and(id.gt(uuid_lit(&c.uuid)))));
        }
    }

    let mut days: Vec<_> = cx.snap.days.clone();
    if req.desc {
        days.reverse();
    }
    let want = req.limit + 1;
    let mut rows: Vec<EventRow> = Vec::new();
    for day in days {
        let Some((day_start, day_end)) = day_bounds(&day.day) else { continue };
        let (lo, hi) = (from_ms.max(day_start), to_ms.min(day_end));
        if lo >= hi {
            continue;
        }
        let mut snap = cx.snap.as_ref().clone();
        snap.from_ms = lo;
        snap.to_ms = hi;
        snap.days = vec![day];
        let scan = cx.plan_with(Arc::new(snap), &ScanRequest { exprs: exprs.clone(), filters: filters.clone(), ordered: false })?;
        let schema = scan.schema();
        let options = SortOptions { descending: req.desc, nulls_first: false };
        let key = ["ts", "uuid"]
            .iter()
            .map(|c| Ok(PhysicalSortExpr::new(Arc::new(PhysColumn::new(c, schema.index_of(c)?)) as _, options)))
            .collect::<Result<Vec<_>, arrow::error::ArrowError>>()?;
        let ordering = LexOrdering::new(key).ok_or_else(|| ApiError::internal("empty sort key"))?;
        // A sort with a fetch is a bounded top-N heap.
        let top: Arc<dyn ExecutionPlan> = Arc::new(SortExec::new(ordering, scan).with_fetch(Some(want - rows.len())));
        let batches = datafusion::physical_plan::collect(top, cx.task_ctx()).await?;
        for b in &batches {
            rows.extend(batch_to_rows(b, &promoted)?);
        }
        if rows.len() >= want {
            break;
        }
    }
    let more = rows.len() > req.limit;
    rows.truncate(req.limit);
    let next_cursor = match (more, rows.last()) {
        (true, Some(last)) => Some(req.encode_cursor(project, Position { ts: last.ts, uuid: last.uuid })),
        _ => None,
    };
    let events: Vec<Value> = rows.iter().map(|r| event_json(r, &promoted)).collect();
    Ok(json!({ "events": events, "next_cursor": next_cursor }))
}
