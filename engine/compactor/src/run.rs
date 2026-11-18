//! The compactor cycle: retention → person deletion → compaction → GC, per
//! project, plus the bucket-wide WAL lifecycle.

use crate::config::Ctx;
use crate::job::{self, Outcome, ProjectEnv};
use crate::lease::LeaseKeeper;
use crate::{gc, tombstones, triggers, wal};
use la_core::control::{ProjectConfig, Projects, Shards};
use la_core::error::Result;
use la_core::lease::{Lease, RENEW_EVERY_MS};
use la_core::time::{day_of, DAY_MS};
use la_core::{identity, keys, manifest, persons, tombstone};
use std::collections::{BTreeSet, HashMap, HashSet};
use std::sync::Arc;
use std::time::Duration;

/// Compactions run back to back on one day within a single cycle.
const MAX_ROUNDS_PER_DAY: usize = 4;

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct CycleReport {
    pub projects: usize,
    pub compactions: usize,
    pub gc_deleted: u64,
    pub days_expired: usize,
    pub tombstones_pending: usize,
    pub errors: usize,
}

#[derive(Debug, Default)]
struct ProjectReport {
    compactions: usize,
    gc_deleted: u64,
    days_expired: usize,
    tombstones_pending: usize,
    l0_files: usize,
}

/// Every project that has state the compactor looks after.
pub async fn known_projects(ctx: &Ctx) -> Result<Vec<u32>> {
    let mut ids: BTreeSet<u32> = manifest::list_projects(&ctx.store).await?.into_iter().collect();
    for top in ["identity", "persons", "tombstones", "replay"] {
        ids.extend(ctx.store.list_dirs(top).await?.iter().filter_map(|d| keys::parse_project_dir(d)));
    }
    Ok(ids.into_iter().collect())
}

async fn process_project(
    ctx: &Ctx,
    project: u32,
    pcfg: &ProjectConfig,
    now_ms: i64,
    full: bool,
) -> Result<ProjectReport> {
    let store = &ctx.store;
    let mut report = ProjectReport::default();

    if pcfg.closed {
        report.gc_deleted = gc::purge_project(ctx, project).await?;
        return Ok(report);
    }

    // Retention first: no point compacting what is about to go.
    let mut days = manifest::list_days(store, project).await?;
    let expired = gc::apply_retention(ctx, project, pcfg, &days, now_ms).await?;
    report.days_expired = expired.len();
    days.retain(|d| !expired.contains(d));

    let hot_from = day_of(now_ms - (ctx.cfg.hot_days as i64 - 1) * DAY_MS);
    let scope: Vec<&String> = days.iter().filter(|d| full || d.as_str() >= hot_from.as_str()).collect();

    let tombstone_list = tombstone::load_all(store, project).await?;
    let max_tombstone = tombstone_list.iter().map(|t| t.ts_ms).max().unwrap_or(0);
    let mut state = if tombstone_list.is_empty() {
        tombstones::TombstoneState::default()
    } else {
        tombstones::load_state(ctx, project).await?
    };

    // The identity map and schema are only loaded once something needs them.
    let mut env: Option<Arc<ProjectEnv>> = None;
    if !tombstone_list.is_empty() && state.side_ts < max_tombstone {
        let loaded = ProjectEnv::load(store, project).await?;
        let loaded = if tombstones::write_expansions(ctx, &loaded, state.side_ts).await? {
            ProjectEnv::load(store, project).await?
        } else {
            loaded
        };
        env = Some(Arc::new(loaded));
    }
    let max_tombstone = env.as_ref().map(|e| e.tombstones.max_ts).unwrap_or(max_tombstone);

    let mut all_days_clean = true;
    for day in scope {
        let mut m = manifest::load_day(store, project, day).await?;
        if m.version > 0 && m.tombstone_ts < max_tombstone {
            if env.is_none() {
                env = Some(Arc::new(ProjectEnv::load(store, project).await?));
            }
            if let Some(e) = &env {
                let res = tombstones::process_day(ctx, e, day).await?;
                all_days_clean &= res.done;
                if res.rewritten {
                    tracing::info!(project, day = %day, rows = res.rows_deleted, replay = res.replay_deleted, "tombstones applied");
                    m = manifest::load_day(store, project, day).await?;
                }
            }
        }
        for _ in 0..MAX_ROUNDS_PER_DAY {
            let Some(group) = triggers::next_job(&m, now_ms, &ctx.cfg) else {
                break;
            };
            // State newer than the snapshot `m` that names the job's inputs.
            let fresh = match env.take() {
                Some(e) => ProjectEnv::refreshed(e, store).await?,
                None => Arc::new(ProjectEnv::load(store, project).await?),
            };
            let e = env.insert(fresh);
            match job::compact(ctx, e, day, group).await? {
                Outcome::Committed { manifest, .. } => {
                    report.compactions += 1;
                    m = manifest;
                }
                Outcome::Unchanged(_) | Outcome::InputVanished | Outcome::Busy => break,
            }
        }
        report.l0_files += m.files.iter().filter(|f| f.level == 0).count();
        if full {
            report.gc_deleted += gc::gc_day(ctx, project, day, now_ms).await?;
        }
    }

    if full {
        if !tombstone_list.is_empty() && state.side_ts < max_tombstone && all_days_clean {
            if let Some(e) = &env {
                tombstones::finish_side_tables(ctx, e).await?;
                state.side_ts = max_tombstone;
                tracing::info!(project, "person deletion complete");
            }
        }
        identity::compact(store, project).await?;
        persons::compact(store, project, &HashSet::new(), false).await?;
        report.gc_deleted += gc::gc_side_tables(ctx, project, now_ms).await?;
        report.gc_deleted += gc::apply_replay_retention(ctx, project, pcfg, now_ms).await?;
        report.gc_deleted += tombstones::expire_tombstones(ctx, project, now_ms).await?;
    }
    report.tombstones_pending = tombstone_list
        .iter()
        .filter(|t| !t.id.ends_with(tombstones::EXPANSION_SUFFIX) && t.ts_ms > state.side_ts)
        .count();
    Ok(report)
}

/// One pass over the projects selected by `owns`. `full` examines every day
/// and runs GC (otherwise only the hot days are compacted); `global` adds the
/// bucket-wide WAL lifecycle.
pub async fn cycle(
    ctx: &Ctx,
    now_ms: i64,
    full: bool,
    global: bool,
    owns: &(dyn Fn(u32) -> bool + Sync),
) -> Result<CycleReport> {
    let projects_cfg = Projects::load(&ctx.store).await?;
    let mut report = CycleReport::default();
    let mut l0: HashMap<u32, usize> = HashMap::new();
    for project in known_projects(ctx).await?.into_iter().filter(|p| owns(*p)) {
        report.projects += 1;
        let pcfg = projects_cfg.get_or_default(project);
        match process_project(ctx, project, &pcfg, now_ms, full).await {
            Ok(r) => {
                report.compactions += r.compactions;
                report.gc_deleted += r.gc_deleted;
                report.days_expired += r.days_expired;
                report.tombstones_pending += r.tombstones_pending;
                l0.insert(project, r.l0_files);
            }
            Err(e) => {
                report.errors += 1;
                ctx.metrics.inc("errors_total", &[], 1);
                tracing::error!(project, error = %e, "project pass failed");
            }
        }
    }
    if global && full {
        match wal::wal_lifecycle(ctx, now_ms).await {
            Ok(n) => report.gc_deleted += n,
            Err(e) => {
                report.errors += 1;
                tracing::error!(error = %e, "wal lifecycle failed");
            }
        }
    }
    let mut top: Vec<(u32, usize)> = l0.into_iter().collect();
    top.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(&b.0)));
    for (project, n) in top.into_iter().take(ctx.cfg.top_projects) {
        ctx.metrics.set("l0_files", &[("project", &project.to_string())], n as f64);
    }
    ctx.metrics.set("tombstones_pending", &[], report.tombstones_pending as f64);
    ctx.metrics.inc("cycles_total", &[], 1);
    Ok(report)
}

#[derive(Debug, Clone, Default)]
pub struct RunOptions {
    /// Run one full cycle and return.
    pub once: bool,
    /// Shard indices of the current shard map this process may work on
    /// (`None` = all).
    pub shards: Option<Vec<u32>>,
    /// Skip leases (single cycle on a bucket nobody else compacts).
    pub no_lease: bool,
}

/// The `la-shipper compact` loop: holds `compactor-<shardkey>` leases for the
/// current shard map and runs [`cycle`] for the projects of the shards it owns.
pub async fn run(ctx: Ctx, opts: RunOptions, mut shutdown: tokio::sync::watch::Receiver<bool>) -> Result<()> {
    let mut keepers: HashMap<String, LeaseKeeper> = HashMap::new();
    let mut last_full = 0i64;
    tracing::info!(store = ctx.store.describe(), holder = %ctx.holder, "compactor started");
    loop {
        let pass = run_pass(&ctx, &opts, &mut keepers, &mut last_full).await;
        match pass {
            Ok(report) if opts.once => {
                return if report.errors == 0 {
                    Ok(())
                } else {
                    Err(la_core::Error::invalid(format!("{} project passes failed", report.errors)))
                };
            }
            Ok(_) => {}
            Err(e) if opts.once => return Err(e),
            Err(e) => {
                ctx.metrics.inc("errors_total", &[], 1);
                tracing::error!(error = %e, "compactor pass failed; retrying next cycle");
            }
        }
        tokio::select! {
            _ = tokio::time::sleep(Duration::from_millis(ctx.cfg.interval_ms)) => {}
            _ = shutdown.changed() => {}
        }
        if *shutdown.borrow() {
            tracing::info!("compactor stopping");
            return Ok(());
        }
    }
}

async fn run_pass(
    ctx: &Ctx,
    opts: &RunOptions,
    keepers: &mut HashMap<String, LeaseKeeper>,
    last_full: &mut i64,
) -> Result<CycleReport> {
    let now = la_core::time::now_ms();
    let shards = Shards::load(&ctx.store).await?;
    let map = shards.map_at(now);
    let wanted: Vec<u32> =
        (0..map.shards.max(1)).filter(|s| opts.shards.as_ref().is_none_or(|l| l.contains(s))).collect();
    let mut owned: HashSet<u32> = HashSet::new();
    for s in &wanted {
        let shardkey = format!("{}-{}", map.version, s);
        if opts.no_lease {
            owned.insert(*s);
            continue;
        }
        if !keepers.contains_key(&shardkey) {
            let lease = Lease::new(ctx.store.clone(), &format!("compactor-{shardkey}"), &ctx.holder);
            keepers.insert(shardkey.clone(), LeaseKeeper::start(lease, Duration::from_millis(RENEW_EVERY_MS)).await);
        }
        let held = keepers.get(&shardkey).is_some_and(LeaseKeeper::held);
        ctx.metrics.set("lease_held", &[("shard", &shardkey)], if held { 1.0 } else { 0.0 });
        if held {
            owned.insert(*s);
        }
    }
    // Leases of shard maps that are no longer current are let go.
    keepers.retain(|k, _| k.starts_with(&format!("{}-", map.version)));
    if owned.is_empty() {
        tracing::info!("no compactor lease held; standing by");
        return Ok(CycleReport::default());
    }
    let full = opts.once || now - *last_full >= ctx.cfg.full_scan_interval_ms;
    let global = owned.contains(&0);
    let owns = |p: u32| owned.contains(&la_codec::murmur3::shard_of(p, map.shards.max(1)));
    let report = cycle(ctx, now, full, global, &owns).await?;
    if full {
        *last_full = now;
    }
    tracing::info!(
        projects = report.projects,
        compactions = report.compactions,
        gc_deleted = report.gc_deleted,
        days_expired = report.days_expired,
        tombstones_pending = report.tombstones_pending,
        errors = report.errors,
        full,
        "compactor cycle"
    );
    Ok(report)
}
