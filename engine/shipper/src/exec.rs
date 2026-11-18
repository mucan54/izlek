//! Execution of one shipper plan (`spec/manifest.md` §6 step 3):
//! decode → identity delta → persons delta → L0 files → day manifests
//! (`plans[shardkey] = n`) → schema statistics → `_done` markers.
//!
//! The plan's segments are held decompressed (that is the plan's size budget)
//! and decoded in two passes: first only identify/set_person records, whose
//! deltas must be committed before any row is keyed; then events, which go
//! straight into their `(project, day)` row group — decoded events are never
//! buffered for the whole plan.
//!
//! Every step is idempotent per `(shardkey, n)`, so a plan may be executed
//! any number of times, by anyone, and stopped anywhere:
//!
//! * identity and persons deltas carry `applied[shardkey]`;
//! * an L0 object is created with create-if-absent under its deterministic
//!   name — if the name is taken (an earlier execution got that far) the
//!   existing object is described and referenced instead of being rewritten,
//!   so a manifest entry always matches the bytes it points at;
//! * a day manifest that already records `plans[shardkey] ≥ n` is left alone
//!   (the L0 may long have been compacted away);
//! * schema statistics carry `applied[shardkey]`;
//! * `_done` markers are plain idempotent puts.

use crate::config::{ShipCtx, UnknownProjects};
use crate::decode::{decode_entry, DecodeStats, ProjectBatch, UNKNOWN_PROJECT};
use crate::error::{Result, ShipError};
use crate::plan::{mark_done, parse_shardkey, segment_hour_ms, Plan};
use crate::rows::{build_row, PlanStats};
use la_codec::murmur3::shard_of;
use la_codec::wal::{SegmentReader, Tail, FORMAT_NATIVE, FORMAT_REPLAY_INDEX};
use la_codec::wire::{KIND_IDENTIFY, KIND_SET_PERSON};
use la_compactor::describe::describe_events_file;
use la_compactor::merge::REPLAY_CHUNK_EVENT;
use la_core::control::{Projects, Shards};
use la_core::events::{sort_and_dedupe, EventRow};
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::manifest::{self, FileEntry};
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::persons::PropWrite;
use la_core::schema::ProjectSchema;
use la_core::time::day_of;
use la_core::tombstone::{Tombstone, TombstoneSet};
use la_core::{keys, persons, Store};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::io::Read;
use std::sync::Arc;

/// Boundaries at which a failure can be injected (design §17.5).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum Step {
    IdentityDelta { project: u32 },
    PersonsDelta { project: u32 },
    L0Written { project: u32, day: String },
    ManifestCommitted { project: u32, day: String },
    SchemaSaved { project: u32 },
    DoneMarker { index: usize },
}

/// Returns `true` to make the execution fail right after the given step.
pub type FailHook = Arc<dyn Fn(&Step) -> bool + Send + Sync>;

#[derive(Debug, Default, Clone, PartialEq)]
pub struct PlanOutcome {
    pub plan: u64,
    pub segments: usize,
    /// `(segment key, reason)`
    pub quarantined: Vec<(String, String)>,
    pub corrupt_tails: usize,
    pub compressed_bytes: u64,
    pub decompressed_bytes: u64,
    pub entries: u64,
    /// Rows in the L0 files this execution added to manifests.
    pub records_out: u64,
    pub parquet_bytes: u64,
    pub files: usize,
    pub links: usize,
    pub identity_conflicts: usize,
    pub person_writes: usize,
    pub tombstoned: u64,
    pub promoted: Vec<(u32, String)>,
    pub decode: DecodeStats,
}

/// Tombstones of all projects, re-listed at most every
/// `tombstone_refresh_ms`; object contents are immutable and cached.
#[derive(Debug, Default)]
pub struct TombstoneCache {
    by_key: HashMap<String, Option<Tombstone>>,
    by_project: HashMap<u32, Vec<String>>,
    refreshed_at: Option<i64>,
}

impl TombstoneCache {
    /// Returns the projects whose tombstone list changed.
    pub async fn refresh(&mut self, store: &Store, now_ms: i64, max_age_ms: i64) -> Result<HashSet<u32>> {
        if self.refreshed_at.is_some_and(|t| now_ms - t < max_age_ms && now_ms >= t) {
            return Ok(HashSet::new());
        }
        let mut by_project: HashMap<u32, Vec<String>> = HashMap::new();
        for o in store.list("tombstones").await? {
            let mut parts = o.key.split('/');
            let (Some("tombstones"), Some(dir), Some(name)) = (parts.next(), parts.next(), parts.next()) else {
                continue;
            };
            let Some(project) = keys::parse_project_dir(dir) else { continue };
            if !name.ends_with(".json") {
                continue;
            }
            if !self.by_key.contains_key(&o.key) {
                let t = match store.get_json::<Tombstone>(&o.key).await {
                    Ok(t) => t,
                    Err(e) => {
                        tracing::warn!(key = %o.key, error = %e, "skipping unreadable tombstone");
                        None
                    }
                };
                self.by_key.insert(o.key.clone(), t);
            }
            by_project.entry(project).or_default().push(o.key);
        }
        let mut changed = HashSet::new();
        for (p, keys) in &by_project {
            if self.by_project.get(p) != Some(keys) {
                changed.insert(*p);
            }
        }
        changed.extend(self.by_project.keys().filter(|p| !by_project.contains_key(p)));
        self.by_key.retain(|k, _| by_project.values().any(|v| v.contains(k)));
        self.by_project = by_project;
        self.refreshed_at = Some(now_ms);
        Ok(changed)
    }

    pub fn list(&self, project: u32) -> Vec<Tombstone> {
        self.by_project
            .get(&project)
            .map(|keys| keys.iter().filter_map(|k| self.by_key.get(k).cloned().flatten()).collect())
            .unwrap_or_default()
    }
}

/// State a shipper keeps between plans of one shardkey. Purely a cache:
/// dropping it never changes results.
#[derive(Debug, Default)]
pub struct ExecState {
    pub identity: HashMap<u32, IdentityMap>,
    pub tombstones: TombstoneCache,
}

/// Identity maps kept between plans before the cache is dropped wholesale.
const IDENTITY_CACHE_PROJECTS: usize = 256;

fn fail_after(ctx: &ShipCtx, step: Step) -> Result<()> {
    match &ctx.fail {
        Some(hook) if hook(&step) => Err(ShipError::Injected(step)),
        _ => Ok(()),
    }
}

fn decompress(data: &[u8], max: u64) -> std::result::Result<Vec<u8>, &'static str> {
    let dec = zstd::stream::read::Decoder::new(data).map_err(|_| "zstd")?;
    let mut out = Vec::with_capacity(data.len().saturating_mul(4).min(64 << 20));
    dec.take(max + 1).read_to_end(&mut out).map_err(|_| "zstd")?;
    if out.len() as u64 > max {
        return Err("too_large");
    }
    Ok(out)
}

async fn quarantine(ctx: &ShipCtx, plan: &Plan, segment: &str, reason: &str, out: &mut PlanOutcome) -> Result<()> {
    if let Some(name) = keys::segment_name(segment) {
        let record = serde_json::json!({
            "segment": segment, "reason": reason, "plan": plan.n, "shardkey": plan.shardkey,
            "holder": ctx.holder, "at": ctx.now_ms(),
        });
        ctx.store.put_json(&keys::wal_quarantine(&plan.shardkey, name), &record).await?;
    }
    ctx.metrics.inc("segments_quarantined_total", &[("reason", reason)], 1);
    tracing::error!(segment, reason, plan = plan.n, shardkey = %plan.shardkey, "segment quarantined");
    out.quarantined.push((segment.to_owned(), reason.to_owned()));
    Ok(())
}

/// A fetched, decompressed segment of the plan.
struct Loaded {
    data: Vec<u8>,
    /// Shard count of the map in force when the segment was closed.
    shards: u32,
}

/// Fetches and decompresses the plan's segments. Together they stay within
/// the plan's size budget, which is what bounds a plan's memory.
async fn load_segments(ctx: &ShipCtx, shards: &Shards, plan: &Plan, out: &mut PlanOutcome) -> Result<Vec<Loaded>> {
    let (version, _) = parse_shardkey(&plan.shardkey)
        .ok_or_else(|| ShipError::other(format!("plan {} has a malformed shardkey {:?}", plan.n, plan.shardkey)))?;
    let mut loaded = Vec::with_capacity(plan.segments.len());
    for segment in &plan.segments {
        let Some(hour) = segment_hour_ms(segment) else {
            quarantine(ctx, plan, segment, "bad_key", out).await?;
            continue;
        };
        let map = shards.map_at(hour);
        if map.version != version {
            // Not ours under the current shard maps; the owning shardkey ships it.
            tracing::warn!(segment, shardkey = %plan.shardkey, "segment belongs to another shard map; skipped");
            continue;
        }
        let Some(compressed) = ctx.store.get(segment).await? else {
            quarantine(ctx, plan, segment, "missing", out).await?;
            continue;
        };
        out.compressed_bytes += compressed.len() as u64;
        let data = match decompress(&compressed, ctx.cfg.max_segment_bytes) {
            Ok(d) => d,
            Err(reason) => {
                quarantine(ctx, plan, segment, reason, out).await?;
                continue;
            }
        };
        drop(compressed);
        let mut reader = match SegmentReader::new(&data) {
            Ok(r) => r,
            Err(_) => {
                quarantine(ctx, plan, segment, "bad_magic", out).await?;
                continue;
            }
        };
        // Walk once to find a torn tail; later passes iterate the valid prefix.
        reader.by_ref().for_each(drop);
        if let Some(Tail::CorruptAt(offset)) = reader.tail() {
            out.corrupt_tails += 1;
            ctx.metrics.inc("wal_corrupt_tail_total", &[], 1);
            tracing::warn!(segment, offset, "corrupt segment tail ignored");
        }
        out.decompressed_bytes += data.len() as u64;
        loaded.push(Loaded { data, shards: map.shards });
        // Keep the executor responsive between large segments.
        tokio::task::yield_now().await;
    }
    Ok(loaded)
}

/// Which projects' entries are accepted, and their `drop_keys`.
struct ProjectGate {
    projects: Projects,
    reject_unknown: bool,
    drop_keys: HashMap<u32, HashSet<String>>,
}

impl ProjectGate {
    async fn load(ctx: &ShipCtx) -> Result<Self> {
        let projects = Projects::load(&ctx.store).await?;
        let reject_unknown = ctx.cfg.unknown_projects == UnknownProjects::Reject && !projects.projects.is_empty();
        Ok(Self { projects, reject_unknown, drop_keys: HashMap::new() })
    }

    fn accepts(&self, project: u32) -> bool {
        project != 0
            && !self.projects.is_closed(project)
            && !(self.reject_unknown && self.projects.get(project).is_none())
    }

    fn drop_keys(&mut self, project: u32) -> &HashSet<String> {
        let projects = &self.projects;
        self.drop_keys
            .entry(project)
            .or_insert_with(|| projects.get(project).map(|p| p.drop_keys.iter().cloned().collect()).unwrap_or_default())
    }
}

/// Whether a native batch contains `identify` or `set_person` records, found
/// by walking the frame only (first byte of each record body).
fn native_has_side_records(body: &[u8]) -> bool {
    let Some(rest) = body.strip_prefix(&la_codec::wire::MAGIC) else {
        return false;
    };
    let mut r = la_codec::Reader::new(rest);
    let skip_header = r.len().and_then(|n| r.take(n).map(drop));
    if skip_header.is_err() {
        return false;
    }
    while !r.is_empty() {
        match r.len().and_then(|n| r.take(n)) {
            Ok(rec) => {
                if matches!(rec.first(), Some(&KIND_IDENTIFY | &KIND_SET_PERSON)) {
                    return true;
                }
            }
            Err(_) => return false,
        }
    }
    false
}

/// Identity links and person writes of one project, in WAL order.
#[derive(Default)]
struct SideRecords {
    links: Vec<Candidate>,
    writes: Vec<PropWrite>,
}

/// Pass 1: only what feeds the identity and persons deltas. These must be
/// committed before any row is keyed, and collecting them first means the
/// events never have to be held in decoded form for the whole plan.
fn collect_side_records(segments: &[Loaded], shard: u32, gate: &mut ProjectGate) -> BTreeMap<u32, SideRecords> {
    let mut out: BTreeMap<u32, SideRecords> = BTreeMap::new();
    let mut scratch = ProjectBatch::default();
    let mut ignored = DecodeStats::default(); // rejects are counted in pass 2
    for seg in segments {
        let Ok(reader) = SegmentReader::new(&seg.data) else { continue };
        for entry in reader {
            let project = entry.envelope.project_id;
            if shard_of(project, seg.shards) != shard || !gate.accepts(project) {
                continue;
            }
            let side = out.entry(project).or_default();
            if entry.envelope.format == FORMAT_REPLAY_INDEX
                || (entry.envelope.format == FORMAT_NATIVE && !native_has_side_records(entry.body))
            {
                continue;
            }
            decode_entry(&entry.envelope, entry.body, gate.drop_keys(project), &mut scratch, &mut ignored);
            scratch.events.clear();
            side.links.append(&mut scratch.links);
            side.writes.append(&mut scratch.writes);
        }
    }
    out
}

/// What pass 2 accumulates for one project.
struct ProjectRows {
    schema: ProjectSchema,
    tombstones: TombstoneSet,
    identity_version: u64,
    stats: PlanStats,
    decode: DecodeStats,
    by_day: BTreeMap<String, Vec<EventRow>>,
    replay_keys: Vec<String>,
    tombstoned: u64,
}

/// Pass 2: events → rows, straight into their `(project, day)` group.
fn build_rows(
    segments: &[Loaded],
    shard: u32,
    gate: &mut ProjectGate,
    identity: &HashMap<u32, IdentityMap>,
    rows: &mut BTreeMap<u32, ProjectRows>,
    rejected_unknown: &mut BTreeMap<u32, u64>,
    out: &mut PlanOutcome,
) {
    let mut scratch = ProjectBatch::default();
    for seg in segments {
        let Ok(reader) = SegmentReader::new(&seg.data) else { continue };
        for entry in reader {
            let project = entry.envelope.project_id;
            if shard_of(project, seg.shards) != shard {
                continue;
            }
            out.entries += 1;
            let (Some(p), Some(map)) = (rows.get_mut(&project), identity.get(&project)) else {
                // Not accepted (unknown project): count what the frame claims.
                let n = if entry.envelope.format == FORMAT_NATIVE {
                    la_codec::wire::check_frame(entry.body).map(|f| f.records as u64).unwrap_or(1).max(1)
                } else {
                    1
                };
                *rejected_unknown.entry(project).or_default() += n;
                continue;
            };
            decode_entry(&entry.envelope, entry.body, gate.drop_keys(project), &mut scratch, &mut p.decode);
            scratch.links.clear();
            scratch.writes.clear();
            for ev in scratch.events.drain(..) {
                p.stats.observe(&ev);
                let row = build_row(ev, map, &p.schema.columns);
                if p.tombstones.matches(&row.person_key, &row.anon_id, row.user_id.as_deref()) {
                    p.tombstoned += 1;
                    if row.event == REPLAY_CHUNK_EVENT {
                        if let Some(serde_json::Value::String(k)) = la_compactor::json::object_member(&row.props, "key")
                        {
                            p.replay_keys.push(k);
                        }
                    }
                    continue;
                }
                p.by_day.entry(day_of(row.ts)).or_default().push(row);
            }
        }
    }
}

/// Sorts into file order with a deterministic choice among equal keys, then
/// drops repeated uuids per person.
fn sort_rows(rows: &mut Vec<EventRow>) {
    rows.sort_by(|a, b| a.sort_key_cmp(b).then(a.recv_ts.cmp(&b.recv_ts)).then_with(|| a.props.cmp(&b.props)));
    sort_and_dedupe(rows);
}

async fn write_l0(
    ctx: &ShipCtx,
    plan: &Plan,
    project: u32,
    day: &str,
    rows: &[EventRow],
    schema: &ProjectSchema,
    identity_version: u64,
) -> Result<FileEntry> {
    let key = keys::l0_file(project, day, &plan.shardkey, plan.n);
    let meta =
        EventsFileMeta { project, day: day.to_owned(), level: 0, schema_version: schema.version, identity_version };
    let (bytes, mut entry) = write_events_file(rows, &schema.columns, &meta)?;
    entry.key.clone_from(&key);
    if ctx.store.put_if_absent(&key, bytes.clone()).await? {
        return Ok(entry);
    }
    // The name is taken: an earlier (or concurrent) execution of this plan
    // got this far. Reference what is there instead of replacing it, so the
    // manifest entry always describes the bytes it points at.
    if let Some(existing) = ctx.store.get(&key).await? {
        match describe_events_file(&key, existing) {
            Ok(found) => return Ok(found),
            Err(e) => tracing::warn!(key = %key, error = %e, "existing L0 object is unreadable; rewriting it"),
        }
    }
    ctx.store.put(&key, bytes).await?;
    Ok(entry)
}

/// L0 files → day manifests → schema statistics for one project.
async fn publish_project(
    ctx: &ShipCtx,
    plan: &Plan,
    project: u32,
    rows: ProjectRows,
    out: &mut PlanOutcome,
) -> Result<()> {
    let store = &ctx.store;
    let shardkey = plan.shardkey.as_str();
    let ProjectRows { schema, tombstones, identity_version, stats, by_day, replay_keys, tombstoned, .. } = rows;
    out.tombstoned += tombstoned;
    if !replay_keys.is_empty() {
        la_compactor::tombstones::delete_replay_keys(store, project, &replay_keys).await?;
    }

    // L0 files, one per day, named after the plan.
    let mut entries: Vec<(String, FileEntry)> = Vec::new();
    for (day, mut day_rows) in by_day {
        if manifest::load_day(store, project, &day).await?.plan_applied(shardkey, plan.n) {
            continue;
        }
        sort_rows(&mut day_rows);
        let entry = write_l0(ctx, plan, project, &day, &day_rows, &schema, identity_version).await?;
        drop(day_rows);
        fail_after(ctx, Step::L0Written { project, day: day.clone() })?;
        entries.push((day, entry));
    }

    // Day manifests. Rows written by this plan had every tombstone up to
    // `tombstones.max_ts` applied; the manifest must not claim more.
    for (day, entry) in entries {
        let applied_ts = tombstones.max_ts;
        let committed = manifest::commit_day(store, project, &day, |m| {
            if m.plan_applied(shardkey, plan.n) {
                return Ok(false);
            }
            m.tombstone_ts = if m.version == 0 { applied_ts } else { m.tombstone_ts.min(applied_ts) };
            m.files.push(entry.clone());
            m.plans.insert(shardkey.to_owned(), plan.n);
            Ok(true)
        })
        .await?;
        if committed.written {
            out.files += 1;
            out.records_out += entry.rows;
            out.parquet_bytes += entry.bytes;
            ctx.metrics.inc("records_out_total", &[], entry.rows);
            ctx.metrics.inc("parquet_bytes_total", &[], entry.bytes);
        }
        fail_after(ctx, Step::ManifestCommitted { project, day })?;
    }

    // Schema statistics and promotion (single writer: this shard's shipper).
    let mut schema = ProjectSchema::load(store, project).await?;
    if schema.applied.get(shardkey).is_none_or(|applied| *applied < plan.n) {
        stats.add_to(&mut schema.stats);
        schema.trim_stats(&day_of(ctx.now_ms()));
        let promoted = schema.promote();
        schema.applied.insert(shardkey.to_owned(), plan.n);
        schema.save(store).await?;
        for key in promoted {
            ctx.metrics.inc("columns_promoted_total", &[], 1);
            tracing::info!(project, key = %key, schema_version = schema.version, "property promoted to a column");
            out.promoted.push((project, key));
        }
    }
    fail_after(ctx, Step::SchemaSaved { project })?;
    Ok(())
}

fn report_decode(ctx: &ShipCtx, st: &DecodeStats, out: &mut PlanOutcome) {
    out.decode.merge(st);
    for (reason, n) in &st.rejected {
        ctx.metrics.inc("records_rejected_total", &[("reason", reason)], *n);
    }
    ctx.metrics.inc("props_dropped_total", &[], st.props_dropped);
    ctx.metrics.inc("entries_skipped_total", &[("reason", "unknown_format")], st.unknown_format);
}

/// Runs (or re-runs) `plan` to completion.
pub async fn execute_plan(ctx: &ShipCtx, shards: &Shards, plan: &Plan, state: &mut ExecState) -> Result<PlanOutcome> {
    let started = std::time::Instant::now();
    let store = &ctx.store;
    let shardkey = plan.shardkey.as_str();
    let mut out = PlanOutcome { plan: plan.n, segments: plan.segments.len(), ..Default::default() };
    let segments = load_segments(ctx, shards, plan, &mut out).await?;
    let shard = parse_shardkey(shardkey).map(|(_, s)| s).unwrap_or(0);
    let mut gate = ProjectGate::load(ctx).await?;

    // People deleted since the last plan: forget cached links so removed
    // identity rows stop resolving.
    let changed = state.tombstones.refresh(store, ctx.now_ms(), ctx.cfg.tombstone_refresh_ms).await?;
    for p in changed {
        state.identity.remove(&p);
    }

    // 1–2. Identity delta, then persons delta, per project — before any data
    //      manifest (`spec/manifest.md` §3).
    let side = collect_side_records(&segments, shard, &mut gate);
    let mut rows: BTreeMap<u32, ProjectRows> = BTreeMap::new();
    for (project, SideRecords { links, writes }) in side {
        if let std::collections::hash_map::Entry::Vacant(e) = state.identity.entry(project) {
            e.insert(IdentityMap::load(store, project).await?);
        }
        let map = state.identity.get_mut(&project).ok_or_else(|| ShipError::other("identity map missing"))?;
        let appended = identity::append_delta(store, project, shardkey, plan.n, links, map).await?;
        out.links += appended.added;
        out.identity_conflicts += appended.conflicts;
        ctx.metrics.inc("identity_links_total", &[], appended.added as u64);
        ctx.metrics.inc("identity_conflicts_total", &[], appended.conflicts as u64);
        fail_after(ctx, Step::IdentityDelta { project })?;

        out.person_writes += writes.len();
        persons::append_delta(store, project, shardkey, plan.n, writes).await?;
        fail_after(ctx, Step::PersonsDelta { project })?;

        let map = &*map;
        rows.insert(
            project,
            ProjectRows {
                schema: ProjectSchema::load(store, project).await?,
                tombstones: TombstoneSet::build(&state.tombstones.list(project), map, 0),
                identity_version: map.version(),
                stats: PlanStats::default(),
                decode: DecodeStats::default(),
                by_day: BTreeMap::new(),
                replay_keys: Vec::new(),
                tombstoned: 0,
            },
        );
    }

    // 3. Rows: stage-1 person keys, promoted columns, tombstones.
    let mut rejected_unknown: BTreeMap<u32, u64> = BTreeMap::new();
    build_rows(&segments, shard, &mut gate, &state.identity, &mut rows, &mut rejected_unknown, &mut out);
    drop(segments);
    let unknown: u64 = rejected_unknown.values().sum();
    if unknown > 0 {
        let mut st = DecodeStats::default();
        st.reject(UNKNOWN_PROJECT, unknown);
        report_decode(ctx, &st, &mut out);
    }

    // 4–6. L0 files → day manifests → schema statistics.
    for (project, p) in rows {
        report_decode(ctx, &p.decode, &mut out);
        if p.by_day.is_empty() && p.stats.days.is_empty() && p.replay_keys.is_empty() {
            continue; // nothing but rejects, links or person writes
        }
        publish_project(ctx, plan, project, p, &mut out).await?;
    }
    // The identity cache only saves reloads; keep it from growing without bound.
    if state.identity.len() > IDENTITY_CACHE_PROJECTS {
        state.identity.clear();
    }

    // 7. `_done` markers, last.
    for (index, segment) in plan.segments.iter().enumerate() {
        mark_done(store, shardkey, segment).await?;
        fail_after(ctx, Step::DoneMarker { index })?;
    }
    ctx.metrics.inc("plans_executed_total", &[], 1);
    ctx.metrics.inc("segments_processed_total", &[], plan.segments.len() as u64);
    ctx.metrics.observe("plan_seconds", &[], started.elapsed().as_secs_f64());
    tracing::info!(
        plan = plan.n,
        shardkey = %plan.shardkey,
        segments = out.segments,
        entries = out.entries,
        records_out = out.records_out,
        rejected = out.decode.rejected_total(),
        files = out.files,
        parquet_bytes = out.parquet_bytes,
        links = out.links,
        quarantined = out.quarantined.len(),
        seconds = started.elapsed().as_secs_f64(),
        "plan executed"
    );
    Ok(out)
}
