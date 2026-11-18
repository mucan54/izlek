//! `POST /q/sql` (`spec/query.md` §7): one read-only `SELECT` over `events`
//! and `persons`, enforced on the parsed statement **and** on the logical
//! plan (allow-list of node types; every table scan must be one of the two
//! providers this module registered).

use crate::error::{ApiError, ApiResult};
use crate::request::Obj;
use crate::scan::{EventsTable, PersonsTable};
use crate::session::QueryCx;
use crate::udf;
use arrow::array::{Array, ArrayRef, AsArray, RecordBatch};
use arrow::datatypes::{DataType, Int64Type, SchemaRef, TimeUnit, UInt64Type};
use arrow::util::display::{ArrayFormatter, FormatOptions};
use datafusion::common::tree_node::{TreeNode, TreeNodeRecursion};
use datafusion::datasource::source_as_provider;
use datafusion::execution::context::SQLOptions;
use datafusion::logical_expr::{Expr, LogicalPlan, LogicalPlanBuilder};
use datafusion::sql::parser::{DFParser, Statement as DfStatement};
use datafusion::sql::sqlparser::ast::Statement as SqlStatement;
use serde_json::{json, Value};
use std::sync::atomic::Ordering;
use std::sync::Arc;

pub const MAX_SQL_BYTES: usize = 64 * 1024;
const SAFE_INT: i64 = (1 << 53) - 1;

#[derive(Debug, Clone)]
pub struct SqlRequest {
    pub from_ms: i64,
    pub to_ms: i64,
    pub sql: String,
}

impl SqlRequest {
    pub fn parse(body: &Value) -> ApiResult<Self> {
        let mut o = Obj::new(body, "sql request")?;
        let (from_ms, to_ms) = o.range()?;
        let sql = o.req_str("sql")?.to_owned();
        o.finish()?;
        if sql.len() > MAX_SQL_BYTES {
            return Err(ApiError::bad_request("SQL text is too long"));
        }
        Ok(Self { from_ms, to_ms, sql })
    }

    /// The person table is loaded whenever the text could reference it.
    pub fn uses_person(&self) -> bool {
        self.sql.to_ascii_lowercase().contains("persons")
    }
}

fn forbidden(what: &str) -> ApiError {
    ApiError::bad_request(format!("{what} is not allowed; /q/sql accepts a single SELECT over `events` and `persons`"))
}

/// Statement-level gate: exactly one statement and it is a query.
fn parse_single_select(sql: &str) -> ApiResult<DfStatement> {
    let mut statements =
        DFParser::parse_sql(sql).map_err(|e| ApiError::bad_request(format!("SQL syntax error: {e}")))?;
    if statements.len() != 1 {
        return Err(forbidden("more than one statement"));
    }
    let Some(stmt) = statements.pop_front() else { return Err(forbidden("an empty statement")) };
    match &stmt {
        DfStatement::Statement(inner) if matches!(inner.as_ref(), SqlStatement::Query(_)) => Ok(stmt),
        _ => Err(forbidden("this statement type")),
    }
}

/// Plan-level gate. Returns whether the plan sorts or windows (and so
/// benefits from scans that keep the file order).
fn check_plan(plan: &LogicalPlan) -> ApiResult<bool> {
    let mut wants_order = false;
    let mut error: Option<ApiError> = None;
    plan.apply_with_subqueries(|node| {
        match node {
            LogicalPlan::Projection(_)
            | LogicalPlan::Filter(_)
            | LogicalPlan::Aggregate(_)
            | LogicalPlan::Join(_)
            | LogicalPlan::Repartition(_)
            | LogicalPlan::Union(_)
            | LogicalPlan::Subquery(_)
            | LogicalPlan::SubqueryAlias(_)
            | LogicalPlan::Limit(_)
            | LogicalPlan::Values(_)
            | LogicalPlan::EmptyRelation(_)
            | LogicalPlan::Distinct(_)
            | LogicalPlan::Unnest(_) => {}
            LogicalPlan::Sort(_) | LogicalPlan::Window(_) => wants_order = true,
            LogicalPlan::TableScan(scan) => {
                let ok = source_as_provider(&scan.source).is_ok_and(|p| {
                    let any = p.as_ref() as &dyn std::any::Any;
                    any.is::<EventsTable>() || any.is::<PersonsTable>()
                });
                if !ok {
                    error = Some(forbidden("this table source"));
                }
            }
            other => error = Some(forbidden(&format!("plan node `{}`", other.display()))),
        }
        if error.is_none() {
            // Expressions may not smuggle in parameters or unresolved wildcards.
            for e in node.expressions() {
                let bad = e.exists(|x| Ok(matches!(x, Expr::Placeholder(_)))).unwrap_or(true);
                if bad {
                    error = Some(forbidden("a query parameter"));
                }
            }
        }
        Ok(if error.is_some() { TreeNodeRecursion::Stop } else { TreeNodeRecursion::Continue })
    })
    .map_err(ApiError::from)?;
    match error {
        Some(e) => Err(e),
        None => Ok(wants_order),
    }
}

fn hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

fn int_json(v: i64) -> Value {
    if (-SAFE_INT..=SAFE_INT).contains(&v) {
        json!(v)
    } else {
        json!(v.to_string())
    }
}

/// One column as JSON values (`spec/query.md` §7 type mapping).
fn column_json(col: &ArrayRef) -> ApiResult<Vec<Value>> {
    let n = col.len();
    let mut out = Vec::with_capacity(n);
    let null = |i: usize| col.is_null(i);
    match col.data_type() {
        DataType::Null => out.resize(n, Value::Null),
        DataType::Int8 | DataType::Int16 | DataType::Int32 | DataType::Int64 | DataType::UInt8 | DataType::UInt16 | DataType::UInt32 => {
            let a = arrow::compute::cast(col, &DataType::Int64)?;
            let a = a.as_primitive::<Int64Type>();
            out.extend((0..n).map(|i| if a.is_null(i) { Value::Null } else { int_json(a.value(i)) }));
        }
        DataType::UInt64 => {
            let a = col.as_primitive::<UInt64Type>();
            out.extend((0..n).map(|i| match (a.is_null(i), a.value(i)) {
                (true, _) => Value::Null,
                (false, v) if v <= SAFE_INT as u64 => json!(v),
                (false, v) => json!(v.to_string()),
            }));
        }
        DataType::Float16 | DataType::Float32 | DataType::Float64 => {
            let a = arrow::compute::cast(col, &DataType::Float64)?;
            let a = a.as_primitive::<arrow::datatypes::Float64Type>();
            out.extend((0..n).map(|i| if a.is_null(i) || !a.value(i).is_finite() { Value::Null } else { json!(a.value(i)) }));
        }
        DataType::Timestamp(_, _) => {
            let a = arrow::compute::cast(col, &DataType::Timestamp(TimeUnit::Millisecond, None))?;
            let a = a.as_primitive::<arrow::datatypes::TimestampMillisecondType>();
            out.extend((0..n).map(|i| if a.is_null(i) { Value::Null } else { int_json(a.value(i)) }));
        }
        DataType::Binary => out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(hex(col.as_binary::<i32>().value(i))) })),
        DataType::LargeBinary => out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(hex(col.as_binary::<i64>().value(i))) })),
        DataType::BinaryView => out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(hex(col.as_binary_view().value(i))) })),
        DataType::FixedSizeBinary(_) => {
            out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(hex(col.as_fixed_size_binary().value(i))) }))
        }
        DataType::Utf8 => out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(col.as_string::<i32>().value(i)) })),
        _ => {
            let options = FormatOptions::default();
            let fmt = ArrayFormatter::try_new(col.as_ref(), &options)?;
            out.extend((0..n).map(|i| if null(i) { Value::Null } else { json!(fmt.value(i).to_string()) }));
        }
    }
    Ok(out)
}

/// `{columns, rows}` for a result set, at most `max_rows` rows.
pub fn result_json(schema: &SchemaRef, batches: &[RecordBatch], max_rows: usize) -> ApiResult<(Value, bool)> {
    let columns: Vec<Value> =
        schema.fields().iter().map(|f| json!({ "name": f.name(), "type": f.data_type().to_string() })).collect();
    let mut rows: Vec<Value> = Vec::new();
    let mut truncated = false;
    'outer: for b in batches {
        let cols: Vec<Vec<Value>> = b.columns().iter().map(column_json).collect::<ApiResult<_>>()?;
        let mut iters: Vec<_> = cols.into_iter().map(Vec::into_iter).collect();
        for _ in 0..b.num_rows() {
            if rows.len() >= max_rows {
                truncated = true;
                break 'outer;
            }
            rows.push(Value::Array(iters.iter_mut().map(|it| it.next().unwrap_or(Value::Null)).collect()));
        }
    }
    Ok((json!({ "columns": columns, "rows": rows }), truncated))
}

pub async fn run(cx: &QueryCx, req: &SqlRequest) -> ApiResult<Value> {
    let events = Arc::new(EventsTable::new(cx.scan.clone()));
    let ordered = events.ordered_flag();
    cx.ctx.register_table("events", events)?;
    cx.ctx.register_table("persons", Arc::new(PersonsTable::new(cx.snap.persons())))?;
    for f in udf::public_udfs() {
        cx.ctx.register_udf(f);
    }

    let statement = parse_single_select(&req.sql)?;
    let state = cx.ctx.state();
    let plan = state.statement_to_plan(statement).await?;
    SQLOptions::new().with_allow_ddl(false).with_allow_dml(false).with_allow_statements(false).verify_plan(&plan)?;
    let wants_order = check_plan(&plan)?;
    ordered.store(wants_order, Ordering::Relaxed);

    let max_rows = cx.engine.cfg.sql_max_rows;
    let limited = LogicalPlanBuilder::from(plan).limit(0, Some(max_rows + 1))?.build()?;
    let optimized = state.optimize(&limited)?;
    // The optimizer must not have introduced anything outside the allow-list.
    check_plan(&optimized)?;
    let physical = state.query_planner().create_physical_plan(&optimized, &state).await?;
    cx.note_plan(&physical);
    let schema = physical.schema();
    let batches = datafusion::physical_plan::collect(physical, cx.task_ctx()).await?;
    let (mut body, truncated) = result_json(&schema, &batches, max_rows)?;
    if truncated {
        body["truncated"] = json!(true);
    }
    Ok(body)
}
