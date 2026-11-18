//! Object storage access (`spec/manifest.md` §0).
//!
//! Only five operations are needed anywhere in the engine: get, put,
//! create-if-absent, delete and list-by-prefix. All coordination between
//! components is built on create-if-absent.

use crate::error::{Error, Result};
use bytes::Bytes;
use futures::TryStreamExt;
use object_store::aws::AmazonS3Builder;
use object_store::local::LocalFileSystem;
use object_store::memory::InMemory;
use object_store::path::Path;
use object_store::prefix::PrefixStore;
use object_store::{ObjectStore, ObjectStoreExt, PutMode, PutOptions, PutPayload};
use std::sync::Arc;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObjInfo {
    pub key: String,
    pub size: u64,
    pub last_modified_ms: i64,
}

#[derive(Debug, Clone, Default)]
pub struct S3Options {
    pub endpoint: Option<String>,
    pub region: Option<String>,
    pub access_key: Option<String>,
    pub secret_key: Option<String>,
}

impl S3Options {
    pub fn from_env() -> Self {
        let get = |k: &str| std::env::var(k).ok().filter(|v| !v.is_empty());
        Self {
            endpoint: get("LA_S3_ENDPOINT"),
            region: get("LA_S3_REGION"),
            access_key: get("LA_S3_ACCESS_KEY"),
            secret_key: get("LA_S3_SECRET_KEY"),
        }
    }
}

/// Cheaply clonable handle to the platform's bucket (or directory).
#[derive(Debug, Clone)]
pub struct Store {
    inner: Arc<dyn ObjectStore>,
    desc: Arc<str>,
}

impl Store {
    /// `LA_STORE_URL` (`s3://bucket[/prefix]` or `file:///abs/dir`), falling
    /// back to `s3://$LA_S3_BUCKET`.
    pub fn from_env() -> Result<Self> {
        let url = match std::env::var("LA_STORE_URL").ok().filter(|v| !v.is_empty()) {
            Some(u) => u,
            None => match std::env::var("LA_S3_BUCKET").ok().filter(|v| !v.is_empty()) {
                Some(b) => format!("s3://{b}"),
                None => return Err(Error::Config("set LA_STORE_URL or LA_S3_BUCKET".into())),
            },
        };
        Self::from_url(&url, S3Options::from_env())
    }

    pub fn from_url(url: &str, s3: S3Options) -> Result<Self> {
        let parsed = url::Url::parse(url).map_err(|e| Error::Config(format!("LA_STORE_URL {url:?}: {e}")))?;
        match parsed.scheme() {
            "file" => {
                let path = parsed.to_file_path().map_err(|_| Error::Config(format!("bad file url {url:?}")))?;
                Self::local(path)
            }
            "s3" => {
                let bucket = parsed.host_str().ok_or_else(|| Error::Config(format!("no bucket in {url:?}")))?;
                let mut b = AmazonS3Builder::new()
                    .with_bucket_name(bucket)
                    .with_region(s3.region.as_deref().unwrap_or("us-east-1"))
                    .with_virtual_hosted_style_request(false);
                if let Some(ep) = &s3.endpoint {
                    b = b.with_endpoint(ep).with_allow_http(ep.starts_with("http://"));
                }
                if let (Some(a), Some(s)) = (&s3.access_key, &s3.secret_key) {
                    b = b.with_access_key_id(a).with_secret_access_key(s);
                }
                let s3store = b.build()?;
                let prefix = parsed.path().trim_matches('/');
                let inner: Arc<dyn ObjectStore> = if prefix.is_empty() {
                    Arc::new(s3store)
                } else {
                    Arc::new(PrefixStore::new(s3store, Path::from(prefix)))
                };
                Ok(Self { inner, desc: url.into() })
            }
            other => Err(Error::Config(format!("unsupported store scheme {other:?}"))),
        }
    }

    /// Directory-backed store; the object key is the relative path under `dir`.
    pub fn local(dir: impl AsRef<std::path::Path>) -> Result<Self> {
        let dir = dir.as_ref();
        std::fs::create_dir_all(dir)?;
        let fs = LocalFileSystem::new_with_prefix(dir)?.with_automatic_cleanup(false);
        Ok(Self { inner: Arc::new(fs), desc: format!("file://{}", dir.display()).into() })
    }

    pub fn memory() -> Self {
        Self { inner: Arc::new(InMemory::new()), desc: "memory://".into() }
    }

    pub fn from_object_store(inner: Arc<dyn ObjectStore>, desc: &str) -> Self {
        Self { inner, desc: desc.into() }
    }

    /// The underlying store, for readers that need range requests (Parquet).
    pub fn object_store(&self) -> Arc<dyn ObjectStore> {
        self.inner.clone()
    }

    pub fn describe(&self) -> &str {
        &self.desc
    }

    /// `None` when the object does not exist.
    pub async fn get(&self, key: &str) -> Result<Option<Bytes>> {
        match self.inner.get(&Path::from(key)).await {
            Ok(r) => Ok(Some(r.bytes().await?)),
            Err(object_store::Error::NotFound { .. }) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    /// Unconditional write (overwrites).
    pub async fn put(&self, key: &str, data: impl Into<Bytes>) -> Result<()> {
        self.inner.put(&Path::from(key), PutPayload::from(data.into())).await?;
        Ok(())
    }

    /// Create-if-absent. Returns `false` when the object already existed (and
    /// was left untouched).
    pub async fn put_if_absent(&self, key: &str, data: impl Into<Bytes>) -> Result<bool> {
        let opts = PutOptions { mode: PutMode::Create, ..Default::default() };
        match self.inner.put_opts(&Path::from(key), PutPayload::from(data.into()), opts).await {
            Ok(_) => Ok(true),
            Err(object_store::Error::AlreadyExists { .. }) => Ok(false),
            // Some S3-compatible stores answer 412 instead of 409 for If-None-Match: *.
            Err(object_store::Error::Precondition { .. }) => Ok(false),
            Err(e) => Err(e.into()),
        }
    }

    /// Deleting a missing object is not an error.
    pub async fn delete(&self, key: &str) -> Result<()> {
        match self.inner.delete(&Path::from(key)).await {
            Ok(()) | Err(object_store::Error::NotFound { .. }) => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    pub async fn exists(&self, key: &str) -> Result<bool> {
        match self.inner.head(&Path::from(key)).await {
            Ok(_) => Ok(true),
            Err(object_store::Error::NotFound { .. }) => Ok(false),
            Err(e) => Err(e.into()),
        }
    }

    /// Lists every object under `prefix`, sorted by key.
    ///
    /// `prefix` is matched on whole path segments: `"wal/dt=2026-11-18"`
    /// lists that directory, `"wal/dt=2026"` matches nothing.
    pub async fn list(&self, prefix: &str) -> Result<Vec<ObjInfo>> {
        let prefix = prefix.trim_matches('/');
        let p = Path::from(prefix);
        let stream = self.inner.list(if prefix.is_empty() { None } else { Some(&p) });
        let mut out: Vec<ObjInfo> = match stream.try_collect::<Vec<_>>().await {
            Ok(v) => v,
            // A missing directory on the local backend is an empty listing.
            Err(object_store::Error::NotFound { .. }) => Vec::new(),
            Err(e) => return Err(e.into()),
        }
        .into_iter()
        .map(|m| ObjInfo {
            key: m.location.to_string(),
            size: m.size,
            last_modified_ms: m.last_modified.timestamp_millis(),
        })
        // Writers on the file backend may leave short-lived temp files around.
        .filter(|o| !is_temp_name(&o.key))
        .collect();
        out.sort_by(|a, b| a.key.cmp(&b.key));
        Ok(out)
    }

    /// Immediate child "directories" of `prefix` (names only, sorted).
    pub async fn list_dirs(&self, prefix: &str) -> Result<Vec<String>> {
        let prefix = prefix.trim_matches('/');
        let p = Path::from(prefix);
        let res = match self.inner.list_with_delimiter(if prefix.is_empty() { None } else { Some(&p) }).await {
            Ok(r) => r,
            Err(object_store::Error::NotFound { .. }) => return Ok(Vec::new()),
            Err(e) => return Err(e.into()),
        };
        let mut out: Vec<String> = res.common_prefixes.iter().filter_map(|c| c.filename().map(str::to_owned)).collect();
        out.sort();
        Ok(out)
    }

    pub async fn get_json<T: serde::de::DeserializeOwned>(&self, key: &str) -> Result<Option<T>> {
        match self.get(key).await? {
            None => Ok(None),
            Some(b) => {
                serde_json::from_slice(&b).map(Some).map_err(|source| Error::Json { key: key.to_owned(), source })
            }
        }
    }

    pub async fn put_json<T: serde::Serialize>(&self, key: &str, value: &T) -> Result<()> {
        self.put(key, to_json(key, value)?).await
    }

    pub async fn put_json_if_absent<T: serde::Serialize>(&self, key: &str, value: &T) -> Result<bool> {
        self.put_if_absent(key, to_json(key, value)?).await
    }
}

fn to_json<T: serde::Serialize>(key: &str, value: &T) -> Result<Vec<u8>> {
    serde_json::to_vec(value).map_err(|source| Error::Json { key: key.to_owned(), source })
}

/// Temp-file conventions of the file backends (Rust `object_store` uses
/// `name#<n>`, the Go client uses a `.tmp-` prefix).
fn is_temp_name(key: &str) -> bool {
    let name = key.rsplit('/').next().unwrap_or(key);
    name.starts_with(".tmp-") || name.rsplit_once('#').is_some_and(|(_, n)| n.bytes().all(|b| b.is_ascii_digit()))
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn exercise(store: Store) {
        assert_eq!(store.get("a/b.json").await.unwrap(), None);
        assert!(!store.exists("a/b.json").await.unwrap());
        assert!(store.put_if_absent("a/b.json", &b"one"[..]).await.unwrap());
        assert!(!store.put_if_absent("a/b.json", &b"two"[..]).await.unwrap());
        assert_eq!(store.get("a/b.json").await.unwrap().unwrap(), &b"one"[..]);
        store.put("a/b.json", &b"three"[..]).await.unwrap();
        assert_eq!(store.get("a/b.json").await.unwrap().unwrap(), &b"three"[..]);
        store.put("a/c=1/d.bin", &b"x"[..]).await.unwrap();
        store.put("a/c=2/e.bin", &b"yy"[..]).await.unwrap();
        store.put("ab/z", &b""[..]).await.unwrap();
        let keys: Vec<_> = store.list("a").await.unwrap().into_iter().map(|o| (o.key, o.size)).collect();
        assert_eq!(keys, [("a/b.json".into(), 5), ("a/c=1/d.bin".into(), 1), ("a/c=2/e.bin".into(), 2)]);
        assert_eq!(store.list_dirs("a").await.unwrap(), ["c=1", "c=2"]);
        assert!(store.list("missing/dir").await.unwrap().is_empty());
        assert!(store.list_dirs("missing/dir").await.unwrap().is_empty());
        store.delete("a/b.json").await.unwrap();
        store.delete("a/b.json").await.unwrap();
        assert!(!store.exists("a/b.json").await.unwrap());
    }

    #[tokio::test]
    async fn memory_backend() {
        exercise(Store::memory()).await;
    }

    #[tokio::test]
    async fn local_backend() {
        let dir = tempfile::tempdir().unwrap();
        exercise(Store::local(dir.path()).unwrap()).await;
        // from_url resolves to the same tree
        let s = Store::from_url(&format!("file://{}", dir.path().display()), S3Options::default()).unwrap();
        assert!(s.exists("ab/z").await.unwrap());
    }

    #[tokio::test]
    async fn create_if_absent_has_exactly_one_winner() {
        let dir = tempfile::tempdir().unwrap();
        for store in [Store::memory(), Store::local(dir.path()).unwrap()] {
            let mut tasks = Vec::new();
            for i in 0..32u8 {
                let s = store.clone();
                tasks.push(tokio::spawn(async move { s.put_if_absent("m/00000001.json", vec![i]).await.unwrap() }));
            }
            let mut winners = 0;
            for t in tasks {
                winners += t.await.unwrap() as u32;
            }
            assert_eq!(winners, 1, "{}", store.describe());
            assert_eq!(store.get("m/00000001.json").await.unwrap().unwrap().len(), 1);
        }
    }

    #[test]
    fn temp_names() {
        assert!(is_temp_name("a/.tmp-123"));
        assert!(is_temp_name("a/file.parquet#12"));
        assert!(!is_temp_name("a/file.parquet"));
        assert!(!is_temp_name("a/b#c"));
    }
}
