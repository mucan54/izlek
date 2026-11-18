//! Shipper configuration (environment variables, see the README).

use crate::exec::FailHook;
use la_core::metrics::Registry;
use la_core::Store;
use std::sync::Arc;

const MB: u64 = 1 << 20;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UnknownProjects {
    /// Entries of projects missing from a published `control/projects.json`
    /// are rejected (`unknown_project`).
    Reject,
    Accept,
}

#[derive(Debug, Clone)]
pub struct ShipConfig {
    /// A plan is cut at most this often …
    pub batch_interval_ms: u64,
    /// … unless this many (estimated) decompressed bytes are waiting.
    pub batch_bytes: u64,
    pub poll_interval_ms: u64,
    /// `wal/dt=…/hh=…/` prefixes of this many past hours are scanned.
    pub scan_hours: i64,
    /// The whole window is re-listed this often; in between only the newest hours.
    pub full_scan_interval_ms: u64,
    /// Segments decompressing to more than this are quarantined.
    pub max_segment_bytes: u64,
    /// Shardkeys (`1-0`) or shard indices (`0`) this process may run; empty = all.
    pub shards: Vec<String>,
    pub unknown_projects: UnknownProjects,
    /// Tombstone listings are reused for this long.
    pub tombstone_refresh_ms: i64,
    /// Starting guess for decompressed/compressed segment size.
    pub initial_ratio: f64,
}

impl Default for ShipConfig {
    fn default() -> Self {
        Self {
            batch_interval_ms: 30_000,
            // Peak memory is roughly 14× this for small events (≈ 1.5 KB per row).
            batch_bytes: 64 * MB,
            poll_interval_ms: 2_000,
            scan_hours: 48,
            full_scan_interval_ms: 60_000,
            max_segment_bytes: 1024 * MB,
            shards: Vec::new(),
            unknown_projects: UnknownProjects::Reject,
            tombstone_refresh_ms: 30_000,
            initial_ratio: 6.0,
        }
    }
}

fn env<T: std::str::FromStr>(name: &str, default: T) -> T {
    std::env::var(name).ok().and_then(|v| v.trim().parse().ok()).unwrap_or(default)
}

impl ShipConfig {
    pub fn from_env() -> Self {
        let d = Self::default();
        let shards = std::env::var("LA_SHIPPER_SHARDS")
            .ok()
            .map(|v| v.split(',').map(|s| s.trim().to_owned()).filter(|s| !s.is_empty() && s != "*").collect())
            .unwrap_or_default();
        Self {
            batch_interval_ms: env("LA_SHIPPER_BATCH_SECS", d.batch_interval_ms / 1000) * 1000,
            batch_bytes: env("LA_SHIPPER_BATCH_MB", d.batch_bytes / MB).max(1) * MB,
            poll_interval_ms: env("LA_SHIPPER_POLL_SECS", d.poll_interval_ms / 1000).max(1) * 1000,
            scan_hours: env("LA_SHIPPER_SCAN_HOURS", d.scan_hours).max(1),
            full_scan_interval_ms: env("LA_SHIPPER_FULL_SCAN_SECS", d.full_scan_interval_ms / 1000) * 1000,
            max_segment_bytes: env("LA_SHIPPER_MAX_SEGMENT_MB", d.max_segment_bytes / MB).max(1) * MB,
            shards,
            unknown_projects: match std::env::var("LA_SHIPPER_UNKNOWN_PROJECTS").as_deref() {
                Ok("accept") => UnknownProjects::Accept,
                _ => UnknownProjects::Reject,
            },
            tombstone_refresh_ms: d.tombstone_refresh_ms,
            initial_ratio: d.initial_ratio,
        }
    }

    /// Whether this process may work on `shardkey` (`<map version>-<shard>`).
    pub fn wants_shard(&self, shardkey: &str) -> bool {
        self.shards.is_empty()
            || self.shards.iter().any(|s| s == shardkey || shardkey.rsplit('-').next() == Some(s.as_str()))
    }
}

/// Shared handles of a shipper process.
#[derive(Clone)]
pub struct ShipCtx {
    pub store: Store,
    pub cfg: ShipConfig,
    pub metrics: Arc<Registry>,
    pub holder: String,
    /// Wall clock (unix ms); replaceable so tests are independent of today's date.
    pub now: Arc<dyn Fn() -> i64 + Send + Sync>,
    /// Failure injection for the crash-safety tests.
    pub fail: Option<FailHook>,
}

impl ShipCtx {
    pub fn new(store: Store, cfg: ShipConfig) -> Self {
        Self {
            store,
            cfg,
            metrics: Arc::new(Registry::new("la_shipper_")),
            holder: "shipper".into(),
            now: Arc::new(la_core::time::now_ms),
            fail: None,
        }
    }

    pub fn now_ms(&self) -> i64 {
        (self.now)()
    }

    pub fn with_now(mut self, now_ms: i64) -> Self {
        self.now = Arc::new(move || now_ms);
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shard_filter_accepts_keys_and_indices() {
        let mut c = ShipConfig::default();
        assert!(c.wants_shard("1-0") && c.wants_shard("7-3"));
        c.shards = vec!["1".into(), "2-0".into()];
        assert!(c.wants_shard("1-1") && c.wants_shard("5-1") && c.wants_shard("2-0"));
        assert!(!c.wants_shard("1-0") && !c.wants_shard("2-2"));
    }
}
