//! File access for the scan layer (design §11.2): a local-disk LRU of whole
//! data files, an in-memory cache of Parquet footers (+ page indexes) and the
//! range-reading `AsyncFileReader` that sits on top of both.
//!
//! Data files are immutable once listed in a manifest, so entries are never
//! invalidated — only evicted. The cache key is `<object key>@<size>`: the one
//! exception to immutability (an L0 file rewritten under the same name by a
//! re-executed shipper plan) changes the size in practice, and a reader that
//! fails on a stale entry drops it and retries against the store.

use crate::error::ScanLimitExceeded;
use bytes::Bytes;
use datafusion::datasource::listing::PartitionedFile;
use datafusion::datasource::physical_plan::ParquetFileReaderFactory;
use datafusion::physical_plan::metrics::ExecutionPlanMetricsSet;
use futures::future::BoxFuture;
use futures::{FutureExt, StreamExt};
use lru::LruCache;
use object_store::path::Path;
use object_store::{ObjectStore, ObjectStoreExt};
use parquet::arrow::arrow_reader::ArrowReaderOptions;
use parquet::arrow::async_reader::{AsyncFileReader, MetadataFetch};
use parquet::errors::{ParquetError, Result as PqResult};
use parquet::file::metadata::{PageIndexPolicy, ParquetMetaData, ParquetMetaDataReader};
use sha2::{Digest, Sha256};
use std::collections::HashSet;
use std::ops::Range;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

/// Bytes fetched from the tail of a file in the first footer request.
const FOOTER_HINT: usize = 256 * 1024;
const MAX_PARALLEL_DOWNLOADS: usize = 2;

/// Per-query I/O accounting and the scanned-bytes budget.
#[derive(Debug, Default)]
pub struct QueryStats {
    pub bytes_scanned: AtomicU64,
    pub files: AtomicU64,
    /// 0 = unlimited.
    pub scan_limit: u64,
}

impl QueryStats {
    pub fn new(scan_limit: u64) -> Arc<Self> {
        Arc::new(Self { scan_limit, ..Default::default() })
    }

    fn charge(&self, n: u64) -> PqResult<()> {
        let total = self.bytes_scanned.fetch_add(n, Ordering::Relaxed) + n;
        if self.scan_limit > 0 && total > self.scan_limit {
            return Err(ParquetError::External(Box::new(ScanLimitExceeded(self.scan_limit))));
        }
        Ok(())
    }

    pub fn scanned(&self) -> u64 {
        self.bytes_scanned.load(Ordering::Relaxed)
    }
}

#[derive(Debug, Default)]
pub struct CacheCounters {
    pub disk_hits: AtomicU64,
    pub disk_misses: AtomicU64,
    pub footer_hits: AtomicU64,
    pub footer_misses: AtomicU64,
    pub downloads: AtomicU64,
    pub evictions: AtomicU64,
    pub remote_bytes: AtomicU64,
}

struct DiskState {
    /// file name → size
    lru: LruCache<String, u64>,
    bytes: u64,
}

struct FooterState {
    lru: LruCache<String, (Arc<ParquetMetaData>, usize)>,
    bytes: usize,
}

pub struct FileCache {
    store: Arc<dyn ObjectStore>,
    dir: Option<PathBuf>,
    cap_bytes: u64,
    footer_cap: usize,
    disk: Mutex<DiskState>,
    footers: Mutex<FooterState>,
    downloading: Mutex<HashSet<String>>,
    download_slots: Arc<tokio::sync::Semaphore>,
    pub counters: CacheCounters,
}

impl std::fmt::Debug for FileCache {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FileCache").field("dir", &self.dir).field("cap_bytes", &self.cap_bytes).finish()
    }
}

fn cache_key(key: &str, size: u64) -> String {
    format!("{key}@{size}")
}

fn file_name(cache_key: &str) -> String {
    let d = Sha256::digest(cache_key.as_bytes());
    let mut s = String::with_capacity(44);
    for b in &d[..20] {
        s.push_str(&format!("{b:02x}"));
    }
    s.push_str(".bin");
    s
}

fn external<E: std::error::Error + Send + Sync + 'static>(e: E) -> ParquetError {
    ParquetError::External(Box::new(e))
}

impl FileCache {
    /// `dir = None` (or `cap_bytes = 0`) disables the disk tier; footers are
    /// always cached in memory.
    pub fn new(
        store: Arc<dyn ObjectStore>,
        dir: Option<PathBuf>,
        cap_bytes: u64,
        footer_cap: u64,
    ) -> std::io::Result<Arc<Self>> {
        let dir = dir.filter(|_| cap_bytes > 0);
        let mut disk = DiskState { lru: LruCache::unbounded(), bytes: 0 };
        if let Some(d) = &dir {
            std::fs::create_dir_all(d)?;
            // Adopt what a previous process left behind (oldest first).
            let mut found: Vec<(std::time::SystemTime, String, u64)> = Vec::new();
            for entry in std::fs::read_dir(d)? {
                let entry = entry?;
                let name = entry.file_name().to_string_lossy().into_owned();
                let meta = entry.metadata()?;
                if !meta.is_file() {
                    continue;
                }
                if name.ends_with(".bin") && name.len() == 44 {
                    found.push((meta.modified().unwrap_or(std::time::UNIX_EPOCH), name, meta.len()));
                } else if name.ends_with(".tmp") {
                    let _ = std::fs::remove_file(entry.path());
                }
            }
            found.sort();
            for (_, name, size) in found {
                disk.bytes += size;
                disk.lru.put(name, size);
            }
        }
        let cache = Arc::new(Self {
            store,
            dir,
            cap_bytes,
            footer_cap: footer_cap as usize,
            disk: Mutex::new(disk),
            footers: Mutex::new(FooterState { lru: LruCache::unbounded(), bytes: 0 }),
            downloading: Mutex::new(HashSet::new()),
            download_slots: Arc::new(tokio::sync::Semaphore::new(MAX_PARALLEL_DOWNLOADS)),
            counters: CacheCounters::default(),
        });
        cache.evict();
        Ok(cache)
    }

    pub fn object_store(&self) -> &Arc<dyn ObjectStore> {
        &self.store
    }

    pub fn disk_bytes(&self) -> u64 {
        self.disk.lock().unwrap().bytes
    }

    pub fn disk_files(&self) -> usize {
        self.disk.lock().unwrap().lru.len()
    }

    pub fn footer_bytes(&self) -> usize {
        self.footers.lock().unwrap().bytes
    }

    fn evict(&self) {
        let Some(dir) = &self.dir else { return };
        let mut victims = Vec::new();
        {
            let mut st = self.disk.lock().unwrap();
            while st.bytes > self.cap_bytes {
                let Some((name, size)) = st.lru.pop_lru() else { break };
                st.bytes -= size;
                victims.push(name);
            }
        }
        for name in victims {
            // Readers that already opened the file keep their descriptor.
            let _ = std::fs::remove_file(dir.join(&name));
            self.counters.evictions.fetch_add(1, Ordering::Relaxed);
        }
    }

    /// Opens the cached copy of `key`, if present.
    fn open_local(&self, key: &str, size: u64) -> Option<Arc<std::fs::File>> {
        let dir = self.dir.as_ref()?;
        let name = file_name(&cache_key(key, size));
        if self.disk.lock().unwrap().lru.get(&name).is_none() {
            return None;
        }
        match std::fs::File::open(dir.join(&name)) {
            Ok(f) => Some(Arc::new(f)),
            Err(_) => {
                self.forget_local(&name);
                None
            }
        }
    }

    fn forget_local(&self, name: &str) {
        let mut st = self.disk.lock().unwrap();
        if let Some(size) = st.lru.pop(name) {
            st.bytes -= size;
        }
        if let Some(dir) = &self.dir {
            let _ = std::fs::remove_file(dir.join(name));
        }
    }

    /// Starts copying the whole object into the disk tier (no-op if cached,
    /// in flight, disabled, or too large to be worth caching).
    pub fn schedule_download(self: &Arc<Self>, key: &str, size: u64) {
        let Some(dir) = self.dir.clone() else { return };
        if size == 0 || size > self.cap_bytes / 2 {
            return;
        }
        let ck = cache_key(key, size);
        let name = file_name(&ck);
        if self.disk.lock().unwrap().lru.contains(&name) || !self.downloading.lock().unwrap().insert(name.clone()) {
            return;
        }
        let Ok(handle) = tokio::runtime::Handle::try_current() else {
            self.downloading.lock().unwrap().remove(&name);
            return;
        };
        let this = Arc::clone(self);
        let location = Path::from(key);
        handle.spawn(async move {
            let _permit = this.download_slots.clone().acquire_owned().await;
            let tmp = dir.join(format!("{name}.{}.tmp", std::process::id()));
            let result = this.download(&location, &tmp, size).await;
            match result {
                Ok(()) if std::fs::rename(&tmp, dir.join(&name)).is_ok() => {
                    {
                        let mut st = this.disk.lock().unwrap();
                        if st.lru.put(name.clone(), size).is_none() {
                            st.bytes += size;
                        }
                    }
                    this.counters.downloads.fetch_add(1, Ordering::Relaxed);
                    this.evict();
                }
                Ok(()) => {
                    let _ = std::fs::remove_file(&tmp);
                }
                Err(e) => {
                    tracing::debug!(key = %location, error = %e, "cache fill failed");
                    let _ = std::fs::remove_file(&tmp);
                }
            }
            this.downloading.lock().unwrap().remove(&name);
        });
    }

    async fn download(&self, location: &Path, tmp: &std::path::Path, size: u64) -> Result<(), String> {
        use tokio::io::AsyncWriteExt;
        let mut stream = self.store.get(location).await.map_err(|e| e.to_string())?.into_stream();
        let mut file = tokio::fs::File::create(tmp).await.map_err(|e| e.to_string())?;
        let mut written = 0u64;
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|e| e.to_string())?;
            written += chunk.len() as u64;
            file.write_all(&chunk).await.map_err(|e| e.to_string())?;
        }
        file.flush().await.map_err(|e| e.to_string())?;
        if written != size {
            return Err(format!("object is {written} bytes, manifest says {size}"));
        }
        Ok(())
    }

    fn footer_get(&self, ck: &str) -> Option<Arc<ParquetMetaData>> {
        let hit = self.footers.lock().unwrap().lru.get(ck).map(|(m, _)| Arc::clone(m));
        let counter = if hit.is_some() { &self.counters.footer_hits } else { &self.counters.footer_misses };
        counter.fetch_add(1, Ordering::Relaxed);
        hit
    }

    fn footer_put(&self, ck: String, meta: Arc<ParquetMetaData>) {
        let size = meta.memory_size();
        let mut st = self.footers.lock().unwrap();
        if let Some((_, old)) = st.lru.put(ck, (meta, size)) {
            st.bytes -= old;
        }
        st.bytes += size;
        while st.bytes > self.footer_cap && st.lru.len() > 1 {
            if let Some((_, (_, sz))) = st.lru.pop_lru() {
                st.bytes -= sz;
            }
        }
    }

    fn footer_drop(&self, ck: &str) {
        let mut st = self.footers.lock().unwrap();
        if let Some((_, sz)) = st.lru.pop(ck) {
            st.bytes -= sz;
        }
    }

    /// Footer with page indexes for a data file, loading it if needed.
    pub async fn metadata(self: &Arc<Self>, key: &str, size: u64) -> PqResult<Arc<ParquetMetaData>> {
        let mut reader = self.reader(key, size, None);
        reader.get_metadata(None).await
    }

    /// A reader for one object. `stats = None` skips scan accounting.
    pub fn reader(self: &Arc<Self>, key: &str, size: u64, stats: Option<Arc<QueryStats>>) -> CachedReader {
        let local = self.open_local(key, size);
        let counter = if local.is_some() { &self.counters.disk_hits } else { &self.counters.disk_misses };
        counter.fetch_add(1, Ordering::Relaxed);
        CachedReader { cache: Arc::clone(self), location: Path::from(key), key: key.to_owned(), size, local, stats }
    }
}

/// Range reader over the disk tier or, when the file is not cached, the
/// object store.
pub struct CachedReader {
    cache: Arc<FileCache>,
    location: Path,
    key: String,
    size: u64,
    local: Option<Arc<std::fs::File>>,
    stats: Option<Arc<QueryStats>>,
}

fn read_local(file: &std::fs::File, range: &Range<u64>) -> std::io::Result<Bytes> {
    use std::os::unix::fs::FileExt;
    let mut buf = vec![0u8; (range.end - range.start) as usize];
    file.read_exact_at(&mut buf, range.start)?;
    Ok(Bytes::from(buf))
}

impl CachedReader {
    async fn read(&mut self, ranges: Vec<Range<u64>>) -> PqResult<Vec<Bytes>> {
        if let Some(file) = &self.local {
            let file = Arc::clone(file);
            let total: u64 = ranges.iter().map(|r| r.end - r.start).sum();
            let read = move || ranges.iter().map(|r| read_local(&file, r)).collect::<std::io::Result<Vec<_>>>();
            // Small reads are cheaper inline than a round trip through the blocking pool.
            let out = if total <= 64 * 1024 {
                read()
            } else {
                tokio::task::spawn_blocking(read).await.map_err(external)?
            };
            return out.map_err(external);
        }
        let total: u64 = ranges.iter().map(|r| r.end - r.start).sum();
        self.cache.counters.remote_bytes.fetch_add(total, Ordering::Relaxed);
        self.cache.schedule_download(&self.key, self.size);
        self.cache.store.get_ranges(&self.location, &ranges).await.map_err(external)
    }

    async fn load_metadata(&mut self) -> PqResult<Arc<ParquetMetaData>> {
        let size = self.size;
        let fetch = FooterFetch { reader: self };
        let meta = ParquetMetaDataReader::new()
            .with_page_index_policy(PageIndexPolicy::Optional)
            .with_prefetch_hint(Some(FOOTER_HINT))
            .load_and_finish(fetch, size)
            .await?;
        Ok(Arc::new(meta))
    }
}

struct FooterFetch<'a> {
    reader: &'a mut CachedReader,
}

impl MetadataFetch for FooterFetch<'_> {
    fn fetch(&mut self, range: Range<u64>) -> BoxFuture<'_, PqResult<Bytes>> {
        async move { Ok(self.reader.read(vec![range]).await?.pop().unwrap_or_default()) }.boxed()
    }
}

impl AsyncFileReader for CachedReader {
    fn get_bytes(&mut self, range: Range<u64>) -> BoxFuture<'_, PqResult<Bytes>> {
        async move {
            if let Some(s) = &self.stats {
                s.charge(range.end - range.start)?;
            }
            Ok(self.read(vec![range]).await?.pop().unwrap_or_default())
        }
        .boxed()
    }

    fn get_byte_ranges(&mut self, ranges: Vec<Range<u64>>) -> BoxFuture<'_, PqResult<Vec<Bytes>>> {
        async move {
            if let Some(s) = &self.stats {
                s.charge(ranges.iter().map(|r| r.end - r.start).sum())?;
            }
            self.read(ranges).await
        }
        .boxed()
    }

    fn get_metadata<'a>(
        &'a mut self,
        _options: Option<&'a ArrowReaderOptions>,
    ) -> BoxFuture<'a, PqResult<Arc<ParquetMetaData>>> {
        async move {
            let ck = cache_key(&self.key, self.size);
            if let Some(m) = self.cache.footer_get(&ck) {
                return Ok(m);
            }
            let meta = match self.load_metadata().await {
                Ok(m) => m,
                Err(first) => {
                    // The manifest's size may be stale for a rewritten L0 file
                    // (see module docs): ask the store and try once more,
                    // bypassing whatever the disk tier holds.
                    let head = self.cache.store.head(&self.location).await.map_err(external)?;
                    if head.size == self.size && self.local.is_none() {
                        return Err(first);
                    }
                    if self.local.take().is_some() {
                        self.cache.forget_local(&file_name(&ck));
                    }
                    self.cache.footer_drop(&ck);
                    self.size = head.size;
                    self.load_metadata().await?
                }
            };
            self.cache.footer_put(cache_key(&self.key, self.size), Arc::clone(&meta));
            Ok(meta)
        }
        .boxed()
    }
}

/// Hands [`CachedReader`]s to DataFusion's Parquet source, bound to one
/// query's accounting.
#[derive(Debug)]
pub struct CachedReaderFactory {
    pub cache: Arc<FileCache>,
    pub stats: Arc<QueryStats>,
}

impl ParquetFileReaderFactory for CachedReaderFactory {
    fn create_reader(
        &self,
        _partition_index: usize,
        file: PartitionedFile,
        _metadata_size_hint: Option<usize>,
        _metrics: &ExecutionPlanMetricsSet,
    ) -> datafusion::common::Result<Box<dyn AsyncFileReader + Send>> {
        let key = file.object_meta.location.to_string();
        Ok(Box::new(self.cache.reader(&key, file.object_meta.size, Some(Arc::clone(&self.stats)))))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::Store;

    fn blob(n: usize, seed: u8) -> Vec<u8> {
        (0..n).map(|i| (i as u8).wrapping_mul(31).wrapping_add(seed)).collect()
    }

    async fn wait_for(cond: impl Fn() -> bool) {
        for _ in 0..500 {
            if cond() {
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
        panic!("condition not reached");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn cold_reads_use_ranges_then_the_disk_tier() {
        let store = Store::memory();
        let data = blob(100_000, 1);
        store.put("data/p=1/d=2026-01-01/a.parquet", data.clone()).await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let cache = FileCache::new(store.object_store(), Some(dir.path().into()), 1 << 20, 1 << 20).unwrap();
        let stats = QueryStats::new(0);

        let mut r = cache.reader("data/p=1/d=2026-01-01/a.parquet", data.len() as u64, Some(stats.clone()));
        assert_eq!(&r.get_bytes(10..20).await.unwrap()[..], &data[10..20]);
        assert_eq!(cache.counters.remote_bytes.load(Ordering::Relaxed), 10, "cold read fetched only the range");
        assert_eq!(stats.scanned(), 10);
        wait_for(|| cache.disk_files() == 1).await;
        assert_eq!(cache.disk_bytes(), data.len() as u64);

        // Remove the object: the cached copy must now serve the reads.
        store.delete("data/p=1/d=2026-01-01/a.parquet").await.unwrap();
        let mut r = cache.reader("data/p=1/d=2026-01-01/a.parquet", data.len() as u64, Some(stats.clone()));
        let got = r.get_byte_ranges(vec![0..5, 99_990..100_000]).await.unwrap();
        assert_eq!(&got[0][..], &data[0..5]);
        assert_eq!(&got[1][..], &data[99_990..]);
        assert_eq!(cache.counters.disk_hits.load(Ordering::Relaxed), 1);
        assert_eq!(cache.counters.remote_bytes.load(Ordering::Relaxed), 10);
        assert_eq!(stats.scanned(), 25);

        // A second process adopts the directory.
        let again = FileCache::new(store.object_store(), Some(dir.path().into()), 1 << 20, 1 << 20).unwrap();
        assert_eq!(again.disk_files(), 1);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn lru_eviction_respects_the_cap() {
        let store = Store::memory();
        let dir = tempfile::tempdir().unwrap();
        let cache = FileCache::new(store.object_store(), Some(dir.path().into()), 250_000, 1 << 20).unwrap();
        for i in 0..4u8 {
            let key = format!("f{i}");
            store.put(&key, blob(100_000, i)).await.unwrap();
            cache.reader(&key, 100_000, None).get_bytes(0..1).await.unwrap();
            let want = (i as usize + 1).min(2);
            wait_for(|| cache.disk_files() == want && cache.downloading.lock().unwrap().is_empty()).await;
            if i == 1 {
                // Touch f0 so that f1 becomes the eviction victim.
                assert!(cache.open_local("f0", 100_000).is_some());
            }
        }
        assert!(cache.disk_bytes() <= 250_000);
        assert!(cache.open_local("f3", 100_000).is_some());
        assert!(cache.open_local("f1", 100_000).is_none());
        assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 2);
        assert!(cache.counters.evictions.load(Ordering::Relaxed) >= 2);

        // Oversized objects and a disabled tier never download.
        store.put("big", blob(200_000, 9)).await.unwrap();
        cache.reader("big", 200_000, None).get_bytes(0..1).await.unwrap();
        let off = FileCache::new(store.object_store(), None, 1 << 20, 1 << 20).unwrap();
        off.reader("f0", 100_000, None).get_bytes(0..1).await.unwrap();
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        assert_eq!(cache.disk_files(), 2);
        assert_eq!(off.disk_files(), 0);
    }

    #[tokio::test]
    async fn scan_budget_is_enforced() {
        let store = Store::memory();
        store.put("f", blob(1000, 0)).await.unwrap();
        let cache = FileCache::new(store.object_store(), None, 0, 1 << 20).unwrap();
        let stats = QueryStats::new(150);
        let mut r = cache.reader("f", 1000, Some(stats.clone()));
        r.get_bytes(0..100).await.unwrap();
        let err = r.get_bytes(100..200).await.unwrap_err();
        assert!(err.to_string().contains("scan limit of 150 bytes"), "{err}");
    }
}
