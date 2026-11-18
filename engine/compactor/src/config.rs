//! Compactor tuning knobs (all overridable through `LA_COMPACTOR_*`).

use la_core::metrics::Registry;
use la_core::Store;
use std::path::PathBuf;
use std::sync::Arc;

const MB: u64 = 1 << 20;
const HOUR_MS: i64 = 3_600_000;

#[derive(Debug, Clone)]
pub struct CompactorConfig {
    /// L1 trigger: more than this many L0 files in a day …
    pub l0_trigger_files: usize,
    /// … or more than this many L0 bytes.
    pub l0_trigger_bytes: u64,
    /// Open day: merge all L0+L1 files once there are more L1 files than this (0 = never).
    pub l1_max_files: usize,
    /// A day is finalized into L2 this long after it ended (UTC).
    pub l2_delay_ms: i64,
    /// Output files are closed at the next person boundary beyond this size.
    pub target_file_bytes: u64,
    /// Upper bound on files merged by one job (bounds memory).
    pub max_inputs: usize,
    pub batch_rows: usize,
    pub max_moved_rows: usize,
    pub max_person_uuids: usize,
    /// Inputs up to this size are merged from memory, larger ones from the spool directory.
    pub mem_input_bytes: u64,
    /// Unreferenced objects and old manifest versions are kept this long.
    pub gc_grace_ms: i64,
    /// Extra age required of a data file on top of the grace period.
    pub gc_slack_ms: i64,
    pub keep_manifests: usize,
    pub wal_retention_days: i64,
    pub tombstone_retention_days: i64,
    /// Where inputs/outputs are spooled; the system temp dir when unset.
    pub spool_dir: Option<PathBuf>,
    /// Days (counting back from today) examined on every cycle.
    pub hot_days: usize,
    pub interval_ms: u64,
    /// Every day of every project is examined this often (also runs GC).
    pub full_scan_interval_ms: i64,
    /// `l0_files{project}` is exported for this many projects.
    pub top_projects: usize,
}

impl Default for CompactorConfig {
    fn default() -> Self {
        Self {
            l0_trigger_files: 8,
            l0_trigger_bytes: 256 * MB,
            l1_max_files: 32,
            l2_delay_ms: 6 * HOUR_MS,
            target_file_bytes: 512 * MB,
            max_inputs: 32,
            batch_rows: 4096,
            max_moved_rows: 1_000_000,
            max_person_uuids: 2_000_000,
            mem_input_bytes: 32 * MB,
            gc_grace_ms: 24 * HOUR_MS,
            gc_slack_ms: HOUR_MS,
            keep_manifests: 20,
            wal_retention_days: 30,
            tombstone_retention_days: 400,
            spool_dir: None,
            hot_days: 3,
            interval_ms: 60_000,
            full_scan_interval_ms: HOUR_MS,
            top_projects: 20,
        }
    }
}

fn env<T: std::str::FromStr>(name: &str, default: T) -> T {
    std::env::var(name).ok().and_then(|v| v.trim().parse().ok()).unwrap_or(default)
}

impl CompactorConfig {
    pub fn from_env() -> Self {
        let d = Self::default();
        Self {
            l0_trigger_files: env("LA_COMPACTOR_L0_FILES", d.l0_trigger_files),
            l0_trigger_bytes: env("LA_COMPACTOR_L0_MB", d.l0_trigger_bytes / MB) * MB,
            l1_max_files: env("LA_COMPACTOR_L1_MAX_FILES", d.l1_max_files),
            l2_delay_ms: env("LA_COMPACTOR_L2_DELAY_HOURS", d.l2_delay_ms / HOUR_MS) * HOUR_MS,
            target_file_bytes: env("LA_COMPACTOR_TARGET_MB", d.target_file_bytes / MB) * MB,
            max_inputs: env("LA_COMPACTOR_MAX_INPUTS", d.max_inputs).max(2),
            batch_rows: env("LA_COMPACTOR_BATCH_ROWS", d.batch_rows).max(16),
            max_moved_rows: env("LA_COMPACTOR_MAX_MOVED_ROWS", d.max_moved_rows).max(1),
            max_person_uuids: env("LA_COMPACTOR_MAX_PERSON_UUIDS", d.max_person_uuids),
            mem_input_bytes: env("LA_COMPACTOR_MEM_INPUT_MB", d.mem_input_bytes / MB) * MB,
            gc_grace_ms: env("LA_COMPACTOR_GC_GRACE_HOURS", d.gc_grace_ms / HOUR_MS) * HOUR_MS,
            gc_slack_ms: d.gc_slack_ms,
            keep_manifests: env("LA_COMPACTOR_KEEP_MANIFESTS", d.keep_manifests).max(1),
            wal_retention_days: env("LA_WAL_RETENTION_DAYS", d.wal_retention_days).max(1),
            tombstone_retention_days: env("LA_TOMBSTONE_RETENTION_DAYS", d.tombstone_retention_days).max(1),
            spool_dir: std::env::var("LA_COMPACTOR_SPOOL").ok().filter(|v| !v.is_empty()).map(PathBuf::from),
            hot_days: env("LA_COMPACTOR_HOT_DAYS", d.hot_days).max(1),
            interval_ms: env("LA_COMPACTOR_INTERVAL_SECS", d.interval_ms / 1000).max(1) * 1000,
            full_scan_interval_ms: env("LA_COMPACTOR_FULL_SCAN_MINUTES", d.full_scan_interval_ms / 60_000) * 60_000,
            top_projects: d.top_projects,
        }
    }
}

/// Everything a compactor routine needs.
#[derive(Clone)]
pub struct Ctx {
    pub store: Store,
    pub cfg: CompactorConfig,
    pub metrics: Arc<Registry>,
    pub holder: String,
}

impl Ctx {
    pub fn new(store: Store, cfg: CompactorConfig) -> Self {
        Self { store, cfg, metrics: Arc::new(Registry::new("la_compactor_")), holder: "compactor".into() }
    }

    pub fn spool(&self) -> std::io::Result<tempfile::TempDir> {
        match &self.cfg.spool_dir {
            Some(dir) => {
                std::fs::create_dir_all(dir)?;
                tempfile::Builder::new().prefix("la-compact-").tempdir_in(dir)
            }
            None => tempfile::Builder::new().prefix("la-compact-").tempdir(),
        }
    }
}
