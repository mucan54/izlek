//! Scan layer (design §11.1, §9.2; `spec/query.md` §10): turns a project
//! snapshot plus a projection and filters into a physical plan over the
//! day manifests' Parquet files.
//!
//! Per file:
//!
//! ```text
//! Parquet (projection, row-group/page pruning, row filter incl. tombstones)
//!   → residual filter → projection
//! ```
//!
//! A file whose `identity_version` predates the newest identity link is read
//! twice with disjoint predicates: rows that keep their `person_key` stream
//! through in file order, rows whose `a:` key now maps to a user are
//! rewritten, re-filtered and sorted on the side. A day with more than one
//! stream is merged on `(person_key, ts, uuid)` and adjacent equal keys are
//! collapsed (dedupe). Single-file days cost nothing extra.
//!
//! The assembled plan is wrapped in one opaque leaf ([`EventsScanExec`]) so
//! the session's physical optimizer cannot repartition or re-sort inside it.

use crate::filecache::{CachedReaderFactory, FileCache, QueryStats};
use crate::state::Snapshot;
use arrow::array::{Array, ArrayRef, AsArray, BooleanArray, BooleanBuilder, RecordBatch, StringBuilder};
use arrow::datatypes::{DataType, Field, Schema, SchemaRef};
use async_trait::async_trait;
use datafusion::catalog::{Session, TableProvider};
use datafusion::common::tree_node::{Transformed, TreeNode, TreeNodeRecursion};
use datafusion::common::{Column, DFSchema, Result, ScalarValue};
use datafusion::datasource::listing::PartitionedFile;
use datafusion::datasource::physical_plan::{FileScanConfigBuilder, FileSource, ParquetSource};
use datafusion::datasource::source::DataSourceExec;
use datafusion::datasource::TableType;
use datafusion::execution::object_store::ObjectStoreUrl;
use datafusion::execution::{SendableRecordBatchStream, TaskContext};
use datafusion::logical_expr::simplify::SimplifyContext;
use datafusion::optimizer::simplify_expressions::ExprSimplifier;
use datafusion::logical_expr::{
    lit, ColumnarValue, Expr, ScalarFunctionArgs, ScalarUDF, ScalarUDFImpl, Signature, TableProviderFilterPushDown,
    Volatility,
};
use datafusion::physical_expr::expressions::Column as PhysColumn;
use datafusion::physical_expr::{EquivalenceProperties, LexOrdering, PhysicalExpr, PhysicalSortExpr};
use datafusion::physical_plan::empty::EmptyExec;
use datafusion::physical_plan::execution_plan::{Boundedness, EmissionType};
use datafusion::physical_plan::filter::FilterExec;
use datafusion::physical_plan::filter_pushdown::PushedDown;
use datafusion::physical_plan::projection::ProjectionExec;
use datafusion::physical_plan::sorts::sort::SortExec;
use datafusion::physical_plan::sorts::sort_preserving_merge::SortPreservingMergeExec;
use datafusion::physical_plan::stream::RecordBatchStreamAdapter;
use datafusion::physical_plan::union::UnionExec;
use datafusion::physical_plan::{DisplayAs, DisplayFormatType, ExecutionPlan, Partitioning, PlanProperties};
use datafusion::prelude::ident;
use futures::{StreamExt, TryStreamExt};
use la_core::identity::IdentityMap;
use la_core::manifest::{DayManifest, FileEntry};
use la_core::schema::events_schema;
use la_core::time;
use std::collections::BTreeSet;
use std::fmt;
use std::hash::{Hash, Hasher};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

/// URL the query's object store is registered under in the runtime.
pub const STORE_URL: &str = "la://store";

/// Everything a scan needs besides the request itself.
#[derive(Debug, Clone)]
pub struct ScanContext {
    pub snap: Arc<Snapshot>,
    pub files: Arc<FileCache>,
    pub stats: Arc<QueryStats>,
    pub pushdown_filters: bool,
    pub partitions: usize,
}

/// What to read.
#[derive(Debug, Clone, Default)]
pub struct ScanRequest {
    /// Output columns: expressions over the table schema with their names.
    pub exprs: Vec<(Expr, String)>,
    /// Conjuncts over the table schema (unqualified column references).
    pub filters: Vec<Expr>,
    /// Keep every output partition sorted by `(person_key, ts, uuid)`.
    pub ordered: bool,
}

pub fn ts_lit(ms: i64) -> Expr {
    lit(ScalarValue::TimestampMillisecond(Some(ms), Some("UTC".into())))
}

/// Table schema for a snapshot: the current project schema (missing `p_*`
/// columns in older files read as null).
pub fn table_schema(snap: &Snapshot) -> SchemaRef {
    events_schema(&snap.schema.columns)
}

fn columns_of(e: &Expr) -> BTreeSet<String> {
    e.column_refs().into_iter().map(|c| c.name.clone()).collect()
}

/// Drops table qualifiers so expressions resolve against the bare table schema.
pub fn unqualify(e: Expr) -> Result<Expr> {
    e.transform(|x| match x {
        Expr::Column(c) if c.relation.is_some() => Ok(Transformed::yes(Expr::Column(Column::new_unqualified(c.name)))),
        other => Ok(Transformed::no(other)),
    })
    .map(|t| t.data)
}

// ---------------------------------------------------------------------------
// Identity rewrite functions
// ---------------------------------------------------------------------------

/// `la_linked(person_key)` / `la_rewrite(person_key)`: whether an `a:` key has
/// a link newer than the file's identity version, and the `u:` key it maps to.
#[derive(Debug)]
struct IdentityFn {
    map: Arc<IdentityMap>,
    after: u64,
    rewrite: bool,
    signature: Signature,
}

impl IdentityFn {
    fn udf(map: &Arc<IdentityMap>, after: u64, rewrite: bool) -> Arc<ScalarUDF> {
        Arc::new(ScalarUDF::new_from_impl(Self {
            map: Arc::clone(map),
            after,
            rewrite,
            signature: Signature::exact(vec![DataType::Utf8], Volatility::Immutable),
        }))
    }
}

impl PartialEq for IdentityFn {
    fn eq(&self, o: &Self) -> bool {
        Arc::ptr_eq(&self.map, &o.map) && self.after == o.after && self.rewrite == o.rewrite
    }
}
impl Eq for IdentityFn {}
impl Hash for IdentityFn {
    fn hash<H: Hasher>(&self, state: &mut H) {
        (Arc::as_ptr(&self.map) as usize, self.after, self.rewrite).hash(state);
    }
}

impl ScalarUDFImpl for IdentityFn {
    fn name(&self) -> &str {
        if self.rewrite {
            "la_rewrite"
        } else {
            "la_linked"
        }
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, _: &[DataType]) -> Result<DataType> {
        Ok(if self.rewrite { DataType::Utf8 } else { DataType::Boolean })
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let input = match &args.args[0] {
            ColumnarValue::Array(a) => Arc::clone(a),
            ColumnarValue::Scalar(s) => s.to_array_of_size(n)?,
        };
        let keys = input.as_string::<i32>();
        // Rows of one person are adjacent: remember the previous lookup.
        let mut last: Option<(&str, Option<&str>)> = None;
        let mut resolve = |i: usize| -> Option<&str> {
            if keys.is_null(i) {
                return None;
            }
            let pk = keys.value(i);
            if let Some((p, v)) = last {
                if p == pk {
                    return v;
                }
            }
            let v = pk.strip_prefix("a:").and_then(|anon| self.map.resolve_after(anon, self.after));
            last = Some((pk, v));
            v
        };
        let out: ArrayRef = if self.rewrite {
            let mut b = StringBuilder::with_capacity(n, n * 16);
            let mut buf = String::new();
            for i in 0..n {
                match resolve(i) {
                    Some(user) => {
                        buf.clear();
                        buf.push_str("u:");
                        buf.push_str(user);
                        b.append_value(&buf);
                    }
                    None if keys.is_null(i) => b.append_null(),
                    None => b.append_value(keys.value(i)),
                }
            }
            Arc::new(b.finish())
        } else {
            let mut b = BooleanBuilder::with_capacity(n);
            for i in 0..n {
                b.append_value(resolve(i).is_some());
            }
            Arc::new(b.finish())
        };
        Ok(ColumnarValue::Array(out))
    }
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

/// Drops rows whose `(person_key, ts, uuid)` equals the previous row's
/// (`spec/query.md` §10). Input: one partition sorted on those columns.
#[derive(Debug)]
pub struct DedupExec {
    input: Arc<dyn ExecutionPlan>,
    /// Indices of `person_key`, `ts`, `uuid` in the input schema.
    key: [usize; 3],
    props: Arc<PlanProperties>,
}

impl DedupExec {
    pub fn new(input: Arc<dyn ExecutionPlan>, key: [usize; 3]) -> Self {
        let props = Arc::new(PlanProperties::new(
            input.properties().eq_properties.clone(),
            Partitioning::UnknownPartitioning(1),
            EmissionType::Incremental,
            Boundedness::Bounded,
        ));
        Self { input, key, props }
    }
}

impl DisplayAs for DedupExec {
    fn fmt_as(&self, _t: DisplayFormatType, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "DedupExec")
    }
}

/// Key of the last row seen, carried across batch boundaries.
#[derive(Default)]
struct LastKey {
    person: String,
    ts: i64,
    uuid: Vec<u8>,
    set: bool,
}

fn dedup_batch(batch: RecordBatch, key: [usize; 3], last: &mut LastKey) -> Result<RecordBatch> {
    let n = batch.num_rows();
    if n == 0 {
        return Ok(batch);
    }
    let person = batch.column(key[0]).as_string::<i32>();
    let ts = batch.column(key[1]).as_primitive::<arrow::datatypes::TimestampMillisecondType>();
    let uuid = batch.column(key[2]).as_fixed_size_binary();
    let same = |i: usize, j: usize| ts.value(i) == ts.value(j) && uuid.value(i) == uuid.value(j) && person.value(i) == person.value(j);
    let first_dup = last.set && ts.value(0) == last.ts && uuid.value(0) == last.uuid.as_slice() && person.value(0) == last.person;
    let mut dups = usize::from(first_dup);
    let mut keep = BooleanBuilder::with_capacity(n);
    keep.append_value(!first_dup);
    for i in 1..n {
        let d = same(i, i - 1);
        dups += usize::from(d);
        keep.append_value(!d);
    }
    last.person.clear();
    last.person.push_str(person.value(n - 1));
    last.ts = ts.value(n - 1);
    last.uuid.clear();
    last.uuid.extend_from_slice(uuid.value(n - 1));
    last.set = true;
    if dups == 0 {
        return Ok(batch);
    }
    let mask: BooleanArray = keep.finish();
    Ok(arrow::compute::filter_record_batch(&batch, &mask)?)
}

impl ExecutionPlan for DedupExec {
    fn name(&self) -> &str {
        "DedupExec"
    }
    fn properties(&self) -> &Arc<PlanProperties> {
        &self.props
    }
    fn children(&self) -> Vec<&Arc<dyn ExecutionPlan>> {
        vec![&self.input]
    }
    fn maintains_input_order(&self) -> Vec<bool> {
        vec![true]
    }
    fn apply_expressions(
        &self,
        _f: &mut dyn FnMut(&Arc<dyn PhysicalExpr>) -> Result<TreeNodeRecursion>,
    ) -> Result<TreeNodeRecursion> {
        Ok(TreeNodeRecursion::Continue)
    }
    fn with_new_children(self: Arc<Self>, mut children: Vec<Arc<dyn ExecutionPlan>>) -> Result<Arc<dyn ExecutionPlan>> {
        Ok(Arc::new(DedupExec::new(children.swap_remove(0), self.key)))
    }
    fn execute(&self, partition: usize, context: Arc<TaskContext>) -> Result<SendableRecordBatchStream> {
        let input = self.input.execute(partition, context)?;
        let key = self.key;
        let mut last = LastKey::default();
        let stream = input.map(move |b| dedup_batch(b?, key, &mut last)).try_filter(|b| futures::future::ready(b.num_rows() > 0));
        Ok(Box::pin(RecordBatchStreamAdapter::new(self.schema(), stream)))
    }
}

// ---------------------------------------------------------------------------
// Opaque scan leaf
// ---------------------------------------------------------------------------

/// The assembled scan. Each unit is a single-partition plan for one day.
/// Ordered scans expose one partition per day (each sorted); unordered scans
/// chain days into at most `partitions` output partitions.
#[derive(Debug)]
pub struct EventsScanExec {
    units: Vec<Arc<dyn ExecutionPlan>>,
    /// Output partition → unit indices, read one after another.
    groups: Vec<Vec<usize>>,
    ordered: bool,
    files: usize,
    props: Arc<PlanProperties>,
}

impl EventsScanExec {
    fn new(
        schema: SchemaRef,
        units: Vec<Arc<dyn ExecutionPlan>>,
        weights: Vec<u64>,
        ordered: bool,
        partitions: usize,
        files: usize,
    ) -> Result<Self> {
        let groups: Vec<Vec<usize>> = if units.is_empty() {
            vec![vec![]]
        } else if ordered {
            (0..units.len()).map(|i| vec![i]).collect()
        } else {
            // Heaviest first onto the lightest partition.
            let n = partitions.clamp(1, units.len());
            let mut order: Vec<usize> = (0..units.len()).collect();
            order.sort_by_key(|i| std::cmp::Reverse(weights[*i]));
            let mut groups = vec![Vec::new(); n];
            let mut load = vec![0u64; n];
            for i in order {
                let g = (0..n).min_by_key(|g| load[*g]).unwrap_or(0);
                load[g] += weights[i].max(1);
                groups[g].push(i);
            }
            groups
        };
        let mut eq = EquivalenceProperties::new(Arc::clone(&schema));
        if ordered {
            let mut sort = Vec::new();
            for name in ["person_key", "ts", "uuid"] {
                match schema.index_of(name) {
                    Ok(i) => sort.push(PhysicalSortExpr::new_default(Arc::new(PhysColumn::new(name, i)))),
                    Err(_) => break,
                }
            }
            if let Some(ordering) = LexOrdering::new(sort) {
                eq.add_ordering(ordering);
            }
        }
        let props = PlanProperties::new(
            eq,
            Partitioning::UnknownPartitioning(groups.len()),
            EmissionType::Incremental,
            Boundedness::Bounded,
        );
        Ok(Self { units, groups, ordered, files, props: Arc::new(props) })
    }

    pub fn files(&self) -> usize {
        self.files
    }

    /// Operator names of the hidden per-day plans, for the slow-query log.
    pub fn inner_summary(&self) -> String {
        let mut counts: std::collections::BTreeMap<String, usize> = std::collections::BTreeMap::new();
        fn walk(p: &Arc<dyn ExecutionPlan>, counts: &mut std::collections::BTreeMap<String, usize>) {
            *counts.entry(p.name().to_owned()).or_default() += 1;
            for c in p.children() {
                walk(c, counts);
            }
        }
        for u in &self.units {
            walk(u, &mut counts);
        }
        counts.iter().map(|(k, v)| format!("{k}×{v}")).collect::<Vec<_>>().join(",")
    }
}

impl DisplayAs for EventsScanExec {
    fn fmt_as(&self, _t: DisplayFormatType, f: &mut fmt::Formatter) -> fmt::Result {
        write!(
            f,
            "EventsScanExec: days={}, files={}, partitions={}, ordered={}",
            self.units.len(),
            self.files,
            self.groups.len(),
            self.ordered
        )
    }
}

impl ExecutionPlan for EventsScanExec {
    fn name(&self) -> &str {
        "EventsScanExec"
    }
    fn properties(&self) -> &Arc<PlanProperties> {
        &self.props
    }
    fn children(&self) -> Vec<&Arc<dyn ExecutionPlan>> {
        vec![]
    }
    fn apply_expressions(
        &self,
        _f: &mut dyn FnMut(&Arc<dyn PhysicalExpr>) -> Result<TreeNodeRecursion>,
    ) -> Result<TreeNodeRecursion> {
        Ok(TreeNodeRecursion::Continue)
    }
    fn with_new_children(self: Arc<Self>, _children: Vec<Arc<dyn ExecutionPlan>>) -> Result<Arc<dyn ExecutionPlan>> {
        Ok(self)
    }
    fn execute(&self, partition: usize, context: Arc<TaskContext>) -> Result<SendableRecordBatchStream> {
        let units: Vec<Arc<dyn ExecutionPlan>> =
            self.groups.get(partition).map(|g| g.iter().map(|i| Arc::clone(&self.units[*i])).collect()).unwrap_or_default();
        // Units are opened lazily, one after another.
        let stream = futures::stream::iter(units).map(move |u| u.execute(0, Arc::clone(&context))).try_flatten();
        Ok(Box::pin(RecordBatchStreamAdapter::new(self.schema(), stream)))
    }
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, PartialEq, Eq)]
enum Variant {
    /// No identity rewrite needed.
    Plain,
    /// Rows that keep their key (file order preserved).
    Unlinked(u64),
    /// `a:` rows with a newer link: rewritten and sorted.
    Linked(u64),
}

struct DayFilters {
    /// Tombstone conjuncts for this day's manifest.
    tombstones: Vec<Expr>,
}

fn string_list(items: impl IntoIterator<Item = String>) -> Vec<Expr> {
    let mut v: Vec<String> = items.into_iter().collect();
    v.sort();
    v.into_iter().map(lit).collect()
}

fn tombstone_filters(snap: &Snapshot, after_ts: i64) -> Vec<Expr> {
    let set = snap.tombstone_set(after_ts);
    if set.is_empty() {
        return vec![];
    }
    let mut out = Vec::new();
    let users: Vec<String> =
        set.person_keys.iter().filter_map(|k| k.strip_prefix("u:")).filter(|u| !u.is_empty()).map(str::to_owned).collect();
    if !set.person_keys.is_empty() {
        out.push(ident("person_key").in_list(string_list(set.person_keys.iter().cloned()), true));
    }
    if !set.anon_ids.is_empty() {
        out.push(ident("anon_id").in_list(string_list(set.anon_ids.iter().cloned()), true));
    }
    if !users.is_empty() {
        out.push(ident("user_id").is_null().or(ident("user_id").in_list(string_list(users), true)));
    }
    out
}

fn sort_exprs(schema: &Schema, names: &[&str]) -> Result<LexOrdering> {
    let exprs = names
        .iter()
        .map(|n| Ok(PhysicalSortExpr::new_default(Arc::new(PhysColumn::new(n, schema.index_of(n)?)) as _)))
        .collect::<Result<Vec<_>>>()?;
    LexOrdering::new(exprs).ok_or_else(|| datafusion::error::DataFusionError::Internal("empty ordering".into()))
}

const H_PERSON: &str = "__la_person_key";
const H_TS: &str = "__la_ts";
const H_UUID: &str = "__la_uuid";

/// Coerces, simplifies (constant folding, `coalesce` → `CASE`) and lowers a
/// logical expression against `schema`.
pub fn to_physical(state: &dyn Session, e: &Expr, schema: &SchemaRef) -> Result<Arc<dyn PhysicalExpr>> {
    let df = Arc::new(DFSchema::try_from(Arc::clone(schema))?);
    let context = SimplifyContext::builder()
        .with_schema(Arc::clone(&df))
        .with_config_options(Arc::new(state.config_options().clone()))
        .with_query_execution_start_time(state.execution_props().query_execution_start_time)
        .build();
    let simplifier = ExprSimplifier::new(context);
    let e = simplifier.coerce(e.clone(), &df)?;
    let e = simplifier.simplify(e)?;
    state.create_physical_expr(e, &df)
}

struct Planner<'a> {
    cx: &'a ScanContext,
    state: &'a dyn Session,
    table: SchemaRef,
    /// Filters that do not look at `person_key` (safe before a rewrite).
    filters_other: Vec<Expr>,
    /// Filters that depend on `person_key` (must see the rewritten key).
    filters_person: Vec<Expr>,
    ordered: bool,
}

impl Planner<'_> {
    fn physical(&self, e: &Expr, schema: &SchemaRef) -> Result<Arc<dyn PhysicalExpr>> {
        to_physical(self.state, e, schema)
    }

    /// One stream over one file. `layout` is the list of output expressions.
    fn file_stream(
        &self,
        file: &FileEntry,
        day: &DayFilters,
        layout: &[(Expr, String)],
        variant: Variant,
        keep_order: bool,
    ) -> Result<Arc<dyn ExecutionPlan>> {
        let snap = &self.cx.snap;
        // Conjuncts evaluated against the stored (pre-rewrite) row.
        let mut scan_filters: Vec<Expr> = day.tombstones.clone();
        if file.min_ts < snap.from_ms {
            scan_filters.push(ident("ts").gt_eq(ts_lit(snap.from_ms)));
        }
        if file.max_ts >= snap.to_ms {
            scan_filters.push(ident("ts").lt(ts_lit(snap.to_ms)));
        }
        scan_filters.extend(self.filters_other.iter().cloned());
        match variant {
            Variant::Plain => scan_filters.extend(self.filters_person.iter().cloned()),
            Variant::Unlinked(iv) => {
                scan_filters.extend(self.filters_person.iter().cloned());
                let linked = IdentityFn::udf(&snap.identity, iv, false).call(vec![ident("person_key")]);
                scan_filters.push(datafusion::logical_expr::not(linked));
            }
            Variant::Linked(iv) => {
                // Unresolved keys sort first ("a:" < "u:"), so the range bound
                // lets row-group statistics skip the rest of the file.
                scan_filters.push(ident("person_key").lt(lit("a;")));
                scan_filters.push(IdentityFn::udf(&snap.identity, iv, false).call(vec![ident("person_key")]));
            }
        }
        let late_filters: &[Expr] = if matches!(variant, Variant::Linked(_)) { &self.filters_person } else { &[] };

        // Columns to decode.
        let mut needed: BTreeSet<String> = BTreeSet::new();
        for e in scan_filters.iter().chain(late_filters).chain(layout.iter().map(|(e, _)| e)) {
            needed.extend(columns_of(e));
        }
        if matches!(variant, Variant::Linked(_)) {
            needed.insert("person_key".into());
        }
        let mut indices: Vec<usize> = needed.iter().map(|n| self.table.index_of(n)).collect::<Result<_, _>>()?;
        indices.sort_unstable();
        if indices.is_empty() {
            // `count(*)`-style scans still need a column to count rows.
            indices.push(self.table.index_of("flags")?);
        }

        let factory = Arc::new(CachedReaderFactory { cache: Arc::clone(&self.cx.files), stats: Arc::clone(&self.cx.stats) });
        let source = ParquetSource::new(Arc::clone(&self.table))
            .with_parquet_file_reader_factory(factory)
            .with_enable_page_index(true)
            .with_pushdown_filters(self.cx.pushdown_filters)
            .with_reorder_filters(true);
        let mut source: Arc<dyn FileSource> = Arc::new(source);
        let mut residual: Vec<Expr> = Vec::new();
        if !scan_filters.is_empty() {
            let physical = scan_filters.iter().map(|e| self.physical(e, &self.table)).collect::<Result<Vec<_>>>()?;
            let pushed = source.try_pushdown_filters(physical, self.state.config_options())?;
            for (e, p) in scan_filters.iter().zip(&pushed.filters) {
                if matches!(p, PushedDown::No) {
                    residual.push(e.clone());
                }
            }
            if let Some(updated) = pushed.updated_node {
                source = updated;
            }
        }
        let config = FileScanConfigBuilder::new(ObjectStoreUrl::parse(STORE_URL)?, source)
            .with_file(PartitionedFile::new(file.key.clone(), file.bytes))
            .with_projection_indices(Some(indices))?
            .with_preserve_order(keep_order)
            .build();
        let mut plan: Arc<dyn ExecutionPlan> = DataSourceExec::from_data_source(config);

        if let Some(pred) = residual.into_iter().reduce(Expr::and) {
            plan = Arc::new(FilterExec::try_new(self.physical(&pred, &plan.schema())?, plan)?);
        }
        if let Variant::Linked(iv) = variant {
            let schema = plan.schema();
            let rewrite = IdentityFn::udf(&snap.identity, iv, true).call(vec![ident("person_key")]);
            let exprs = schema
                .fields()
                .iter()
                .map(|f| {
                    let e = if f.name() == "person_key" { rewrite.clone() } else { ident(f.name().as_str()) };
                    Ok((self.physical(&e, &schema)?, f.name().clone()))
                })
                .collect::<Result<Vec<_>>>()?;
            plan = Arc::new(ProjectionExec::try_new(exprs, plan)?);
            if let Some(pred) = late_filters.iter().cloned().reduce(Expr::and) {
                plan = Arc::new(FilterExec::try_new(self.physical(&pred, &plan.schema())?, plan)?);
            }
        }
        let schema = plan.schema();
        let exprs =
            layout.iter().map(|(e, name)| Ok((self.physical(e, &schema)?, name.clone()))).collect::<Result<Vec<_>>>()?;
        plan = Arc::new(ProjectionExec::try_new(exprs, plan)?);
        if matches!(variant, Variant::Linked(_)) {
            let ordering = sort_exprs(&plan.schema(), &[H_PERSON, H_TS, H_UUID])?;
            plan = Arc::new(SortExec::new(ordering, plan));
        }
        Ok(plan)
    }

    /// Plan for one day: `(single-partition plan, files read)`, or `None`
    /// when every file is pruned.
    fn day(
        &self,
        manifest: &DayManifest,
        out: &[(Expr, String)],
        out_schema: &SchemaRef,
    ) -> Result<Option<(Arc<dyn ExecutionPlan>, usize, u64)>> {
        let snap = &self.cx.snap;
        let files: Vec<&FileEntry> =
            manifest.files.iter().filter(|f| f.rows > 0 && f.max_ts >= snap.from_ms && f.min_ts < snap.to_ms).collect();
        if files.is_empty() {
            return Ok(None);
        }
        let day = DayFilters { tombstones: tombstone_filters(snap, manifest.tombstone_ts) };
        let mut streams: Vec<(&FileEntry, Variant)> = Vec::new();
        for f in &files {
            // "a:" keys sort before everything else, so a file whose smallest
            // key is already past them has nothing to rewrite.
            let has_unresolved = f.min_person.as_str() < "a;";
            if f.identity_version < snap.max_link_version && has_unresolved {
                streams.push((f, Variant::Unlinked(f.identity_version)));
                streams.push((f, Variant::Linked(f.identity_version)));
            } else {
                streams.push((f, Variant::Plain));
            }
        }
        let rows: u64 = files.iter().map(|f| f.rows).sum();
        if streams.len() == 1 {
            let plan = self.file_stream(streams[0].0, &day, out, Variant::Plain, self.ordered)?;
            return Ok(Some((plan, 1, rows)));
        }
        // Several streams: merge on the key and collapse duplicates.
        let mut layout: Vec<(Expr, String)> = vec![
            (ident("person_key"), H_PERSON.to_owned()),
            (ident("ts"), H_TS.to_owned()),
            (ident("uuid"), H_UUID.to_owned()),
        ];
        let mut mapping: Vec<usize> = Vec::with_capacity(out.len());
        for (e, name) in out {
            let helper = match e {
                Expr::Column(c) => ["person_key", "ts", "uuid"].iter().position(|h| *h == c.name),
                _ => None,
            };
            match helper {
                Some(i) => mapping.push(i),
                None => {
                    mapping.push(layout.len());
                    layout.push((e.clone(), format!("__la_c{}_{name}", layout.len())));
                }
            }
        }
        let inputs = streams
            .iter()
            .map(|(f, v)| self.file_stream(f, &day, &layout, *v, true))
            .collect::<Result<Vec<_>>>()?;
        let union = UnionExec::try_new(inputs)?;
        let ordering = sort_exprs(&union.schema(), &[H_PERSON, H_TS, H_UUID])?;
        let merged: Arc<dyn ExecutionPlan> = Arc::new(SortPreservingMergeExec::new(ordering, union));
        let deduped: Arc<dyn ExecutionPlan> = Arc::new(DedupExec::new(merged, [0, 1, 2]));
        let schema = deduped.schema();
        let exprs: Vec<(Arc<dyn PhysicalExpr>, String)> = mapping
            .iter()
            .zip(out_schema.fields())
            .map(|(i, f)| (Arc::new(PhysColumn::new(schema.field(*i).name(), *i)) as Arc<dyn PhysicalExpr>, f.name().clone()))
            .collect();
        let plan: Arc<dyn ExecutionPlan> = Arc::new(ProjectionExec::try_new(exprs, deduped)?);
        Ok(Some((plan, files.len(), rows)))
    }
}

/// Builds the physical scan for `req`.
pub fn plan_scan(cx: &ScanContext, state: &dyn Session, req: &ScanRequest) -> Result<Arc<EventsScanExec>> {
    let table = table_schema(&cx.snap);
    let (filters_person, filters_other): (Vec<Expr>, Vec<Expr>) =
        req.filters.iter().cloned().partition(|e| columns_of(e).contains("person_key"));
    let planner = Planner { cx, state, table: Arc::clone(&table), filters_other, filters_person, ordered: req.ordered };

    // Output schema, derived from the expressions.
    let mut fields = Vec::with_capacity(req.exprs.len());
    for (e, name) in &req.exprs {
        let p = to_physical(state, e, &table)?;
        fields.push(Field::new(name, p.data_type(&table)?, p.nullable(&table)?));
    }
    let out_schema: SchemaRef = Arc::new(Schema::new(fields));

    let mut units = Vec::new();
    let mut weights = Vec::new();
    let mut files = 0usize;
    for manifest in &cx.snap.days {
        if let Some((plan, n, rows)) = planner.day(manifest, &req.exprs, &out_schema)? {
            // Per-day plans may disagree on nullability with the declared
            // schema only in the safe direction; align the schema exactly.
            units.push(align_schema(plan, &out_schema)?);
            weights.push(rows);
            files += n;
        }
    }
    if units.is_empty() {
        units.push(Arc::new(EmptyExec::new(Arc::clone(&out_schema))));
        weights.push(0);
    }
    Ok(Arc::new(EventsScanExec::new(out_schema, units, weights, req.ordered, cx.partitions, files)?))
}

/// Wraps `plan` so its batches carry exactly `schema` (same types; field
/// nullability and names as declared).
fn align_schema(plan: Arc<dyn ExecutionPlan>, schema: &SchemaRef) -> Result<Arc<dyn ExecutionPlan>> {
    if plan.schema().as_ref() == schema.as_ref() {
        return Ok(plan);
    }
    Ok(Arc::new(RelabelExec { props: relabel_props(&plan, schema), input: plan }))
}

fn relabel_props(input: &Arc<dyn ExecutionPlan>, schema: &SchemaRef) -> Arc<PlanProperties> {
    Arc::new(PlanProperties::new(
        EquivalenceProperties::new(Arc::clone(schema)),
        input.properties().partitioning.clone(),
        EmissionType::Incremental,
        Boundedness::Bounded,
    ))
}

/// Re-labels batches with a schema that differs only in field metadata /
/// nullability.
#[derive(Debug)]
struct RelabelExec {
    input: Arc<dyn ExecutionPlan>,
    props: Arc<PlanProperties>,
}

impl DisplayAs for RelabelExec {
    fn fmt_as(&self, _t: DisplayFormatType, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "RelabelExec")
    }
}

impl ExecutionPlan for RelabelExec {
    fn name(&self) -> &str {
        "RelabelExec"
    }
    fn properties(&self) -> &Arc<PlanProperties> {
        &self.props
    }
    fn children(&self) -> Vec<&Arc<dyn ExecutionPlan>> {
        vec![&self.input]
    }
    fn maintains_input_order(&self) -> Vec<bool> {
        vec![true]
    }
    fn apply_expressions(
        &self,
        _f: &mut dyn FnMut(&Arc<dyn PhysicalExpr>) -> Result<TreeNodeRecursion>,
    ) -> Result<TreeNodeRecursion> {
        Ok(TreeNodeRecursion::Continue)
    }
    fn with_new_children(self: Arc<Self>, mut children: Vec<Arc<dyn ExecutionPlan>>) -> Result<Arc<dyn ExecutionPlan>> {
        let input = children.swap_remove(0);
        Ok(Arc::new(RelabelExec { props: relabel_props(&input, &self.schema()), input }))
    }
    fn execute(&self, partition: usize, context: Arc<TaskContext>) -> Result<SendableRecordBatchStream> {
        let schema = self.schema();
        let target = Arc::clone(&schema);
        let stream = self.input.execute(partition, context)?.map(move |b| {
            let b = b?;
            Ok(RecordBatch::try_new_with_options(
                Arc::clone(&target),
                b.columns().to_vec(),
                &arrow::record_batch::RecordBatchOptions::new().with_row_count(Some(b.num_rows())),
            )?)
        });
        Ok(Box::pin(RecordBatchStreamAdapter::new(schema, stream)))
    }
}

// ---------------------------------------------------------------------------
// Table providers
// ---------------------------------------------------------------------------

/// `events` for SQL: the project's files within the snapshot's time range.
#[derive(Debug)]
pub struct EventsTable {
    cx: ScanContext,
    schema: SchemaRef,
    /// Whether scans should keep (and declare) the file order. Decided per
    /// query from the logical plan before physical planning.
    ordered: Arc<AtomicBool>,
}

impl EventsTable {
    pub fn new(cx: ScanContext) -> Self {
        let schema = table_schema(&cx.snap);
        Self { cx, schema, ordered: Arc::new(AtomicBool::new(false)) }
    }

    pub fn ordered_flag(&self) -> Arc<AtomicBool> {
        Arc::clone(&self.ordered)
    }
}

fn pushable(e: &Expr) -> bool {
    !e.is_volatile() && !e.exists(|x| Ok(matches!(x, Expr::ScalarSubquery(_) | Expr::Exists(_) | Expr::InSubquery(_) | Expr::OuterReferenceColumn(..) | Expr::Placeholder(_)))).unwrap_or(true)
}

#[async_trait]
impl TableProvider for EventsTable {
    fn schema(&self) -> SchemaRef {
        Arc::clone(&self.schema)
    }

    fn table_type(&self) -> TableType {
        TableType::Base
    }

    fn supports_filters_pushdown(&self, filters: &[&Expr]) -> Result<Vec<TableProviderFilterPushDown>> {
        Ok(filters
            .iter()
            .map(|f| if pushable(f) { TableProviderFilterPushDown::Exact } else { TableProviderFilterPushDown::Unsupported })
            .collect())
    }

    async fn scan(
        &self,
        state: &dyn Session,
        projection: Option<&Vec<usize>>,
        filters: &[Expr],
        _limit: Option<usize>,
    ) -> Result<Arc<dyn ExecutionPlan>> {
        let indices: Vec<usize> = match projection {
            Some(p) => p.clone(),
            None => (0..self.schema.fields().len()).collect(),
        };
        let exprs =
            indices.iter().map(|i| (ident(self.schema.field(*i).name().as_str()), self.schema.field(*i).name().clone())).collect();
        let filters = filters.iter().cloned().map(unqualify).collect::<Result<Vec<_>>>()?;
        let req = ScanRequest { exprs, filters, ordered: self.ordered.load(Ordering::Relaxed) };
        let plan = plan_scan(&self.cx, state, &req)?;
        self.cx.stats.files.fetch_add(plan.files() as u64, Ordering::Relaxed);
        Ok(plan)
    }
}

/// `persons(person_key, props)` for SQL: current properties as one JSON
/// object per person, materialized only when the table is actually scanned.
/// Persons named by a tombstone are left out (design §10.5: excluded from
/// results immediately, before the compactor has removed their rows).
#[derive(Debug)]
pub struct PersonsTable {
    table: Arc<la_core::persons::PersonTable>,
    hidden: std::collections::HashSet<String>,
    schema: SchemaRef,
}

impl PersonsTable {
    pub fn new(snap: &Snapshot) -> Self {
        let schema = Arc::new(Schema::new(vec![
            Field::new("person_key", DataType::Utf8, false),
            Field::new("props", DataType::Utf8, false),
        ]));
        Self { table: snap.persons(), hidden: snap.tombstone_set(0).person_keys, schema }
    }
}

#[async_trait]
impl TableProvider for PersonsTable {
    fn schema(&self) -> SchemaRef {
        Arc::clone(&self.schema)
    }

    fn table_type(&self) -> TableType {
        TableType::Base
    }

    async fn scan(
        &self,
        _state: &dyn Session,
        projection: Option<&Vec<usize>>,
        _filters: &[Expr],
        _limit: Option<usize>,
    ) -> Result<Arc<dyn ExecutionPlan>> {
        let mut keys: Vec<&String> = self.table.iter().map(|(k, _)| k).filter(|k| !self.hidden.contains(*k)).collect();
        keys.sort();
        let mut pk = StringBuilder::new();
        let mut props = StringBuilder::new();
        for k in keys {
            pk.append_value(k);
            props.append_value(self.table.person_json(k));
        }
        let batch = RecordBatch::try_new(Arc::clone(&self.schema), vec![Arc::new(pk.finish()), Arc::new(props.finish())])?;
        let exec = datafusion::datasource::memory::MemorySourceConfig::try_new_exec(
            &[vec![batch]],
            Arc::clone(&self.schema),
            projection.cloned(),
        )?;
        Ok(exec)
    }
}

/// Day (UTC) of a timestamp, for callers that iterate days themselves.
pub fn day_bounds(day: &str) -> Option<(i64, i64)> {
    time::day_start_ms(day).map(|s| (s, s + time::DAY_MS))
}
