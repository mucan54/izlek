//! Environment configuration of the query service (see README for the table).

use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct Config {
    /// `LA_QUERY_LISTEN`
    pub listen: String,
    /// `LA_QUERY_INTERNAL_SECRET`; `None` disables the internal header path.
    pub internal_secret: Option<String>,
    /// `LA_QUERY_CACHE_DIR`; `None` disables the local file cache.
    pub cache_dir: Option<PathBuf>,
    /// `LA_QUERY_CACHE_BYTES`
    pub cache_bytes: u64,
    /// `LA_QUERY_FOOTER_CACHE_BYTES`
    pub footer_cache_bytes: u64,
    /// `LA_QUERY_RESULT_CACHE_BYTES` (0 disables the result cache)
    pub result_cache_bytes: u64,
    /// `LA_QUERY_MEMORY_BYTES`: per-query memory pool.
    pub memory_bytes: u64,
    /// `LA_QUERY_TIMEOUT_MS`
    pub timeout: Duration,
    /// `LA_QUERY_SCAN_LIMIT_BYTES`: default per-query scan budget when the
    /// project has none (0 = unlimited).
    pub scan_limit_bytes: u64,
    /// `LA_QUERY_MAX_CONCURRENT`: default per-project concurrency when
    /// `projects.json` does not list the project.
    pub max_concurrent: u32,
    /// `LA_QUERY_QUEUE_MAX`: queries allowed to wait per project.
    pub queue_max: u32,
    /// `LA_QUERY_PARTITIONS`: scan/aggregation parallelism per query.
    pub partitions: usize,
    /// `LA_QUERY_MANIFEST_TTL_MS`
    pub manifest_ttl: Duration,
    /// `LA_QUERY_TOMBSTONE_TTL_MS` (clamped to 60 s)
    pub tombstone_ttl: Duration,
    /// `LA_QUERY_CONTROL_REFRESH_MS`
    pub control_refresh: Duration,
    /// `LA_QUERY_SLOW_MS`
    pub slow_query: Duration,
    /// `LA_QUERY_PUSHDOWN_FILTERS`: evaluate filters inside the Parquet decoder.
    pub pushdown_filters: bool,
    /// `LA_QUERY_SQL_MAX_ROWS`
    pub sql_max_rows: usize,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            listen: "0.0.0.0:8090".into(),
            internal_secret: None,
            cache_dir: None,
            cache_bytes: 20 << 30,
            footer_cache_bytes: 256 << 20,
            result_cache_bytes: 64 << 20,
            memory_bytes: 2 << 30,
            timeout: Duration::from_secs(60),
            scan_limit_bytes: 0,
            max_concurrent: 4,
            queue_max: 16,
            partitions: std::thread::available_parallelism().map(|n| n.get()).unwrap_or(4).clamp(1, 16),
            manifest_ttl: Duration::from_secs(10),
            tombstone_ttl: Duration::from_secs(30),
            control_refresh: Duration::from_secs(15),
            slow_query: Duration::from_secs(5),
            pushdown_filters: true,
            sql_max_rows: 10_000,
        }
    }
}

fn var(name: &str) -> Option<String> {
    std::env::var(name).ok().map(|v| v.trim().to_owned()).filter(|v| !v.is_empty())
}

fn num<T: std::str::FromStr>(name: &str, default: T) -> Result<T, String> {
    match var(name) {
        None => Ok(default),
        Some(v) => v.parse().map_err(|_| format!("{name}: invalid value {v:?}")),
    }
}

impl Config {
    pub fn from_env() -> Result<Self, String> {
        let d = Config::default();
        let ms = |name: &str, default: Duration| -> Result<Duration, String> {
            Ok(Duration::from_millis(num(name, default.as_millis() as u64)?))
        };
        let pushdown = match var("LA_QUERY_PUSHDOWN_FILTERS").as_deref() {
            None => d.pushdown_filters,
            Some("1" | "true" | "on") => true,
            Some("0" | "false" | "off") => false,
            Some(v) => return Err(format!("LA_QUERY_PUSHDOWN_FILTERS: invalid value {v:?}")),
        };
        Ok(Self {
            listen: var("LA_QUERY_LISTEN").unwrap_or(d.listen),
            internal_secret: var("LA_QUERY_INTERNAL_SECRET"),
            cache_dir: var("LA_QUERY_CACHE_DIR").map(PathBuf::from),
            cache_bytes: num("LA_QUERY_CACHE_BYTES", d.cache_bytes)?,
            footer_cache_bytes: num("LA_QUERY_FOOTER_CACHE_BYTES", d.footer_cache_bytes)?,
            result_cache_bytes: num("LA_QUERY_RESULT_CACHE_BYTES", d.result_cache_bytes)?,
            memory_bytes: num("LA_QUERY_MEMORY_BYTES", d.memory_bytes)?.max(16 << 20),
            timeout: ms("LA_QUERY_TIMEOUT_MS", d.timeout)?,
            scan_limit_bytes: num("LA_QUERY_SCAN_LIMIT_BYTES", d.scan_limit_bytes)?,
            max_concurrent: num("LA_QUERY_MAX_CONCURRENT", d.max_concurrent)?.max(1),
            queue_max: num("LA_QUERY_QUEUE_MAX", d.queue_max)?,
            partitions: num("LA_QUERY_PARTITIONS", d.partitions)?.clamp(1, 64),
            manifest_ttl: ms("LA_QUERY_MANIFEST_TTL_MS", d.manifest_ttl)?,
            tombstone_ttl: ms("LA_QUERY_TOMBSTONE_TTL_MS", d.tombstone_ttl)?.min(Duration::from_secs(60)),
            control_refresh: ms("LA_QUERY_CONTROL_REFRESH_MS", d.control_refresh)?.max(Duration::from_millis(100)),
            slow_query: ms("LA_QUERY_SLOW_MS", d.slow_query)?,
            pushdown_filters: pushdown,
            sql_max_rows: num("LA_QUERY_SQL_MAX_ROWS", d.sql_max_rows)?.clamp(1, 10_000),
        })
    }
}
