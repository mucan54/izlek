//! Query execution wrapper: limits (design §11.7), result cache
//! (`spec/query.md` §9), metrics (§16.1) and the slow-query log (§16.5).

use crate::error::{ApiError, ApiResult};
use crate::filecache::QueryStats;
use crate::json::canonical;
use crate::session::{QueryCx, POOL_BYTES};
use crate::state::Engine;
use crate::{q_events, q_funnel, q_retention, q_sql, q_trends};
use la_core::metrics::Registry;
use lru::LruCache;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Trends,
    Funnel,
    Retention,
    Events,
    Sql,
}

impl Kind {
    pub fn name(self) -> &'static str {
        match self {
            Kind::Trends => "trends",
            Kind::Funnel => "funnel",
            Kind::Retention => "retention",
            Kind::Events => "events",
            Kind::Sql => "sql",
        }
    }
}

enum Request {
    Trends(q_trends::TrendsRequest),
    Funnel(q_funnel::FunnelRequest),
    Retention(q_retention::RetentionRequest),
    Events(q_events::EventsRequest),
    Sql(q_sql::SqlRequest),
}

impl Request {
    fn parse(kind: Kind, body: &Value) -> ApiResult<Self> {
        Ok(match kind {
            Kind::Trends => Request::Trends(q_trends::TrendsRequest::parse(body)?),
            Kind::Funnel => Request::Funnel(q_funnel::FunnelRequest::parse(body)?),
            Kind::Retention => Request::Retention(q_retention::RetentionRequest::parse(body)?),
            Kind::Events => Request::Events(q_events::EventsRequest::parse(body)?),
            Kind::Sql => Request::Sql(q_sql::SqlRequest::parse(body)?),
        })
    }

    fn range(&self) -> (i64, i64) {
        match self {
            Request::Trends(r) => (r.from_ms, r.to_ms),
            Request::Funnel(r) => (r.from_ms, r.to_ms),
            Request::Retention(r) => (r.from_ms, r.to_ms),
            Request::Events(r) => (r.from_ms, r.to_ms),
            Request::Sql(r) => (r.from_ms, r.to_ms),
        }
    }

    fn uses_person(&self) -> bool {
        match self {
            Request::Trends(r) => r.uses_person(),
            Request::Funnel(r) => r.uses_person(),
            Request::Retention(r) => r.uses_person(),
            Request::Events(r) => r.uses_person(),
            Request::Sql(r) => r.uses_person(),
        }
    }

    async fn run(&self, cx: &QueryCx) -> ApiResult<Value> {
        match self {
            Request::Trends(r) => q_trends::run(cx, r).await,
            Request::Funnel(r) => q_funnel::run(cx, r).await,
            Request::Retention(r) => q_retention::run(cx, r).await,
            Request::Events(r) => q_events::run(cx, r).await,
            Request::Sql(r) => q_sql::run(cx, r).await,
        }
    }
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

struct Gate {
    limit: u32,
    permits: Arc<Semaphore>,
    waiting: AtomicU64,
}

/// Per-project concurrency limit with a bounded wait queue.
pub struct Limiter {
    gates: Mutex<HashMap<u32, Arc<Gate>>>,
    queue_max: u64,
    pub queued: AtomicI64,
}

struct Waiting<'a> {
    gate: &'a Gate,
    total: &'a AtomicI64,
}

impl Drop for Waiting<'_> {
    fn drop(&mut self) {
        self.gate.waiting.fetch_sub(1, Ordering::Relaxed);
        self.total.fetch_sub(1, Ordering::Relaxed);
    }
}

impl Limiter {
    pub fn new(queue_max: u32) -> Self {
        Self { gates: Mutex::new(HashMap::new()), queue_max: queue_max as u64, queued: AtomicI64::new(0) }
    }

    /// Waits for a slot; `429` when the project's queue is already full.
    pub async fn acquire(&self, project: u32, limit: u32) -> ApiResult<OwnedSemaphorePermit> {
        let limit = limit.max(1);
        let gate = {
            let mut gates = self.gates.lock().unwrap();
            match gates.get(&project) {
                Some(g) if g.limit == limit => Arc::clone(g),
                _ => {
                    // First use, or the project's limit changed: queries
                    // holding old permits simply finish on the old gate.
                    let g = Arc::new(Gate {
                        limit,
                        permits: Arc::new(Semaphore::new(limit as usize)),
                        waiting: AtomicU64::new(0),
                    });
                    gates.insert(project, Arc::clone(&g));
                    g
                }
            }
        };
        if let Ok(p) = Arc::clone(&gate.permits).try_acquire_owned() {
            return Ok(p);
        }
        if gate.waiting.fetch_add(1, Ordering::Relaxed) >= self.queue_max {
            gate.waiting.fetch_sub(1, Ordering::Relaxed);
            return Err(ApiError::queue_full());
        }
        self.queued.fetch_add(1, Ordering::Relaxed);
        let _waiting = Waiting { gate: &gate, total: &self.queued };
        Arc::clone(&gate.permits).acquire_owned().await.map_err(|_| ApiError::internal("limiter closed"))
    }
}

// ---------------------------------------------------------------------------
// Result cache
// ---------------------------------------------------------------------------

struct Entry {
    body: Arc<Value>,
    files: u64,
    manifests: Arc<BTreeMap<String, u64>>,
    bytes: usize,
}

struct CacheState {
    lru: LruCache<[u8; 32], Entry>,
    bytes: usize,
}

/// Results keyed by request and data versions: a new manifest version (or
/// identity/person version, or tombstone) changes the key, so stale entries
/// are never served — they just age out of the LRU.
pub struct ResultCache {
    cap: usize,
    state: Mutex<CacheState>,
    pub hits: AtomicU64,
    pub misses: AtomicU64,
}

impl ResultCache {
    pub fn new(cap_bytes: u64) -> Self {
        Self {
            cap: cap_bytes as usize,
            state: Mutex::new(CacheState { lru: LruCache::unbounded(), bytes: 0 }),
            hits: AtomicU64::new(0),
            misses: AtomicU64::new(0),
        }
    }

    fn get(&self, key: &[u8; 32]) -> Option<(Arc<Value>, u64, Arc<BTreeMap<String, u64>>)> {
        if self.cap == 0 {
            return None;
        }
        let hit = self.state.lock().unwrap().lru.get(key).map(|e| (e.body.clone(), e.files, e.manifests.clone()));
        let counter = if hit.is_some() { &self.hits } else { &self.misses };
        counter.fetch_add(1, Ordering::Relaxed);
        hit
    }

    fn put(&self, key: [u8; 32], body: Arc<Value>, bytes: usize, files: u64, manifests: Arc<BTreeMap<String, u64>>) {
        // One entry may not monopolize the cache.
        if self.cap == 0 || bytes > self.cap / 4 {
            return;
        }
        let mut st = self.state.lock().unwrap();
        if let Some(old) = st.lru.put(key, Entry { body, files, manifests, bytes }) {
            st.bytes -= old.bytes;
        }
        st.bytes += bytes;
        while st.bytes > self.cap {
            match st.lru.pop_lru() {
                Some((_, e)) => st.bytes -= e.bytes,
                None => break,
            }
        }
    }

    pub fn len(&self) -> usize {
        self.state.lock().unwrap().lru.len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

pub struct Service {
    pub engine: Arc<Engine>,
    pub metrics: Registry,
    pub limiter: Limiter,
    pub cache: ResultCache,
}

struct Outcome {
    body: Value,
    cached: bool,
    bytes_scanned: u64,
    files: u64,
    manifests: Arc<BTreeMap<String, u64>>,
    plans: Vec<String>,
}

impl Service {
    pub fn new(engine: Arc<Engine>) -> Arc<Self> {
        let cfg = &engine.cfg;
        Arc::new(Self {
            limiter: Limiter::new(cfg.queue_max),
            cache: ResultCache::new(cfg.result_cache_bytes),
            metrics: Registry::new("la_query_"),
            engine,
        })
    }

    fn cache_key(project: u32, kind: Kind, body: &Value, versions: &str) -> [u8; 32] {
        let mut h = Sha256::new();
        h.update(project.to_le_bytes());
        h.update([0]);
        h.update(kind.name().as_bytes());
        h.update([0]);
        h.update(canonical(body).as_bytes());
        h.update([0]);
        h.update(versions.as_bytes());
        h.finalize().into()
    }

    async fn execute(&self, project: u32, kind: Kind, req: &Request, body: &Value) -> ApiResult<Outcome> {
        let (from_ms, to_ms) = req.range();
        let snap = Arc::new(self.engine.snapshot(project, from_ms, to_ms, req.uses_person()).await?);
        let manifests = Arc::new(snap.manifest_versions());
        let key = Self::cache_key(project, kind, body, &snap.version_fingerprint());
        if let Some((cached, files, manifests)) = self.cache.get(&key) {
            return Ok(Outcome { body: (*cached).clone(), cached: true, bytes_scanned: 0, files, manifests, plans: vec![] });
        }
        let limit = match snap.config.scan_limit_bytes {
            0 => self.engine.cfg.scan_limit_bytes,
            n => n,
        };
        let stats = QueryStats::new(limit);
        let cx = QueryCx::new(&self.engine, snap, Arc::clone(&stats))?;
        let result = req.run(&cx).await;
        let scanned = stats.scanned();
        self.metrics.inc("bytes_scanned_total", &[], scanned);
        let value = result?;
        let files = stats.files.load(Ordering::Relaxed);
        let size = serde_json::to_vec(&value).map(|v| v.len()).unwrap_or(usize::MAX);
        self.cache.put(key, Arc::new(value.clone()), size, files, Arc::clone(&manifests));
        Ok(Outcome { body: value, cached: false, bytes_scanned: scanned, files, manifests, plans: cx.plan_summaries() })
    }

    /// Runs one query for `project` and returns the response body with `meta`.
    pub async fn query(&self, project: u32, kind: Kind, body: &Value) -> ApiResult<Value> {
        let started = Instant::now();
        let result = self.query_inner(project, kind, body, started).await;
        let elapsed = started.elapsed();
        let status = match &result {
            Ok(_) => "ok".to_owned(),
            Err(e) => e.code.to_owned(),
        };
        self.metrics.inc("queries_total", &[("type", kind.name()), ("status", &status)], 1);
        self.metrics.observe("query_seconds", &[("type", kind.name())], elapsed.as_secs_f64());
        result
    }

    async fn query_inner(&self, project: u32, kind: Kind, body: &Value, started: Instant) -> ApiResult<Value> {
        let req = Request::parse(kind, body)?;
        let limit = self.engine.project_config(project).max_concurrent_queries;
        let timeout = self.engine.cfg.timeout;
        // The time limit covers queueing and execution; dropping the future
        // cancels the query's tasks.
        let run = async {
            let _permit = self.limiter.acquire(project, limit).await?;
            self.execute(project, kind, &req, body).await
        };
        let outcome = match tokio::time::timeout(timeout, run).await {
            Ok(r) => r?,
            Err(_) => return Err(ApiError::timeout()),
        };
        let elapsed = started.elapsed();
        if elapsed >= self.engine.cfg.slow_query && !outcome.cached {
            self.log_slow(project, kind, body, elapsed, &outcome);
        }
        let mut response = outcome.body;
        response["meta"] = json!({
            "elapsed_ms": elapsed.as_millis() as u64,
            "bytes_scanned": outcome.bytes_scanned,
            "files": outcome.files,
            "cached": outcome.cached,
            "manifests": *outcome.manifests,
        });
        Ok(response)
    }

    /// Slow-query log: plan shape and I/O only. The request is identified by
    /// a hash — its text may contain literals (event content, PII).
    fn log_slow(&self, project: u32, kind: Kind, body: &Value, elapsed: Duration, outcome: &Outcome) {
        let digest: String = Sha256::digest(canonical(body).as_bytes()).iter().take(8).map(|b| format!("{b:02x}")).collect();
        tracing::warn!(
            target: "la_query::slow",
            project,
            kind = kind.name(),
            request_sha = %digest,
            elapsed_ms = elapsed.as_millis() as u64,
            bytes_scanned = outcome.bytes_scanned,
            files = outcome.files,
            days = outcome.manifests.len(),
            plan = %outcome.plans.join(" | "),
            "slow query"
        );
    }

    /// Prometheus text (design §16.1).
    pub fn render_metrics(&self) -> String {
        let hits = self.cache.hits.load(Ordering::Relaxed) as f64;
        let misses = self.cache.misses.load(Ordering::Relaxed) as f64;
        let ratio = if hits + misses > 0.0 { hits / (hits + misses) } else { 0.0 };
        let m = &self.metrics;
        m.set("cache_hit_ratio", &[], ratio);
        m.set("cache_hits_total", &[], hits);
        m.set("cache_misses_total", &[], misses);
        m.set("queue_depth", &[], self.limiter.queued.load(Ordering::Relaxed).max(0) as f64);
        m.set("memory_pool_bytes", &[], POOL_BYTES.load(Ordering::Relaxed).max(0) as f64);
        let files = &self.engine.files;
        let c = &files.counters;
        m.set("file_cache_bytes", &[], files.disk_bytes() as f64);
        m.set("file_cache_files", &[], files.disk_files() as f64);
        m.set("footer_cache_bytes", &[], files.footer_bytes() as f64);
        m.set("file_cache_hits_total", &[], c.disk_hits.load(Ordering::Relaxed) as f64);
        m.set("file_cache_misses_total", &[], c.disk_misses.load(Ordering::Relaxed) as f64);
        m.set("file_cache_evictions_total", &[], c.evictions.load(Ordering::Relaxed) as f64);
        m.set("remote_bytes_total", &[], c.remote_bytes.load(Ordering::Relaxed) as f64);
        m.set("ready", &[], if self.engine.ready() { 1.0 } else { 0.0 });
        m.render()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn limiter_queues_then_rejects() {
        let l = Arc::new(Limiter::new(1));
        let a = l.acquire(1, 1).await.unwrap();
        // Another project is independent.
        let _other = l.acquire(2, 1).await.unwrap();
        // One waiter fits in the queue…
        let l2 = Arc::clone(&l);
        let waiter = tokio::spawn(async move { l2.acquire(1, 1).await.map(|_| ()) });
        for _ in 0..200 {
            if l.queued.load(Ordering::Relaxed) == 1 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
        assert_eq!(l.queued.load(Ordering::Relaxed), 1);
        // …the next one is turned away.
        assert_eq!(l.acquire(1, 1).await.unwrap_err().status, 429);
        drop(a);
        waiter.await.unwrap().unwrap();
        assert_eq!(l.queued.load(Ordering::Relaxed), 0);
        // A cancelled waiter leaves the queue.
        let held = l.acquire(1, 1).await.unwrap();
        let l3 = Arc::clone(&l);
        let cancelled = tokio::spawn(async move { l3.acquire(1, 1).await.map(|_| ()) });
        tokio::time::sleep(Duration::from_millis(20)).await;
        cancelled.abort();
        let _ = cancelled.await;
        assert_eq!(l.queued.load(Ordering::Relaxed), 0);
        drop(held);
        assert!(l.acquire(1, 1).await.is_ok());
    }

    #[test]
    fn result_cache_is_bounded() {
        let c = ResultCache::new(1000);
        let m = Arc::new(BTreeMap::new());
        for i in 0..10u8 {
            c.put([i; 32], Arc::new(json!(i)), 200, 1, m.clone());
        }
        assert_eq!(c.len(), 5);
        assert!(c.get(&[0; 32]).is_none());
        assert!(c.get(&[9; 32]).is_some());
        c.put([42; 32], Arc::new(json!("big")), 600, 1, m.clone());
        assert!(c.get(&[42; 32]).is_none(), "oversized entries are not cached");
        let off = ResultCache::new(0);
        off.put([1; 32], Arc::new(json!(1)), 1, 1, m);
        assert!(off.get(&[1; 32]).is_none());
    }
}
