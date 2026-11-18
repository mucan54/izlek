//! Garbage collection and retention (`docs/design.md` §10.3–10.4,
//! `spec/manifest.md` §10).
//!
//! A data file is deleted only when all of these hold:
//! * the current manifest does not list it;
//! * the manifest that was current one grace period ago does not list it
//!   (so it has been unreferenced for at least the grace period and no
//!   running query can still hold a manifest that needs it);
//! * the object itself is older than grace + slack (an output uploaded but
//!   not yet committed is never mistaken for garbage).

use crate::config::Ctx;
use la_core::control::ProjectConfig;
use la_core::error::Result;
use la_core::store::ObjInfo;
use la_core::time::{day_of, DAY_MS};
use la_core::{keys, Store};
use std::collections::HashSet;

/// `files[].key` of any manifest flavour (day, identity, persons).
async fn listed_files(store: &Store, key: &str) -> Result<Option<HashSet<String>>> {
    let Some(v) = store.get_json::<serde_json::Value>(key).await? else {
        return Ok(None);
    };
    Ok(Some(
        v.get("files")
            .and_then(|f| f.as_array())
            .map(|a| a.iter().filter_map(|f| f.get("key")?.as_str().map(str::to_owned)).collect())
            .unwrap_or_default(),
    ))
}

/// Collects one "directory with a `_m/` manifest chain": `prefix` holds the
/// data objects, `prefix/_m` the versions. Returns the number of deleted objects.
pub async fn gc_dir(ctx: &Ctx, prefix: &str, now_ms: i64) -> Result<u64> {
    let store = &ctx.store;
    let grace = ctx.cfg.gc_grace_ms;
    let manifest_dir = format!("{prefix}/_m/");
    let objs = store.list(prefix).await?;
    let mut versions: Vec<(u64, &ObjInfo)> = objs
        .iter()
        .filter(|o| o.key.starts_with(&manifest_dir))
        .filter_map(|o| keys::manifest_version(&o.key).map(|v| (v, o)))
        .collect();
    versions.sort_by(|a, b| b.0.cmp(&a.0));
    let Some((latest, latest_obj)) = versions.first().copied() else {
        // No manifest at all: nothing can be proven unreferenced-by-design
        // except by age; leave such strays to retention.
        return Ok(0);
    };
    // The version that was current `grace` ago.
    let base = versions.iter().find(|(_, o)| o.last_modified_ms <= now_ms - grace).copied();
    let Some(mut protected) = listed_files(store, &latest_obj.key).await? else {
        return Ok(0);
    };
    if let Some((bv, bo)) = base {
        if bv != latest {
            match listed_files(store, &bo.key).await? {
                Some(files) => protected.extend(files),
                None => return Ok(0), // raced with another collector; try next time
            }
        }
    }
    let mut deleted = 0u64;
    let data_cutoff = now_ms - grace - ctx.cfg.gc_slack_ms;
    for o in &objs {
        if o.key.starts_with(&manifest_dir) || !o.key.ends_with(".parquet") {
            continue;
        }
        if protected.contains(&o.key) || o.last_modified_ms > data_cutoff {
            continue;
        }
        store.delete(&o.key).await?;
        deleted += 1;
    }
    // Manifests: keep the newest N, everything younger than the grace period
    // and the base version itself.
    let base_version = base.map(|(v, _)| v).unwrap_or(0);
    for (rank, (v, o)) in versions.iter().enumerate() {
        if rank >= ctx.cfg.keep_manifests && o.last_modified_ms <= now_ms - grace && *v < base_version {
            store.delete(&o.key).await?;
            deleted += 1;
        }
    }
    if deleted > 0 {
        ctx.metrics.inc("gc_deleted_total", &[], deleted);
    }
    Ok(deleted)
}

pub async fn gc_day(ctx: &Ctx, project: u32, day: &str, now_ms: i64) -> Result<u64> {
    gc_dir(ctx, &keys::data_day_prefix(project, day), now_ms).await
}

/// Identity and persons side tables of a project.
pub async fn gc_side_tables(ctx: &Ctx, project: u32, now_ms: i64) -> Result<u64> {
    Ok(gc_dir(ctx, &format!("identity/p={project}"), now_ms).await?
        + gc_dir(ctx, &format!("persons/p={project}"), now_ms).await?)
}

/// First day that is still inside a retention of `days` days at `now_ms`.
/// `days == 0` means "keep forever".
pub fn retention_cutoff(now_ms: i64, days: u32) -> Option<String> {
    (days > 0).then(|| day_of(now_ms - days as i64 * DAY_MS))
}

async fn delete_prefix(ctx: &Ctx, prefix: &str, manifests_first: bool) -> Result<u64> {
    let mut objs = ctx.store.list(prefix).await?;
    if manifests_first {
        // Readers stop seeing the day before its files start to disappear.
        objs.sort_by_key(|o| !o.key.contains("/_m/"));
    }
    let n = objs.len() as u64;
    for o in &objs {
        ctx.store.delete(&o.key).await?;
    }
    Ok(n)
}

/// Deletes every day directory of `project` older than its retention.
/// Returns the days removed.
pub async fn apply_retention(
    ctx: &Ctx,
    project: u32,
    cfg: &ProjectConfig,
    days: &[String],
    now_ms: i64,
) -> Result<Vec<String>> {
    let Some(cutoff) = retention_cutoff(now_ms, cfg.retention_days) else {
        return Ok(Vec::new());
    };
    let mut removed = Vec::new();
    for day in days.iter().filter(|d| d.as_str() < cutoff.as_str()) {
        let n = delete_prefix(ctx, &keys::data_day_prefix(project, day), true).await?;
        ctx.metrics.inc("gc_deleted_total", &[], n);
        ctx.metrics.inc("days_expired_total", &[], 1);
        tracing::info!(project, day = %day, objects = n, "day past retention removed");
        removed.push(day.clone());
    }
    Ok(removed)
}

/// Deletes `replay/p=<pid>/d=<day>/` directories past the replay retention.
pub async fn apply_replay_retention(ctx: &Ctx, project: u32, cfg: &ProjectConfig, now_ms: i64) -> Result<u64> {
    let Some(cutoff) = retention_cutoff(now_ms, cfg.replay_retention_days) else {
        return Ok(0);
    };
    let mut deleted = 0;
    for dir in ctx.store.list_dirs(&keys::replay_project_prefix(project)).await? {
        let Some(day) = keys::parse_day_dir(&dir) else {
            continue;
        };
        if day < cutoff.as_str() {
            deleted += delete_prefix(ctx, &keys::replay_day_prefix(project, day), false).await?;
        }
    }
    if deleted > 0 {
        ctx.metrics.inc("gc_deleted_total", &[], deleted);
    }
    Ok(deleted)
}
