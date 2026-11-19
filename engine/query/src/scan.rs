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
use datafusion::logical_expr::{
    lit, ColumnarValue, Expr, ScalarFunctionArgs, ScalarUDF, ScalarUDFImpl, Signature, TableProviderFilterPushDown,
    Volatility,
};
use datafusion::optimizer::simplify_expressions::ExprSimplifier;
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
use std::collections::{BTreeSet, HashSet};
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

/// What an [`IdentityFn`] computes for a stored `person_key`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
enum IdentityOp {
    /// `la_linked`: an `a:` key with a link newer than the file.
    Linked,
    /// `la_pending`: [`IdentityOp::Linked`], or a `u:` key that such a link
    /// points to (a person whose row set grows through the rewrite).
    Pending,
    /// `la_rewrite`: the key after applying the newer links.
    Rewrite,
}

/// Identity functions bound to one file's identity version.
#[derive(Debug)]
struct IdentityFn {
    map: Arc<IdentityMap>,
    after: u64,
    op: IdentityOp,
    /// Users gaining rows (only for [`IdentityOp::Pending`]).
    targets: Arc<std::collections::HashSet<String>>,
    signature: Signature,
}

impl IdentityFn {
    fn udf(snap: &Snapshot, after: u64, op: IdentityOp) -> Arc<ScalarUDF> {
        let targets = if op == IdentityOp::Pending { snap.link_targets_after(after) } else { Default::default() };
        Arc::new(ScalarUDF::new_from_impl(Self {
            map: Arc::clone(&snap.identity),
            after,
            op,
            targets: Arc::new(targets),
            signature: Signature::exact(vec![DataType::Utf8], Volatility::Immutable),
        }))
    }
}

impl PartialEq for IdentityFn {
    fn eq(&self, o: &Self) -> bool {
        Arc::ptr_eq(&self.map, &o.map) && self.after == o.after && self.op == o.op
    }
}
impl Eq for IdentityFn {}
impl Hash for IdentityFn {
    fn hash<H: Hasher>(&self, state: &mut H) {
        (Arc::as_ptr(&self.map) as usize, self.after, self.op).hash(state);
    }
}

impl ScalarUDFImpl for IdentityFn {
    fn name(&self) -> &str {
        match self.op {
            IdentityOp::Linked => "la_linked",
            IdentityOp::Pending => "la_pending",
            IdentityOp::Rewrite => "la_rewrite",
        }
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, _: &[DataType]) -> Result<DataType> {
        Ok(if self.op == IdentityOp::Rewrite { DataType::Utf8 } else { DataType::Boolean })
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let input = match &args.args[0] {
            ColumnarValue::Array(a) => Arc::clone(a),
            ColumnarValue::Scalar(s) => s.to_array_of_size(n)?,
        };
        let keys = input.as_string::<i32>();
        let out: ArrayRef = match self.op {
            IdentityOp::Rewrite => {
                // Rows of one person are adjacent: remember the previous lookup.
                let mut last: Option<(&str, Option<&str>)> = None;
                let mut b = StringBuilder::with_capacity(n, n * 16);
                let mut buf = String::new();
                for i in 0..n {
                    if keys.is_null(i) {
                        b.append_null();
                        continue;
                    }
                    let pk = keys.value(i);
                    let user = match last {
                        Some((p, v)) if p == pk => v,
                        _ => {
                            let v = pk.strip_prefix("a:").and_then(|anon| self.map.resolve_after(anon, self.after));
                            last = Some((pk, v));
                            v
                        }
                    };
                    match user {
                        Some(user) => {
                            buf.clear();
                            buf.push_str("u:");
                            buf.push_str(user);
                            b.append_value(&buf);
                        }
                        None => b.append_value(pk),
                    }
                }
                Arc::new(b.finish())
            }
            IdentityOp::Linked | IdentityOp::Pending => {
                let pending = self.op == IdentityOp::Pending;
                let mut last: Option<(&str, bool)> = None;
                let mut b = BooleanBuilder::with_capacity(n);
                for i in 0..n {
                    if keys.is_null(i) {
                        b.append_value(false);
                        continue;
                    }
                    let pk = keys.value(i);
                    let hit = match last {
                        Some((p, v)) if p == pk => v,
                        _ => {
                            let v = match pk.strip_prefix("a:") {
                                Some(anon) => self.map.resolve_after(anon, self.after).is_some(),
                                None => pending && pk.strip_prefix("u:").is_some_and(|u| self.targets.contains(u)),
                            };
                            last = Some((pk, v));
                            v
                        }
                    };
                    b.append_value(hit);
                }
                Arc::new(b.finish())
            }
        };
        Ok(ColumnarValue::Array(out))
    }
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

/// Drops rows that repeat the `uuid` of an earlier row of the same person
/// (`spec/query.md` §10) — the rule compaction applies, so a day reads the
/// same before and after it is compacted. Input: one partition sorted on
/// `(person_key, ts, uuid)`, hence the copy that stays is the earliest.
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

/// The person the stream is currently in and the uuids seen for them,
/// carried across batch boundaries. Bounded by one person's rows in a day.
#[derive(Default)]
struct Seen {
    person: String,
    uuids: HashSet<[u8; 16]>,
    set: bool,
}

fn dedup_batch(batch: RecordBatch, key: [usize; 3], seen: &mut Seen) -> Result<RecordBatch> {
    let n = batch.num_rows();
    if n == 0 {
        return Ok(batch);
    }
    let person = batch.column(key[0]).as_string::<i32>();
    let uuid = batch.column(key[2]).as_fixed_size_binary();
    let mut dups = 0usize;
    let mut keep = BooleanBuilder::with_capacity(n);
    for i in 0..n {
        let p = person.value(i);
        if !seen.set || p != seen.person {
            seen.person.clear();
            seen.person.push_str(p);
            seen.uuids.clear();
            seen.set = true;
        }
        let mut id = [0u8; 16];
        id.copy_from_slice(uuid.value(i));
        let fresh = seen.uuids.insert(id);
        dups += usize::from(!fresh);
        keep.append_value(fresh);
    }
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
        let mut seen = Seen::default();
        let stream = input
            .map(move |b| dedup_batch(b?, key, &mut seen))
            .try_filter(|b| futures::future::ready(b.num_rows() > 0));
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
        let units: Vec<Arc<dyn ExecutionPlan>> = self
            .groups
            .get(partition)
            .map(|g| g.iter().map(|i| Arc::clone(&self.units[*i])).collect())
            .unwrap_or_default();
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
    /// Rows of persons the rewrite does not touch at all (file order preserved).
    Settled(u64),
    /// Every row of the persons it does touch — the `a:` rows being rewritten
    /// and the rows already stored under the target user: rewritten and sorted.
    Pending(u64),
}

impl Variant {
    /// Rows go through the key rewrite and a sort.
    fn rewrites(self) -> Option<u64> {
        match self {
            Variant::Linked(iv) | Variant::Pending(iv) => Some(iv),
            _ => None,
        }
    }
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
    let users: Vec<String> = set
        .person_keys
        .iter()
        .filter_map(|k| k.strip_prefix("u:"))
        .filter(|u| !u.is_empty())
        .map(str::to_owned)
        .collect();
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

/// What the `person_key` conjuncts say about the keys a row may have.
#[derive(Debug, Default, PartialEq)]
struct PersonBounds {
    /// Inclusive lower bound.
    lo: Option<String>,
    /// Upper bound and whether it is inclusive.
    hi: Option<(String, bool)>,
    /// Explicit candidate keys (`=` / `IN`), when any conjunct lists them.
    keys: Option<Vec<String>>,
}

fn utf8_literal(e: &Expr) -> Option<&str> {
    match e {
        Expr::Literal(
            ScalarValue::Utf8(Some(s)) | ScalarValue::LargeUtf8(Some(s)) | ScalarValue::Utf8View(Some(s)),
            _,
        ) => Some(s),
        _ => None,
    }
}

fn is_person_key(e: &Expr) -> bool {
    matches!(e, Expr::Column(c) if c.name == "person_key")
}

impl PersonBounds {
    /// Reads the bounds off top-level conjuncts of the shapes
    /// `person_key <op> 'lit'` and `person_key IN ('a', …)`; anything else
    /// (negations, disjunctions, functions) contributes nothing.
    fn from_filters(filters: &[Expr]) -> Self {
        use datafusion::logical_expr::{BinaryExpr, Operator};
        let mut b = PersonBounds::default();
        let tighten_lo = |b: &mut PersonBounds, v: &str| {
            if b.lo.as_deref().is_none_or(|cur| v > cur) {
                b.lo = Some(v.to_owned());
            }
        };
        let tighten_hi = |b: &mut PersonBounds, v: &str, inclusive: bool| {
            let tighter = match &b.hi {
                None => true,
                Some((cur, cur_inc)) => v < cur.as_str() || (v == cur && *cur_inc && !inclusive),
            };
            if tighter {
                b.hi = Some((v.to_owned(), inclusive));
            }
        };
        let restrict = |b: &mut PersonBounds, keys: Vec<String>| {
            b.keys = Some(match b.keys.take() {
                None => keys,
                Some(cur) => cur.into_iter().filter(|k| keys.contains(k)).collect(),
            });
        };
        let mut stack: Vec<&Expr> = filters.iter().collect();
        while let Some(e) = stack.pop() {
            match e {
                Expr::BinaryExpr(BinaryExpr { left, op: Operator::And, right }) => {
                    stack.push(left);
                    stack.push(right);
                }
                Expr::BinaryExpr(BinaryExpr { left, op, right }) if is_person_key(left) => {
                    let Some(v) = utf8_literal(right) else { continue };
                    match op {
                        Operator::Eq => restrict(&mut b, vec![v.to_owned()]),
                        Operator::GtEq | Operator::Gt => tighten_lo(&mut b, v),
                        Operator::Lt => tighten_hi(&mut b, v, false),
                        Operator::LtEq => tighten_hi(&mut b, v, true),
                        _ => {}
                    }
                }
                Expr::InList(list) if !list.negated && is_person_key(&list.expr) => {
                    let keys: Option<Vec<String>> =
                        list.list.iter().map(|x| utf8_literal(x).map(str::to_owned)).collect();
                    if let Some(keys) = keys {
                        restrict(&mut b, keys);
                    }
                }
                _ => {}
            }
        }
        b
    }

    /// Whether a file whose stored keys span `[min, max]` can hold a row
    /// satisfying the bounds.
    fn may_contain(&self, min: &str, max: &str) -> bool {
        if self.lo.as_deref().is_some_and(|lo| max < lo) {
            return false;
        }
        if let Some((hi, inclusive)) = &self.hi {
            if min > hi.as_str() || (min == hi && !inclusive) {
                return false;
            }
        }
        self.keys.as_ref().is_none_or(|keys| keys.iter().any(|k| min <= k.as_str() && k.as_str() <= max))
    }
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

/// `(single-partition plan, files read, rows in the day's files)`.
type DayPlan = (Arc<dyn ExecutionPlan>, usize, u64);

struct Planner<'a> {
    cx: &'a ScanContext,
    state: &'a dyn Session,
    table: SchemaRef,
    /// Filters that do not look at `person_key` (safe before a rewrite).
    filters_other: Vec<Expr>,
    /// Filters that depend on `person_key` (must see the rewritten key).
    filters_person: Vec<Expr>,
    /// Key range implied by `filters_person`, for file-level pruning.
    bounds: PersonBounds,
    /// Whether any output expression or filter looks at `person_key`.
    uses_person: bool,
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
                let linked = IdentityFn::udf(snap, iv, IdentityOp::Linked).call(vec![ident("person_key")]);
                scan_filters.push(datafusion::logical_expr::not(linked));
            }
            Variant::Linked(iv) => {
                // Unresolved keys sort first ("a:" < "u:"), so the range bound
                // lets row-group statistics skip the rest of the file.
                scan_filters.push(ident("person_key").lt(lit("a;")));
                scan_filters.push(IdentityFn::udf(snap, iv, IdentityOp::Linked).call(vec![ident("person_key")]));
            }
            Variant::Settled(iv) => {
                scan_filters.extend(self.filters_person.iter().cloned());
                let pending = IdentityFn::udf(snap, iv, IdentityOp::Pending).call(vec![ident("person_key")]);
                scan_filters.push(datafusion::logical_expr::not(pending));
            }
            Variant::Pending(iv) => {
                scan_filters.push(IdentityFn::udf(snap, iv, IdentityOp::Pending).call(vec![ident("person_key")]));
            }
        }
        let late_filters: &[Expr] = if variant.rewrites().is_some() { &self.filters_person } else { &[] };

        // Columns to decode.
        let mut needed: BTreeSet<String> = BTreeSet::new();
        for e in scan_filters.iter().chain(late_filters).chain(layout.iter().map(|(e, _)| e)) {
            needed.extend(columns_of(e));
        }
        if variant.rewrites().is_some() {
            needed.insert("person_key".into());
        }
        let mut indices: Vec<usize> = needed.iter().map(|n| self.table.index_of(n)).collect::<Result<_, _>>()?;
        indices.sort_unstable();
        if indices.is_empty() {
            // `count(*)`-style scans still need a column to count rows.
            indices.push(self.table.index_of("flags")?);
        }

        let factory =
            Arc::new(CachedReaderFactory { cache: Arc::clone(&self.cx.files), stats: Arc::clone(&self.cx.stats) });
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
        if let Some(iv) = variant.rewrites() {
            let schema = plan.schema();
            let rewrite = IdentityFn::udf(snap, iv, IdentityOp::Rewrite).call(vec![ident("person_key")]);
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
        let exprs = layout
            .iter()
            .map(|(e, name)| Ok((self.physical(e, &schema)?, name.clone())))
            .collect::<Result<Vec<_>>>()?;
        plan = Arc::new(ProjectionExec::try_new(exprs, plan)?);
        if variant.rewrites().is_some() {
            let ordering = sort_exprs(&plan.schema(), &[H_PERSON, H_TS, H_UUID])?;
            plan = Arc::new(SortExec::new(ordering, plan));
        }
        Ok(plan)
    }

    /// A day whose single file predates some identity links. No dedupe is
    /// needed (uuids are unique within a file), so only the persons the
    /// rewrite touches are re-sorted — together with the rows they already
    /// had — and everything else streams through in file order. The two
    /// streams hold disjoint persons, hence the merge needs `person_key` only
    /// and the untouched rows never pay for `ts`/`uuid`.
    fn single_file_rewrite(
        &self,
        file: &FileEntry,
        day: &DayFilters,
        out: &[(Expr, String)],
        out_schema: &SchemaRef,
    ) -> Result<Arc<dyn ExecutionPlan>> {
        let iv = file.identity_version;
        let mut layout: Vec<(Expr, String)> = vec![(ident("person_key"), H_PERSON.to_owned())];
        let mut mapping: Vec<usize> = Vec::with_capacity(out.len());
        for (e, name) in out {
            if matches!(e, Expr::Column(c) if c.name == "person_key") {
                mapping.push(0);
            } else {
                mapping.push(layout.len());
                layout.push((e.clone(), format!("__la_c{}_{name}", layout.len())));
            }
        }
        let settled = self.file_stream(file, day, &layout, Variant::Settled(iv), true)?;
        // The side stream carries the full sort key, dropped again after its sort.
        let mut side_layout =
            vec![layout[0].clone(), (ident("ts"), H_TS.to_owned()), (ident("uuid"), H_UUID.to_owned())];
        side_layout.extend(layout[1..].iter().cloned());
        let side = self.file_stream(file, day, &side_layout, Variant::Pending(iv), true)?;
        let side_schema = side.schema();
        let keep: Vec<(Arc<dyn PhysicalExpr>, String)> = std::iter::once(0)
            .chain(3..side_layout.len())
            .map(|i| {
                (
                    Arc::new(PhysColumn::new(side_schema.field(i).name(), i)) as Arc<dyn PhysicalExpr>,
                    side_schema.field(i).name().clone(),
                )
            })
            .collect();
        let side: Arc<dyn ExecutionPlan> = Arc::new(ProjectionExec::try_new(keep, side)?);
        let union = UnionExec::try_new(vec![align_schema(settled, &side.schema())?, side])?;
        let ordering = sort_exprs(&union.schema(), &[H_PERSON])?;
        let merged: Arc<dyn ExecutionPlan> = Arc::new(SortPreservingMergeExec::new(ordering, union));
        let schema = merged.schema();
        let exprs: Vec<(Arc<dyn PhysicalExpr>, String)> = mapping
            .iter()
            .zip(out_schema.fields())
            .map(|(i, f)| {
                (Arc::new(PhysColumn::new(schema.field(*i).name(), *i)) as Arc<dyn PhysicalExpr>, f.name().clone())
            })
            .collect();
        Ok(Arc::new(ProjectionExec::try_new(exprs, merged)?))
    }

    /// Plan for one day: `(single-partition plan, files read)`, or `None`
    /// when every file is pruned.
    fn day(&self, manifest: &DayManifest, out: &[(Expr, String)], out_schema: &SchemaRef) -> Result<Option<DayPlan>> {
        let snap = &self.cx.snap;
        let files: Vec<&FileEntry> =
            manifest.files.iter().filter(|f| f.rows > 0 && f.max_ts >= snap.from_ms && f.min_ts < snap.to_ms).collect();
        if files.is_empty() {
            return Ok(None);
        }
        let day = DayFilters { tombstones: tombstone_filters(snap, manifest.tombstone_ts) };
        let rows: u64 = files.iter().map(|f| f.rows).sum();
        if let [file] = files.as_slice() {
            let stale = file.identity_version < snap.max_link_version && file.min_person.as_str() < "a;";
            if stale && !self.ordered && !self.uses_person {
                // Nothing in the request can tell which person a row belongs
                // to, and a single file has no duplicates: read it as it is.
                let plan = self.file_stream(file, &day, out, Variant::Plain, false)?;
                return Ok(Some((plan, 1, rows)));
            }
            if stale {
                return Ok(Some((self.single_file_rewrite(file, &day, out, out_schema)?, 1, rows)));
            }
        }
        let mut streams: Vec<(&FileEntry, Variant)> = Vec::new();
        for f in &files {
            // Rows that keep their stored key can only match if the file's key
            // range (manifest) intersects what the filters allow.
            let in_range = self.bounds.may_contain(&f.min_person, &f.max_person);
            // "a:" keys sort before everything else, so a file whose smallest
            // key is already past them has nothing to rewrite.
            let has_unresolved = f.min_person.as_str() < "a;";
            if f.identity_version < snap.max_link_version && has_unresolved {
                if in_range {
                    streams.push((f, Variant::Unlinked(f.identity_version)));
                }
                // Rewritten rows get a new key: the stored range says nothing.
                streams.push((f, Variant::Linked(f.identity_version)));
            } else if in_range {
                streams.push((f, Variant::Plain));
            }
        }
        if streams.is_empty() {
            return Ok(None);
        }
        let mut read: Vec<&str> = streams.iter().map(|(f, _)| f.key.as_str()).collect();
        read.dedup();
        let n_files = read.len();
        if let [(file, Variant::Plain)] = streams.as_slice() {
            let plan = self.file_stream(file, &day, out, Variant::Plain, self.ordered)?;
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
        let inputs =
            streams.iter().map(|(f, v)| self.file_stream(f, &day, &layout, *v, true)).collect::<Result<Vec<_>>>()?;
        let union = UnionExec::try_new(inputs)?;
        let ordering = sort_exprs(&union.schema(), &[H_PERSON, H_TS, H_UUID])?;
        let merged: Arc<dyn ExecutionPlan> = Arc::new(SortPreservingMergeExec::new(ordering, union));
        let deduped: Arc<dyn ExecutionPlan> = Arc::new(DedupExec::new(merged, [0, 1, 2]));
        let schema = deduped.schema();
        let exprs: Vec<(Arc<dyn PhysicalExpr>, String)> = mapping
            .iter()
            .zip(out_schema.fields())
            .map(|(i, f)| {
                (Arc::new(PhysColumn::new(schema.field(*i).name(), *i)) as Arc<dyn PhysicalExpr>, f.name().clone())
            })
            .collect();
        let plan: Arc<dyn ExecutionPlan> = Arc::new(ProjectionExec::try_new(exprs, deduped)?);
        Ok(Some((plan, n_files, rows)))
    }
}

/// Builds the physical scan for `req`.
pub fn plan_scan(cx: &ScanContext, state: &dyn Session, req: &ScanRequest) -> Result<Arc<EventsScanExec>> {
    let table = table_schema(&cx.snap);
    let (filters_person, filters_other): (Vec<Expr>, Vec<Expr>) =
        req.filters.iter().cloned().partition(|e| columns_of(e).contains("person_key"));
    let bounds = PersonBounds::from_filters(&filters_person);
    let uses_person = !filters_person.is_empty() || req.exprs.iter().any(|(e, _)| columns_of(e).contains("person_key"));
    let planner = Planner {
        cx,
        state,
        table: Arc::clone(&table),
        filters_other,
        filters_person,
        bounds,
        uses_person,
        ordered: req.ordered,
    };

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
    !e.is_volatile()
        && !e
            .exists(|x| {
                Ok(matches!(
                    x,
                    Expr::ScalarSubquery(_)
                        | Expr::Exists(_)
                        | Expr::InSubquery(_)
                        | Expr::OuterReferenceColumn(..)
                        | Expr::Placeholder(_)
                ))
            })
            .unwrap_or(true)
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
            .map(|f| {
                if pushable(f) {
                    TableProviderFilterPushDown::Exact
                } else {
                    TableProviderFilterPushDown::Unsupported
                }
            })
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
        let exprs = indices
            .iter()
            .map(|i| (ident(self.schema.field(*i).name().as_str()), self.schema.field(*i).name().clone()))
            .collect();
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
        let batch =
            RecordBatch::try_new(Arc::clone(&self.schema), vec![Arc::new(pk.finish()), Arc::new(props.finish())])?;
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn person_bounds_from_conjuncts() {
        let pk = || ident("person_key");
        let b =
            PersonBounds::from_filters(&[pk().gt_eq(lit("u:b")), pk().lt(lit("u:m")).and(ident("event").eq(lit("x")))]);
        assert_eq!(b, PersonBounds { lo: Some("u:b".into()), hi: Some(("u:m".into(), false)), keys: None });
        assert!(b.may_contain("a:x", "u:c") && b.may_contain("u:l", "u:z"));
        assert!(!b.may_contain("a:x", "u:a") && !b.may_contain("u:m", "u:z"));
        let b = PersonBounds::from_filters(&[pk().in_list(vec![lit("u:a"), lit("u:k")], false), pk().eq(lit("u:k"))]);
        assert_eq!(b.keys, Some(vec!["u:k".to_owned()]));
        assert!(b.may_contain("u:a", "u:k") && !b.may_contain("u:a", "u:j"));
        // Shapes that prove nothing leave the bounds open.
        let open = PersonBounds::from_filters(&[
            datafusion::logical_expr::not(pk().eq(lit("u:a"))),
            pk().eq(lit("u:a")).or(pk().eq(lit("u:b"))),
            pk().in_list(vec![lit("u:a")], true),
            ident("anon_id").eq(lit("x")),
        ]);
        assert_eq!(open, PersonBounds::default());
        assert!(open.may_contain("a", "z"));
    }
}
