//! The ship loop (`spec/manifest.md` §6): one worker per shardkey, each
//! holding the `shipper-<shardkey>` lease, cutting and executing plans.

use crate::config::ShipCtx;
use crate::error::{Result, ShipError};
use crate::exec::{execute_plan, ExecState, PlanOutcome};
use crate::plan::{
    active_shardkeys, done_set, hour_prefixes, latest_plan_number, list_segments, load_plan, plan_finished,
    segment_belongs, Plan,
};
use la_compactor::lease::LeaseKeeper;
use la_core::control::Shards;
use la_core::keys;
use la_core::lease::{Lease, RENEW_EVERY_MS};
use la_core::store::ObjInfo;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::time::{Duration, Instant};

#[derive(Debug)]
pub enum Tick {
    /// Nothing is waiting.
    Idle,
    /// Segments are waiting for the batch window to close.
    Waiting { pending: usize },
    Executed(Box<PlanOutcome>),
    /// The newest plan is an unfinished rebuild plan: shipping is fenced
    /// until `la-shipper rebuild` completes (or is aborted).
    Blocked,
    /// Someone else created the next plan first; state is re-read next tick.
    Lost,
}

/// Plan sequencing state of one shardkey.
pub struct ShardWorker {
    pub shardkey: String,
    synced: bool,
    n: u64,
    done: HashSet<String>,
    last_plan: Option<Instant>,
    /// Running estimate of decompressed/compressed segment size.
    ratio: f64,
    state: ExecState,
}

impl ShardWorker {
    pub fn new(shardkey: &str, initial_ratio: f64) -> Self {
        Self {
            shardkey: shardkey.to_owned(),
            synced: false,
            n: 0,
            done: HashSet::new(),
            last_plan: None,
            ratio: initial_ratio,
            state: ExecState::default(),
        }
    }

    /// Step 1 of the loop: read the newest plan and finish it if needed.
    async fn sync(&mut self, ctx: &ShipCtx, shards: &Shards) -> Result<Option<Tick>> {
        let store = &ctx.store;
        let n = latest_plan_number(store, &self.shardkey).await?;
        let mut executed = None;
        if n > 0 {
            let plan = load_plan(store, &self.shardkey, n)
                .await?
                .ok_or_else(|| ShipError::other(format!("plan {n} of {} vanished", self.shardkey)))?;
            if !plan_finished(store, &plan).await? {
                if plan.rebuild.is_some() {
                    return Ok(Some(Tick::Blocked));
                }
                tracing::info!(plan = n, shardkey = %self.shardkey, "re-executing unfinished plan");
                ctx.metrics.inc("plans_reexecuted_total", &[], 1);
                executed = Some(execute_plan(ctx, shards, &plan, &mut self.state).await?);
            }
        }
        self.n = n;
        self.done = done_set(store, &self.shardkey).await?;
        self.synced = true;
        Ok(executed.map(|o| Tick::Executed(Box::new(o))))
    }

    /// One iteration. `segments` is the current listing of the scan window;
    /// `force` cuts a plan regardless of the batch window.
    pub async fn tick(&mut self, ctx: &ShipCtx, shards: &Shards, segments: &[ObjInfo], force: bool) -> Result<Tick> {
        if !self.synced {
            if let Some(t) = self.sync(ctx, shards).await? {
                return Ok(t);
            }
        }
        let pending: Vec<&ObjInfo> = segments
            .iter()
            .filter(|o| segment_belongs(shards, &self.shardkey, &o.key))
            .filter(|o| keys::segment_name(&o.key).is_some_and(|n| !self.done.contains(n)))
            .collect();
        let shardkey = self.shardkey.clone();
        let report_backlog = |waiting: &[&ObjInfo]| {
            let labels = [("shard", shardkey.as_str())];
            ctx.metrics.set("segments_pending", &labels, waiting.len() as f64);
            // Age of the oldest segment not shipped yet (0 when caught up).
            let oldest = waiting.iter().map(|o| o.last_modified_ms).min();
            let lag = oldest.map(|t| (la_core::time::now_ms() - t).max(0) as f64 / 1000.0).unwrap_or(0.0);
            ctx.metrics.set("ship_lag_seconds", &labels, lag);
        };
        report_backlog(&pending);
        if pending.is_empty() {
            return Ok(Tick::Idle);
        }
        let estimate = |o: &ObjInfo| (o.size as f64 * self.ratio) as u64;
        let waiting: u64 = pending.iter().map(|o| estimate(o)).sum();
        let window_over =
            self.last_plan.is_none_or(|t| t.elapsed() >= Duration::from_millis(ctx.cfg.batch_interval_ms));
        if !(force || window_over || waiting >= ctx.cfg.batch_bytes) {
            return Ok(Tick::Waiting { pending: pending.len() });
        }
        // Step 2: the next plan takes pending segments in key order up to the size budget.
        let mut chosen = Vec::new();
        let mut budget = 0u64;
        for o in &pending {
            chosen.push(o.key.clone());
            budget += estimate(o);
            if budget >= ctx.cfg.batch_bytes {
                break;
            }
        }
        let plan = Plan {
            n: self.n + 1,
            shardkey: self.shardkey.clone(),
            created_at: ctx.now_ms(),
            holder: ctx.holder.clone(),
            segments: chosen,
            rebuild: None,
        };
        if !ctx.store.put_json_if_absent(&keys::wal_plan(&self.shardkey, plan.n), &plan).await? {
            self.synced = false;
            return Ok(Tick::Lost);
        }
        self.n = plan.n;
        self.last_plan = Some(Instant::now());
        // Step 3. On failure the plan stays the newest one and is re-executed
        // by whoever looks next.
        let out = match execute_plan(ctx, shards, &plan, &mut self.state).await {
            Ok(out) => out,
            Err(e) => {
                self.synced = false;
                return Err(e);
            }
        };
        self.done.extend(plan.segments.iter().filter_map(|s| keys::segment_name(s)).map(str::to_owned));
        report_backlog(&pending[plan.segments.len()..]);
        if out.compressed_bytes > 0 {
            let seen = out.decompressed_bytes as f64 / out.compressed_bytes as f64;
            self.ratio = (0.7 * self.ratio + 0.3 * seen).clamp(1.0, 100.0);
        }
        Ok(Tick::Executed(Box::new(out)))
    }
}

/// Ships everything currently waiting, for every active shardkey this
/// process may run, without taking leases. Used by `la-shipper ship --once`
/// and the tests. Returns the outcome of every plan executed.
pub async fn ship_once(ctx: &ShipCtx) -> Result<Vec<PlanOutcome>> {
    let shards = Shards::load(&ctx.store).await?;
    let now = ctx.now_ms();
    let segments = list_segments(&ctx.store, &hour_prefixes(now, ctx.cfg.scan_hours)).await?;
    let mut outcomes = Vec::new();
    for shardkey in active_shardkeys(&shards, now, ctx.cfg.scan_hours) {
        if !ctx.cfg.wants_shard(&shardkey) {
            continue;
        }
        let mut worker = ShardWorker::new(&shardkey, ctx.cfg.initial_ratio);
        loop {
            match worker.tick(ctx, &shards, &segments, true).await? {
                Tick::Executed(out) => outcomes.push(*out),
                Tick::Lost => continue,
                Tick::Blocked => {
                    tracing::warn!(shardkey = %shardkey, "shipping is fenced by an unfinished rebuild");
                    break;
                }
                Tick::Idle | Tick::Waiting { .. } => break,
            }
        }
    }
    Ok(outcomes)
}

/// Listing cache for the scan window: the newest hours are listed on every
/// call, the rest every `full_scan_interval`.
#[derive(Default)]
struct SegmentScanner {
    by_prefix: BTreeMap<String, Vec<ObjInfo>>,
    last_full: Option<Instant>,
}

impl SegmentScanner {
    const HOT_HOURS: usize = 3;

    async fn scan(&mut self, ctx: &ShipCtx) -> Result<Vec<ObjInfo>> {
        let prefixes = hour_prefixes(ctx.now_ms(), ctx.cfg.scan_hours);
        let full = self.last_full.is_none_or(|t| t.elapsed() >= Duration::from_millis(ctx.cfg.full_scan_interval_ms));
        let hot_from = prefixes.len().saturating_sub(Self::HOT_HOURS);
        for (i, p) in prefixes.iter().enumerate() {
            if full || i >= hot_from || !self.by_prefix.contains_key(p) {
                let listed = list_segments(&ctx.store, std::slice::from_ref(p)).await?;
                self.by_prefix.insert(p.clone(), listed);
            }
        }
        if full {
            self.last_full = Some(Instant::now());
        }
        self.by_prefix.retain(|p, _| prefixes.contains(p));
        Ok(self.by_prefix.values().flatten().cloned().collect())
    }
}

/// The default mode: runs until `shutdown` flips to `true`.
pub async fn run(ctx: ShipCtx, mut shutdown: tokio::sync::watch::Receiver<bool>) -> Result<()> {
    let mut workers: HashMap<String, (LeaseKeeper, ShardWorker, bool)> = HashMap::new();
    let mut scanner = SegmentScanner::default();
    tracing::info!(store = ctx.store.describe(), holder = %ctx.holder, "shipper started");
    loop {
        let pass = async {
            let shards = Shards::load(&ctx.store).await?;
            let active: Vec<String> = active_shardkeys(&shards, ctx.now_ms(), ctx.cfg.scan_hours)
                .into_iter()
                .filter(|k| ctx.cfg.wants_shard(k))
                .collect();
            workers.retain(|k, _| active.contains(k));
            for shardkey in &active {
                if !workers.contains_key(shardkey) {
                    let lease = Lease::new(ctx.store.clone(), &format!("shipper-{shardkey}"), &ctx.holder);
                    let keeper = LeaseKeeper::start(lease, Duration::from_millis(RENEW_EVERY_MS)).await;
                    let worker = ShardWorker::new(shardkey, ctx.cfg.initial_ratio);
                    workers.insert(shardkey.clone(), (keeper, worker, false));
                }
            }
            let segments = scanner.scan(&ctx).await?;
            for (shardkey, (keeper, worker, was_held)) in workers.iter_mut() {
                let held = keeper.held();
                ctx.metrics.set("lease_held", &[("shard", shardkey)], if held { 1.0 } else { 0.0 });
                if held != *was_held {
                    tracing::info!(shardkey = %shardkey, held, "shipper lease changed hands");
                    // Whatever happened while we were not the owner is unknown.
                    worker.synced = false;
                    *was_held = held;
                }
                if !held {
                    continue;
                }
                match worker.tick(&ctx, &shards, &segments, false).await {
                    Ok(Tick::Blocked) => {
                        ctx.metrics.set("rebuild_fenced", &[("shard", shardkey)], 1.0);
                        tracing::warn!(shardkey = %shardkey, "shipping is fenced by an unfinished rebuild");
                    }
                    Ok(_) => ctx.metrics.set("rebuild_fenced", &[("shard", shardkey)], 0.0),
                    Err(e) => {
                        ctx.metrics.inc("errors_total", &[], 1);
                        tracing::error!(shardkey = %shardkey, error = %e, "plan execution failed; it will be retried");
                    }
                }
            }
            Ok::<(), ShipError>(())
        };
        if let Err(e) = pass.await {
            ctx.metrics.inc("errors_total", &[], 1);
            tracing::error!(error = %e, "ship pass failed");
        }
        tokio::select! {
            _ = tokio::time::sleep(Duration::from_millis(ctx.cfg.poll_interval_ms)) => {}
            _ = shutdown.changed() => {}
        }
        if *shutdown.borrow() {
            tracing::info!("shipper stopping");
            return Ok(());
        }
    }
}
