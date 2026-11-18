//! `wal/` lifecycle (`spec/manifest.md` §10): segments, plans, `_done`
//! markers and quarantine records are removed after the WAL retention; the
//! highest plan of every shardkey is always kept.

use crate::config::Ctx;
use la_core::error::Result;
use la_core::keys;
use la_core::time::{day_of, DAY_MS};

/// `_done` markers outlive their segments by this much, so a marker is never
/// removed while its segment could still be discovered.
const DONE_EXTRA_MS: i64 = 3 * DAY_MS;

pub async fn wal_lifecycle(ctx: &Ctx, now_ms: i64) -> Result<u64> {
    let store = &ctx.store;
    let retention_ms = ctx.cfg.wal_retention_days * DAY_MS;
    let cutoff_day = day_of(now_ms - retention_ms);
    let mut deleted = 0u64;

    // 1. Segments, by the day in their key (the segment's closing time).
    for dir in store.list_dirs("wal").await? {
        let Some(day) = dir.strip_prefix("dt=").filter(|d| la_core::time::parse_day(d).is_some()) else {
            continue;
        };
        if day < cutoff_day.as_str() {
            for o in store.list(&keys::wal_day_prefix(day)).await? {
                store.delete(&o.key).await?;
                deleted += 1;
            }
        }
    }
    // 2. Plans (never the highest one) and quarantine records, by age.
    for shardkey in store.list_dirs("wal/_plan").await? {
        let objs = store.list(&keys::wal_plan_dir(&shardkey)).await?;
        let highest = objs.iter().filter_map(|o| keys::plan_number(&o.key)).max();
        for o in &objs {
            let n = keys::plan_number(&o.key);
            if n.is_some() && n != highest && o.last_modified_ms < now_ms - retention_ms {
                store.delete(&o.key).await?;
                deleted += 1;
            }
        }
    }
    for shardkey in store.list_dirs("wal/_quarantine").await? {
        for o in store.list(&keys::wal_quarantine_dir(&shardkey)).await? {
            if o.last_modified_ms < now_ms - retention_ms {
                store.delete(&o.key).await?;
                deleted += 1;
            }
        }
    }
    // 3. `_done` markers, strictly after their segments.
    for shardkey in store.list_dirs("wal/_done").await? {
        for o in store.list(&keys::wal_done_dir(&shardkey)).await? {
            if o.last_modified_ms < now_ms - retention_ms - DONE_EXTRA_MS {
                store.delete(&o.key).await?;
                deleted += 1;
            }
        }
    }
    if deleted > 0 {
        ctx.metrics.inc("gc_deleted_total", &[], deleted);
        tracing::info!(deleted, "wal lifecycle");
    }
    Ok(deleted)
}
