//! `la-shipper rebuild --from <day> --to <day>`: re-derives the event files
//! of a range of **event days** from `wal/` (`docs/design.md` §18.5).
//!
//! Protocol (safe on a live bucket; ingestion, queries and compaction keep
//! running, shipping pauses for the duration):
//!
//! 1. **Fence.** For every shardkey the newest plan is finished if it is not
//!    (plain re-execution), then a *rebuild plan* is created as the next plan
//!    number with create-if-absent. By the plan protocol nobody creates a
//!    further plan before the newest one is finished, and shippers treat an
//!    unfinished rebuild plan as "wait" — so from here on the set of shipped
//!    segments (those with a `_done` marker) is frozen.
//! 2. **Replay.** Every shipped segment closed on or after `--from` is decoded
//!    with the normal row production (current identity map, current promoted
//!    schema, all tombstones); rows whose event day lies in the range are
//!    spooled to sorted runs on local disk. (An event of day X can only be in
//!    a segment closed on day X or later, because `ts ≤ recv_ts`.)
//! 3. **Swap.** Per `(project, day)` the runs are merged into L1 files and one
//!    manifest version replaces **all** files of that day with them, keeping
//!    `plans` and raising `plans[shardkey]` to the rebuild plan's number so a
//!    stale re-execution of an older plan can never add its L0 again. A racing
//!    compaction either loses (its inputs are gone → it gives up) or wins
//!    first (its output is then replaced like any other file). The replaced
//!    files are left to the 24 h garbage collection.
//! 4. **Unfence.** The rebuild plans are marked done; shipping resumes with
//!    the segments that were still pending, as ordinary L0 files.
//!
//! Days in the range for which the WAL holds no rows are left untouched (the
//! WAL may simply have expired). `identity/` and `persons/` are only written
//! with `--with-side-tables` (for when those prefixes were lost as well).
//! An interrupted rebuild is resumed by running the same command again, or
//! given up with `--abort` (which only removes the fence).

use crate::config::{ShipCtx, UnknownProjects};
use crate::decode::{decode_entry, DecodeStats, ProjectBatch};
use crate::error::{Result, ShipError};
use crate::exec::{execute_plan, ExecState};
use crate::plan::{done_set, latest_plan_number, load_plan, plan_finished, segment_hour_ms, Plan, RebuildSpec};
use crate::rows::build_row;
use bytes::Bytes;
use la_codec::murmur3::shard_of;
use la_codec::wal::SegmentReader;
use la_compactor::job::{self, CommitPlan, Outcome, ProjectEnv, Replace, TombstoneAdvance};
use la_core::control::{Projects, Shards};
use la_core::events::{sort_and_dedupe, EventRow};
use la_core::identity::{self, IdentityMap};
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::schema::ProjectSchema;
use la_core::time::{day_of, parse_day, DAY_MS};
use la_core::tombstone::{self, TombstoneSet};
use la_core::{keys, manifest, persons};
use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::io::Read;
use std::path::PathBuf;
use std::sync::Arc;

#[derive(Debug, Clone, Default)]
pub struct RebuildOptions {
    pub from: String,
    pub to: String,
    /// Restrict the swap to one project (the fence still covers all shardkeys).
    pub project: Option<u32>,
    /// Also re-apply identity links and person properties found in the WAL.
    pub with_side_tables: bool,
    /// Only remove the fence of an interrupted rebuild.
    pub abort: bool,
    /// Allow a range that starts before the WAL retention horizon.
    pub force: bool,
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct RebuildReport {
    /// `(shardkey, rebuild plan number)`
    pub fenced: Vec<(String, u64)>,
    pub segments: usize,
    pub rows: u64,
    pub days_rebuilt: Vec<(u32, String)>,
    /// Days in range that have data but no WAL rows; left as they were.
    pub days_untouched: Vec<(u32, String)>,
    pub aborted: bool,
}

async fn all_shardkeys(ctx: &ShipCtx, shards: &Shards) -> Result<Vec<String>> {
    let mut set: BTreeSet<String> = shards.all_shardkeys().into_iter().collect();
    for dir in ["wal/_plan", "wal/_done"] {
        set.extend(ctx.store.list_dirs(dir).await?);
    }
    Ok(set.into_iter().filter(|k| crate::plan::parse_shardkey(k).is_some()).collect())
}

/// Makes a rebuild plan the newest plan of `shardkey` (or finds ours).
async fn fence(ctx: &ShipCtx, shards: &Shards, shardkey: &str, spec: &mut RebuildSpec) -> Result<u64> {
    let mut state = ExecState::default();
    for _ in 0..64 {
        let n = latest_plan_number(&ctx.store, shardkey).await?;
        if n > 0 {
            let plan = load_plan(&ctx.store, shardkey, n)
                .await?
                .ok_or_else(|| ShipError::other(format!("plan {n} of {shardkey} vanished")))?;
            if !plan_finished(&ctx.store, &plan).await? {
                match &plan.rebuild {
                    Some(r) if r.from == spec.from && r.to == spec.to => {
                        // Resume: keep the id so side-table deltas stay idempotent.
                        spec.id.clone_from(&r.id);
                        return Ok(n);
                    }
                    Some(r) => {
                        return Err(ShipError::other(format!(
                            "rebuild {} ({}..{}) is in progress on {shardkey}; resume it with the same range or run with --abort",
                            r.id, r.from, r.to
                        )));
                    }
                    None => {
                        execute_plan(ctx, shards, &plan, &mut state).await?;
                        continue;
                    }
                }
            }
        }
        let plan = Plan {
            n: n + 1,
            shardkey: shardkey.to_owned(),
            created_at: ctx.now_ms(),
            holder: ctx.holder.clone(),
            segments: Vec::new(),
            rebuild: Some(spec.clone()),
        };
        if ctx.store.put_json_if_absent(&keys::wal_plan(shardkey, plan.n), &plan).await? {
            return Ok(plan.n);
        }
    }
    Err(ShipError::other(format!("could not fence {shardkey}: plans keep appearing")))
}

async fn unfence(ctx: &ShipCtx, fenced: &[(String, u64)]) -> Result<()> {
    for (shardkey, n) in fenced {
        ctx.store.put(&keys::wal_rebuild_done(shardkey, *n), Bytes::new()).await?;
    }
    Ok(())
}

/// Removes the fence of an interrupted rebuild without rewriting anything.
async fn abort(ctx: &ShipCtx, shards: &Shards) -> Result<RebuildReport> {
    let mut report = RebuildReport { aborted: true, ..Default::default() };
    for shardkey in all_shardkeys(ctx, shards).await? {
        let n = latest_plan_number(&ctx.store, &shardkey).await?;
        if n == 0 {
            continue;
        }
        if let Some(plan) = load_plan(&ctx.store, &shardkey, n).await? {
            if plan.rebuild.is_some() && !plan_finished(&ctx.store, &plan).await? {
                report.fenced.push((shardkey, n));
            }
        }
    }
    unfence(ctx, &report.fenced).await?;
    Ok(report)
}

struct Replay<'a> {
    ctx: &'a ShipCtx,
    opts: &'a RebuildOptions,
    spec: &'a RebuildSpec,
    spool: &'a std::path::Path,
    chunk: u64,
    pending: BTreeMap<u32, ProjectBatch>,
    pending_bytes: u64,
    identity: HashMap<u32, IdentityMap>,
    runs: BTreeMap<(u32, String), Vec<PathBuf>>,
    rows: u64,
    stats: DecodeStats,
}

impl Replay<'_> {
    /// Turns the decoded batches collected so far into sorted runs.
    async fn flush(&mut self) -> Result<()> {
        self.chunk += 1;
        self.pending_bytes = 0;
        let store = &self.ctx.store;
        // Side-table deltas are idempotent per (rebuild id, chunk).
        let delta_key = format!("rb{}", self.spec.id);
        for (project, batch) in std::mem::take(&mut self.pending) {
            if let std::collections::hash_map::Entry::Vacant(e) = self.identity.entry(project) {
                e.insert(IdentityMap::load(store, project).await?);
            }
            let map = self.identity.get_mut(&project).ok_or_else(|| ShipError::other("identity map missing"))?;
            let ProjectBatch { events, links, writes } = batch;
            if self.opts.with_side_tables {
                identity::append_delta(store, project, &delta_key, self.chunk, links, map).await?;
                persons::append_delta(store, project, &delta_key, self.chunk, writes).await?;
            }
            let schema = ProjectSchema::load(store, project).await?;
            let tombs = TombstoneSet::build(&tombstone::load_all(store, project).await?, map, 0);
            let mut by_day: BTreeMap<String, Vec<EventRow>> = BTreeMap::new();
            for ev in events {
                let day = day_of(ev.ts);
                if day.as_str() < self.opts.from.as_str() || day.as_str() > self.opts.to.as_str() {
                    continue;
                }
                let row = build_row(ev, map, &schema.columns);
                if !tombs.matches(&row.person_key, &row.anon_id, row.user_id.as_deref()) {
                    by_day.entry(day).or_default().push(row);
                }
            }
            for (day, mut rows) in by_day {
                rows.sort_by(|a, b| {
                    a.sort_key_cmp(b).then(a.recv_ts.cmp(&b.recv_ts)).then_with(|| a.props.cmp(&b.props))
                });
                sort_and_dedupe(&mut rows);
                let meta = EventsFileMeta {
                    project,
                    day: day.clone(),
                    level: 1,
                    schema_version: schema.version,
                    identity_version: map.version(),
                };
                let (bytes, _) = write_events_file(&rows, &schema.columns, &meta)?;
                let dir = self.spool.join(format!("p{project}")).join(&day);
                std::fs::create_dir_all(&dir)?;
                let path = dir.join(format!("run-{:06}.parquet", self.chunk));
                std::fs::write(&path, &bytes)?;
                self.rows += rows.len() as u64;
                self.runs.entry((project, day)).or_default().push(path);
            }
        }
        Ok(())
    }
}

pub async fn rebuild(ctx: &ShipCtx, cctx: &la_compactor::Ctx, opts: &RebuildOptions) -> Result<RebuildReport> {
    let store = &ctx.store;
    let shards = Shards::load(store).await?;
    if opts.abort {
        return abort(ctx, &shards).await;
    }
    if parse_day(&opts.from).is_none() || parse_day(&opts.to).is_none() || opts.from > opts.to {
        return Err(ShipError::other("rebuild needs --from <YYYY-MM-DD> --to <YYYY-MM-DD> with from <= to"));
    }
    let now = ctx.now_ms();
    let horizon = day_of(now - cctx.cfg.wal_retention_days * DAY_MS + DAY_MS);
    if opts.from < horizon && !opts.force {
        return Err(ShipError::other(format!(
            "--from {} is before the WAL retention horizon {horizon}: the WAL for that day may be incomplete (use --force to override)",
            opts.from
        )));
    }

    // 1. Fence every shardkey.
    let mut spec = RebuildSpec { id: now.to_string(), from: opts.from.clone(), to: opts.to.clone() };
    let mut report = RebuildReport::default();
    for shardkey in all_shardkeys(ctx, &shards).await? {
        let n = fence(ctx, &shards, &shardkey, &mut spec).await?;
        report.fenced.push((shardkey, n));
    }
    let fenced_keys: BTreeSet<&str> = report.fenced.iter().map(|(k, _)| k.as_str()).collect();
    tracing::info!(id = %spec.id, from = %opts.from, to = %opts.to, shardkeys = report.fenced.len(), "rebuild: shipping fenced");

    // 2. Replay the shipped segments.
    let mut done: HashMap<String, HashSet<String>> = HashMap::new();
    for (shardkey, _) in &report.fenced {
        done.insert(shardkey.clone(), done_set(store, shardkey).await?);
    }
    let projects = Projects::load(store).await?;
    let reject_unknown = ctx.cfg.unknown_projects == UnknownProjects::Reject && !projects.projects.is_empty();
    let spool = cctx.spool()?;
    let mut replay = Replay {
        ctx,
        opts,
        spec: &spec,
        spool: spool.path(),
        chunk: 0,
        pending: BTreeMap::new(),
        pending_bytes: 0,
        identity: HashMap::new(),
        runs: BTreeMap::new(),
        rows: 0,
        stats: DecodeStats::default(),
    };
    let mut drop_keys: HashMap<u32, HashSet<String>> = HashMap::new();
    for dir in store.list_dirs("wal").await? {
        let Some(day) = dir.strip_prefix("dt=").filter(|d| parse_day(d).is_some()) else {
            continue;
        };
        if day < opts.from.as_str() {
            continue;
        }
        for obj in store.list(&keys::wal_day_prefix(day)).await? {
            let (Some(name), Some(hour)) = (keys::segment_name(&obj.key), segment_hour_ms(&obj.key)) else {
                continue;
            };
            let map = shards.map_at(hour);
            let shipped_for =
                |shard: u32| done.get(&format!("{}-{}", map.version, shard)).is_some_and(|set| set.contains(name));
            if !(0..map.shards.max(1)).any(shipped_for) {
                continue; // still pending everywhere: the shipper will handle it
            }
            let Some(compressed) = store.get(&obj.key).await? else { continue };
            let mut data = Vec::new();
            let ok = zstd::stream::read::Decoder::new(&compressed[..])
                .map(|d| d.take(ctx.cfg.max_segment_bytes + 1).read_to_end(&mut data).is_ok())
                .unwrap_or(false);
            if !ok || data.len() as u64 > ctx.cfg.max_segment_bytes {
                tracing::warn!(segment = %obj.key, "rebuild: unreadable segment skipped");
                continue;
            }
            let Ok(reader) = SegmentReader::new(&data) else {
                tracing::warn!(segment = %obj.key, "rebuild: unreadable segment skipped");
                continue;
            };
            report.segments += 1;
            for entry in reader {
                let project = entry.envelope.project_id;
                if opts.project.is_some_and(|p| p != project) || project == 0 {
                    continue;
                }
                if !shipped_for(shard_of(project, map.shards)) {
                    continue;
                }
                if projects.is_closed(project) || (reject_unknown && projects.get(project).is_none()) {
                    continue;
                }
                let dk = drop_keys.entry(project).or_insert_with(|| {
                    projects.get(project).map(|p| p.drop_keys.iter().cloned().collect()).unwrap_or_default()
                });
                let batch = replay.pending.entry(project).or_default();
                decode_entry(&entry.envelope, entry.body, dk, batch, &mut replay.stats);
            }
            replay.pending_bytes += data.len() as u64;
            if replay.pending_bytes >= ctx.cfg.batch_bytes {
                replay.flush().await?;
            }
        }
    }
    replay.flush().await?;
    report.rows = replay.rows;
    let runs = std::mem::take(&mut replay.runs);
    drop(replay);

    // 3. Swap each day.
    let set_plans: BTreeMap<String, u64> = report.fenced.iter().cloned().collect();
    let mut envs: HashMap<u32, Arc<ProjectEnv>> = HashMap::new();
    for ((project, day), paths) in runs {
        if let std::collections::hash_map::Entry::Vacant(e) = envs.entry(project) {
            e.insert(Arc::new(ProjectEnv::load(store, project).await?));
        }
        let env = envs.get(&project).ok_or_else(|| ShipError::other("project env missing"))?.clone();
        let current = manifest::load_day(store, project, &day).await?;
        if let Some(foreign) = current.plans.keys().find(|k| !fenced_keys.contains(k.as_str())) {
            return Err(ShipError::other(format!(
                "day {day} of project {project} carries plans of shardkey {foreign}, which was not fenced; the shard map changed during the rebuild"
            )));
        }
        let staged = job::stage_local_runs(cctx, &env, &day, 1, paths).await?;
        let plan = CommitPlan {
            replace: Replace::All,
            set_plans: set_plans.clone(),
            tombstone: Some(TombstoneAdvance { ts: env.tombstones.max_ts, verified: HashSet::new() }),
        };
        match job::commit(cctx, project, &day, &staged, &plan).await? {
            Outcome::Committed { manifest, added } => {
                tracing::info!(project, day = %day, files = added.len(), rows = staged.stats.rows_out, version = manifest.version, "rebuild: day replaced");
                report.days_rebuilt.push((project, day));
            }
            other => {
                return Err(ShipError::other(format!(
                    "rebuild could not commit {day} of project {project}: {other:?}"
                )));
            }
        }
    }
    // Days in range that have data but produced no rows.
    let rebuilt: HashSet<&(u32, String)> = report.days_rebuilt.iter().collect();
    let mut untouched = Vec::new();
    for project in manifest::list_projects(store).await? {
        if opts.project.is_some_and(|p| p != project) {
            continue;
        }
        for day in manifest::list_days(store, project).await? {
            let key = (project, day);
            if key.1 >= opts.from && key.1 <= opts.to && !rebuilt.contains(&key) {
                tracing::warn!(project, day = %key.1, "rebuild: no WAL rows for this day; left untouched");
                untouched.push(key);
            }
        }
    }
    report.days_untouched = untouched;

    // 4. Unfence.
    unfence(ctx, &report.fenced).await?;
    tracing::info!(id = %spec.id, days = report.days_rebuilt.len(), rows = report.rows, segments = report.segments, "rebuild complete");
    Ok(report)
}
