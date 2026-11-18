//! Shipper plans, `_done` markers and segment discovery
//! (`spec/manifest.md` §6).

use crate::error::Result;
use bytes::Bytes;
use la_core::control::Shards;
use la_core::store::ObjInfo;
use la_core::time::{day_start_ms, dt_hh};
use la_core::{keys, Store};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeSet, HashSet};

const HOUR_MS: i64 = 3_600_000;

/// Marks a plan that fences normal shipping while `rebuild` rewrites days
/// (see `rebuild.rs`). Normal plans do not carry it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RebuildSpec {
    pub id: String,
    pub from: String,
    pub to: String,
}

/// `wal/_plan/<shardkey>/<n:010>.json`
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Plan {
    pub n: u64,
    pub shardkey: String,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default)]
    pub holder: String,
    #[serde(default)]
    pub segments: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rebuild: Option<RebuildSpec>,
}

/// `(map version, shard)` of a `<version>-<shard>` shardkey.
pub fn parse_shardkey(shardkey: &str) -> Option<(u32, u32)> {
    let (v, s) = shardkey.split_once('-')?;
    Some((v.parse().ok()?, s.parse().ok()?))
}

/// Start of the hour a segment was closed in, from its key.
pub fn segment_hour_ms(key: &str) -> Option<i64> {
    let (dt, hh) = keys::segment_dt_hh(key)?;
    let h: i64 = hh.parse().ok().filter(|h| (0..24).contains(h))?;
    Some(day_start_ms(dt)? + h * HOUR_MS)
}

/// Whether the shard map in force when `segment` was closed is the one
/// `shardkey` belongs to.
pub fn segment_belongs(shards: &Shards, shardkey: &str, segment: &str) -> bool {
    match (parse_shardkey(shardkey), segment_hour_ms(segment)) {
        (Some((version, _)), Some(t)) => shards.map_at(t).version == version,
        _ => false,
    }
}

/// Highest plan number of a shardkey (0 = none).
pub async fn latest_plan_number(store: &Store, shardkey: &str) -> Result<u64> {
    Ok(store
        .list(&keys::wal_plan_dir(shardkey))
        .await?
        .iter()
        .filter_map(|o| keys::plan_number(&o.key))
        .max()
        .unwrap_or(0))
}

pub async fn load_plan(store: &Store, shardkey: &str, n: u64) -> Result<Option<Plan>> {
    Ok(store.get_json(&keys::wal_plan(shardkey, n)).await?)
}

/// Segment names with a `_done` marker for `shardkey`.
pub async fn done_set(store: &Store, shardkey: &str) -> Result<HashSet<String>> {
    Ok(store
        .list(&keys::wal_done_dir(shardkey))
        .await?
        .iter()
        .filter_map(|o| o.key.rsplit('/').next()?.strip_suffix(".ok").map(str::to_owned))
        .collect())
}

pub async fn mark_done(store: &Store, shardkey: &str, segment_key: &str) -> Result<()> {
    if let Some(name) = keys::segment_name(segment_key) {
        store.put(&keys::wal_done(shardkey, name), Bytes::new()).await?;
    }
    Ok(())
}

/// A plan is finished when every segment has its marker (and, for a rebuild
/// plan, the rebuild marker exists).
pub async fn plan_finished(store: &Store, plan: &Plan) -> Result<bool> {
    if plan.rebuild.is_some() {
        return Ok(store.exists(&keys::wal_rebuild_done(&plan.shardkey, plan.n)).await?);
    }
    for seg in &plan.segments {
        if let Some(name) = keys::segment_name(seg) {
            if !store.exists(&keys::wal_done(&plan.shardkey, name)).await? {
                return Ok(false);
            }
        }
    }
    Ok(true)
}

/// `wal/dt=…/hh=…` prefixes from `hours` before `now_ms` up to one hour ahead
/// (ingestion clocks may run slightly fast), oldest first.
pub fn hour_prefixes(now_ms: i64, hours: i64) -> Vec<String> {
    let top = now_ms.div_euclid(HOUR_MS) * HOUR_MS;
    (-hours..=1)
        .map(|i| {
            let (dt, hh) = dt_hh(top + i * HOUR_MS);
            keys::wal_hour_prefix(&dt, &hh)
        })
        .collect()
}

/// Shardkeys that can still receive segments inside the scan window: those of
/// every shard map in force at some hour of the window.
pub fn active_shardkeys(shards: &Shards, now_ms: i64, hours: i64) -> Vec<String> {
    let top = now_ms.div_euclid(HOUR_MS) * HOUR_MS;
    let mut out = BTreeSet::new();
    for i in -hours..=1 {
        let m = shards.map_at(top + i * HOUR_MS);
        for s in 0..m.shards.max(1) {
            out.insert(format!("{}-{}", m.version, s));
        }
    }
    out.into_iter().collect()
}

/// Lists segment objects under the given hour prefixes, sorted by key.
pub async fn list_segments(store: &Store, prefixes: &[String]) -> Result<Vec<ObjInfo>> {
    let mut out = Vec::new();
    for p in prefixes {
        out.extend(store.list(p).await?.into_iter().filter(|o| keys::segment_name(&o.key).is_some()));
    }
    out.sort_by(|a, b| a.key.cmp(&b.key));
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::control::ShardMap;

    #[test]
    fn plan_json_matches_the_spec_example() {
        let text = r#"{ "n": 1234, "shardkey": "1-0", "created_at": 1795046400000, "holder": "ship-a",
            "segments": [ "wal/dt=2026-11-18/hh=21/ing-01-000001795046.wal.zst" ] }"#;
        let p: Plan = serde_json::from_str(text).unwrap();
        assert_eq!((p.n, p.shardkey.as_str(), p.segments.len(), p.rebuild.is_none()), (1234, "1-0", 1, true));
        let back = serde_json::to_value(&p).unwrap();
        assert!(back.get("rebuild").is_none(), "normal plans serialize exactly the spec'd fields");
        assert_eq!(back, serde_json::from_str::<serde_json::Value>(text).unwrap());
    }

    #[test]
    fn segments_follow_the_shard_map_of_their_hour() {
        let from = day_start_ms("2026-11-18").unwrap() + 21 * HOUR_MS;
        let shards = Shards {
            maps: vec![
                ShardMap { version: 1, from_ms: 0, shards: 1 },
                ShardMap { version: 2, from_ms: from, shards: 2 },
            ],
        };
        let before = "wal/dt=2026-11-18/hh=20/n-000000000001.wal.zst";
        let after = "wal/dt=2026-11-18/hh=21/n-000000000002.wal.zst";
        assert_eq!(segment_hour_ms(after), Some(from));
        assert!(segment_belongs(&shards, "1-0", before) && !segment_belongs(&shards, "1-0", after));
        assert!(segment_belongs(&shards, "2-1", after) && !segment_belongs(&shards, "2-1", before));
        assert!(!segment_belongs(&shards, "2-1", "wal/_done/2-1/x.ok"));
        // Both maps are live while the window straddles the switch…
        assert_eq!(active_shardkeys(&shards, from + HOUR_MS, 48), ["1-0", "2-0", "2-1"]);
        // …and the old one drops out once the switch left the window.
        assert_eq!(active_shardkeys(&shards, from + 60 * HOUR_MS, 48), ["2-0", "2-1"]);
    }

    #[test]
    fn hour_prefixes_cover_the_window() {
        let now = day_start_ms("2026-11-18").unwrap() + 30 * 60_000;
        let p = hour_prefixes(now, 48);
        assert_eq!(p.len(), 50);
        assert_eq!(p.first().unwrap(), "wal/dt=2026-11-16/hh=00");
        assert_eq!(p[48], "wal/dt=2026-11-18/hh=00");
        assert_eq!(p.last().unwrap(), "wal/dt=2026-11-18/hh=01");
    }
}
