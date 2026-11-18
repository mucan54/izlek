//! Per-query execution context: an isolated DataFusion session with its own
//! memory pool, bound to one project snapshot.

use crate::error::{ApiError, ApiResult};
use crate::filecache::QueryStats;
use crate::filter::Compiler;
use crate::scan::{plan_scan, EventsScanExec, ScanContext, ScanRequest, STORE_URL};
use crate::state::{Engine, Snapshot};
use arrow::array::RecordBatch;
use datafusion::error::DataFusionError;
use datafusion::execution::memory_pool::{GreedyMemoryPool, MemoryConsumer, MemoryPool, MemoryReservation};
use datafusion::execution::runtime_env::RuntimeEnvBuilder;
use datafusion::execution::{SendableRecordBatchStream, SessionStateBuilder, TaskContext};
use datafusion::physical_expr::expressions::Column as PhysColumn;
use datafusion::physical_expr::{LexOrdering, PhysicalSortExpr};
use datafusion::physical_plan::sorts::sort_preserving_merge::SortPreservingMergeExec;
use datafusion::physical_plan::{ExecutionPlan, ExecutionPlanProperties};
use datafusion::prelude::{SessionConfig, SessionContext};
use futures::StreamExt;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};

/// Bytes currently reserved by all running queries (`memory_pool_bytes`).
pub static POOL_BYTES: AtomicI64 = AtomicI64::new(0);

/// Greedy pool with a hard cap that also feeds the process-wide gauge.
#[derive(Debug)]
struct TrackedPool {
    inner: GreedyMemoryPool,
}

impl std::fmt::Display for TrackedPool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "la_query_pool({})", self.inner)
    }
}

impl MemoryPool for TrackedPool {
    fn name(&self) -> &str {
        "la_query_pool"
    }
    fn grow(&self, reservation: &MemoryReservation, additional: usize) {
        POOL_BYTES.fetch_add(additional as i64, Ordering::Relaxed);
        self.inner.grow(reservation, additional);
    }
    fn shrink(&self, reservation: &MemoryReservation, shrink: usize) {
        POOL_BYTES.fetch_sub(shrink as i64, Ordering::Relaxed);
        self.inner.shrink(reservation, shrink);
    }
    fn try_grow(&self, reservation: &MemoryReservation, additional: usize) -> datafusion::common::Result<()> {
        self.inner.try_grow(reservation, additional)?;
        POOL_BYTES.fetch_add(additional as i64, Ordering::Relaxed);
        Ok(())
    }
    fn reserved(&self) -> usize {
        self.inner.reserved()
    }
}

impl Drop for TrackedPool {
    fn drop(&mut self) {
        // Reservations dropped with the pool never call `shrink`.
        POOL_BYTES.fetch_sub(self.inner.reserved() as i64, Ordering::Relaxed);
    }
}

/// Everything an endpoint needs to plan and run one query.
pub struct QueryCx {
    pub engine: Arc<Engine>,
    pub snap: Arc<Snapshot>,
    pub stats: Arc<QueryStats>,
    pub ctx: SessionContext,
    pub scan: ScanContext,
    pub compiler: Compiler,
    pool: Arc<dyn MemoryPool>,
    /// Operator-name summaries of the plans that ran (slow-query log).
    plans: Mutex<Vec<String>>,
}

impl QueryCx {
    pub fn new(engine: &Arc<Engine>, snap: Arc<Snapshot>, stats: Arc<QueryStats>) -> ApiResult<Self> {
        let cfg = &engine.cfg;
        let pool: Arc<dyn MemoryPool> =
            Arc::new(TrackedPool { inner: GreedyMemoryPool::new(cfg.memory_bytes as usize) });
        let runtime = RuntimeEnvBuilder::new().with_memory_pool(Arc::clone(&pool)).build_arc()?;
        let url = url_of(STORE_URL)?;
        runtime.register_object_store(&url, engine.store.object_store());

        let mut config = SessionConfig::new()
            .with_target_partitions(cfg.partitions)
            .with_batch_size(8192)
            .with_information_schema(false)
            .with_default_catalog_and_schema("la", "q");
        {
            let o = config.options_mut();
            o.execution.parquet.pushdown_filters = cfg.pushdown_filters;
            o.execution.parquet.reorder_filters = true;
            o.execution.enable_recursive_ctes = false;
            o.optimizer.prefer_existing_sort = true;
            o.execution.time_zone = Some("+00:00".into());
        }
        let state = SessionStateBuilder::new().with_config(config).with_runtime_env(runtime).with_default_features().build();
        let ctx = SessionContext::new_with_state(state);
        let scan = ScanContext {
            snap: Arc::clone(&snap),
            files: Arc::clone(&engine.files),
            stats: Arc::clone(&stats),
            pushdown_filters: cfg.pushdown_filters,
            partitions: cfg.partitions,
        };
        let compiler = Compiler::new(snap.schema.columns.clone(), snap.persons());
        Ok(Self { engine: Arc::clone(engine), snap, stats, ctx, scan, compiler, pool, plans: Mutex::new(Vec::new()) })
    }

    pub fn task_ctx(&self) -> Arc<TaskContext> {
        self.ctx.task_ctx()
    }

    /// Plans a scan of the snapshot's events.
    pub fn plan(&self, req: &ScanRequest) -> ApiResult<Arc<EventsScanExec>> {
        let state = self.ctx.state();
        let plan = plan_scan(&self.scan, &state, req)?;
        self.note_plan(&(Arc::clone(&plan) as Arc<dyn ExecutionPlan>));
        Ok(plan)
    }

    /// Plans a scan against a narrower view of the snapshot (same versions).
    pub fn plan_with(&self, snap: Arc<Snapshot>, req: &ScanRequest) -> ApiResult<Arc<EventsScanExec>> {
        let state = self.ctx.state();
        let cx = ScanContext { snap, ..self.scan.clone() };
        let plan = plan_scan(&cx, &state, req)?;
        self.note_plan(&(Arc::clone(&plan) as Arc<dyn ExecutionPlan>));
        Ok(plan)
    }

    pub fn note_plan(&self, plan: &Arc<dyn ExecutionPlan>) {
        let mut plans = self.plans.lock().unwrap();
        if plans.len() < 8 {
            plans.push(plan_summary(plan));
        }
    }

    pub fn plan_summaries(&self) -> Vec<String> {
        self.plans.lock().unwrap().clone()
    }

    /// A reservation against the query's memory pool for operator state that
    /// lives outside DataFusion.
    pub fn reserve(&self, name: &str) -> MemBudget {
        MemBudget { reservation: MemoryConsumer::new(name.to_owned()).register(&self.pool) }
    }
}

fn url_of(s: &str) -> ApiResult<url::Url> {
    url::Url::parse(s).map_err(|e| ApiError::internal(format!("store url: {e}")))
}

/// Accounting handle for hand-written operator state.
pub struct MemBudget {
    reservation: MemoryReservation,
}

impl MemBudget {
    pub fn grow(&self, bytes: usize) -> ApiResult<()> {
        self.reservation.try_grow(bytes).map_err(|_| ApiError::memory_limit())
    }
}

/// Operator names only — never expressions or literals, which could carry
/// event content.
pub fn plan_summary(plan: &Arc<dyn ExecutionPlan>) -> String {
    fn walk(p: &Arc<dyn ExecutionPlan>, out: &mut String, depth: usize) {
        out.push_str(p.name());
        if let Some(scan) = (p.as_ref() as &dyn std::any::Any).downcast_ref::<EventsScanExec>() {
            out.push_str(&format!(
                "[parts={},files={},{}]",
                p.output_partitioning().partition_count(),
                scan.files(),
                scan.inner_summary()
            ));
        }
        let children = p.children();
        if !children.is_empty() && depth < 12 {
            out.push('(');
            for (i, c) in children.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                walk(c, out, depth + 1);
            }
            out.push(')');
        }
    }
    let mut s = String::new();
    walk(plan, &mut s, 0);
    s
}

/// Merges all partitions of `plan` (each sorted on `columns`) into one
/// stream sorted on `columns`.
pub fn merged_stream(
    plan: Arc<dyn ExecutionPlan>,
    columns: &[&str],
    task: Arc<TaskContext>,
) -> ApiResult<SendableRecordBatchStream> {
    if plan.output_partitioning().partition_count() <= 1 {
        return Ok(plan.execute(0, task)?);
    }
    let schema = plan.schema();
    let sort = columns
        .iter()
        .map(|c| Ok(PhysicalSortExpr::new_default(Arc::new(PhysColumn::new(c, schema.index_of(c)?)) as _)))
        .collect::<Result<Vec<_>, arrow::error::ArrowError>>()?;
    let ordering = LexOrdering::new(sort).ok_or_else(|| ApiError::internal("empty merge key"))?;
    let merge = SortPreservingMergeExec::new(ordering, plan);
    Ok(merge.execute(0, task)?)
}

/// Runs `fold` over every batch of every partition, one task per partition
/// (at most `parallel` at a time), and returns the per-partition states.
pub async fn fold_partitions<S, F, N>(
    plan: Arc<dyn ExecutionPlan>,
    task: Arc<TaskContext>,
    parallel: usize,
    new_state: N,
    fold: F,
) -> ApiResult<Vec<S>>
where
    S: Send + 'static,
    N: Fn() -> S,
    F: Fn(&mut S, &RecordBatch) -> ApiResult<()> + Send + Sync + 'static,
{
    let n = plan.output_partitioning().partition_count();
    let fold = Arc::new(fold);
    let mut jobs = futures::stream::iter((0..n).map(|p| {
        let plan = Arc::clone(&plan);
        let task = Arc::clone(&task);
        let fold = Arc::clone(&fold);
        let mut state = new_state();
        // Spawned so partitions use separate worker threads; aborted when the
        // query future is dropped (timeout, client gone).
        datafusion::common::runtime::SpawnedTask::spawn(async move {
            let mut stream = plan.execute(p, task)?;
            while let Some(batch) = stream.next().await {
                fold(&mut state, &batch?)?;
            }
            Ok::<S, ApiError>(state)
        })
    }))
    .buffer_unordered(parallel.max(1));
    let mut out = Vec::with_capacity(n);
    while let Some(joined) = jobs.next().await {
        let state = joined.map_err(|e| ApiError::from(DataFusionError::ExecutionJoin(Box::new(e))))??;
        out.push(state);
    }
    Ok(out)
}
