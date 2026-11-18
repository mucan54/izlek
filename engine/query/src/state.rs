//! Shared service state: control-file snapshots, per-project metadata caches
//! and the per-query [`Snapshot`] every endpoint plans against.

use crate::config::Config;
use crate::error::{ApiError, ApiResult};
use crate::filecache::FileCache;
use la_core::control::{Keys, ProjectConfig, Projects};
use la_core::identity::IdentityMap;
use la_core::manifest::{self, DayManifest};
use la_core::persons::PersonTable;
use la_core::schema::ProjectSchema;
use la_core::tombstone::{self, Tombstone, TombstoneSet};
use la_core::{keys, time, Store};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

/// Largest query range (`spec/query.md` §2).
pub const MAX_RANGE_DAYS: i64 = 400;

#[derive(Debug, Default)]
struct Control {
    keys: Arc<Keys>,
    projects: Arc<Projects>,
    loaded_ok: bool,
}

struct Timed<T> {
    at: Instant,
    value: T,
}

impl<T: Clone> Timed<T> {
    fn fresh(slot: &Option<Timed<T>>, ttl: Duration) -> Option<T> {
        slot.as_ref().filter(|t| t.at.elapsed() < ttl).map(|t| t.value.clone())
    }
}

#[derive(Clone)]
struct IdentityState {
    map: Arc<IdentityMap>,
    max_link_version: u64,
}

#[derive(Clone)]
struct TombstoneState {
    list: Arc<Vec<Tombstone>>,
    digest: Arc<str>,
}

/// The person table and the identity version it was resolved with.
type PersonsState = (Arc<PersonTable>, u64);

#[derive(Default)]
struct ProjectState {
    days: Mutex<Option<Timed<Arc<Vec<String>>>>>,
    manifests: Mutex<HashMap<String, Timed<Arc<DayManifest>>>>,
    schema: Mutex<Option<Timed<Arc<ProjectSchema>>>>,
    tombstones: Mutex<Option<Timed<TombstoneState>>>,
    identity: tokio::sync::Mutex<Option<Timed<IdentityState>>>,
    persons: tokio::sync::Mutex<Option<Timed<PersonsState>>>,
    last_used: Mutex<Option<Instant>>,
}

pub struct Engine {
    pub store: Store,
    pub cfg: Config,
    pub files: Arc<FileCache>,
    control: RwLock<Control>,
    projects: Mutex<HashMap<u32, Arc<ProjectState>>>,
}

/// Everything a query needs to know about its project, frozen at plan time.
#[derive(Debug, Clone)]
pub struct Snapshot {
    pub project: u32,
    /// Inclusive start, unix ms.
    pub from_ms: i64,
    /// Exclusive end, unix ms.
    pub to_ms: i64,
    /// Day manifests with at least one file, ascending by day.
    pub days: Vec<Arc<DayManifest>>,
    pub identity: Arc<IdentityMap>,
    /// Highest link version in `identity`: files at or above it need no rewrite.
    pub max_link_version: u64,
    pub persons: Option<Arc<PersonTable>>,
    pub tombstones: Arc<Vec<Tombstone>>,
    pub tombstone_digest: Arc<str>,
    pub schema: Arc<ProjectSchema>,
    pub config: ProjectConfig,
}

impl Snapshot {
    /// `day → manifest version` for `Meta.manifests`.
    pub fn manifest_versions(&self) -> BTreeMap<String, u64> {
        self.days.iter().map(|m| (m.day.clone(), m.version)).collect()
    }

    /// The version part of the result-cache key (`spec/query.md` §9).
    pub fn version_fingerprint(&self) -> String {
        let mut s = String::new();
        for m in &self.days {
            s.push_str(&format!("{}={};", m.day, m.version));
        }
        s.push_str(&format!("id={};", self.identity.version()));
        if let Some(p) = &self.persons {
            s.push_str(&format!("persons={};", p.version()));
        }
        s.push_str(&format!("schema={};", self.schema.version));
        s.push_str(&format!("tomb={}", self.tombstone_digest));
        s
    }

    pub fn tombstone_set(&self, after_ts: i64) -> TombstoneSet {
        TombstoneSet::build(&self.tombstones, &self.identity, after_ts)
    }

    pub fn persons(&self) -> Arc<PersonTable> {
        self.persons.clone().unwrap_or_default()
    }
}

fn digest_tombstones(list: &[Tombstone]) -> String {
    let mut h = Sha256::new();
    for t in list {
        h.update(t.ts_ms.to_le_bytes());
        h.update(t.id.as_bytes());
        h.update([0]);
        for k in &t.person_keys {
            h.update(k.as_bytes());
            h.update([1]);
        }
        for a in &t.anon_ids {
            h.update(a.as_bytes());
            h.update([2]);
        }
    }
    h.finalize().iter().take(12).map(|b| format!("{b:02x}")).collect()
}

impl Engine {
    pub fn new(store: Store, cfg: Config) -> Result<Arc<Self>, String> {
        let files =
            FileCache::new(store.object_store(), cfg.cache_dir.clone(), cfg.cache_bytes, cfg.footer_cache_bytes)
                .map_err(|e| format!("cache dir: {e}"))?;
        Ok(Arc::new(Self {
            store,
            cfg,
            files,
            control: RwLock::new(Control::default()),
            projects: Mutex::new(HashMap::new()),
        }))
    }

    /// Reloads `control/keys.json` and `control/projects.json`. On failure the
    /// previous (last good) copy stays in effect.
    pub async fn refresh_control(&self) -> Result<(), la_core::Error> {
        let keys = Keys::load(&self.store).await;
        let projects = Projects::load(&self.store).await;
        let mut c = self.control.write().unwrap();
        let mut first_err = None;
        match keys {
            Ok(k) => c.keys = Arc::new(k),
            Err(e) => first_err = Some(e),
        }
        match projects {
            Ok(p) => c.projects = Arc::new(p),
            Err(e) => first_err = first_err.or(Some(e)),
        }
        match first_err {
            None => {
                c.loaded_ok = true;
                Ok(())
            }
            Some(e) => Err(e),
        }
    }

    /// True once the control files have been read successfully at least once.
    pub fn ready(&self) -> bool {
        self.control.read().unwrap().loaded_ok
    }

    pub fn keys(&self) -> Arc<Keys> {
        self.control.read().unwrap().keys.clone()
    }

    pub fn project_config(&self, project: u32) -> ProjectConfig {
        let c = self.control.read().unwrap();
        match c.projects.get(project) {
            Some(p) => p.clone(),
            None => ProjectConfig { max_concurrent_queries: self.cfg.max_concurrent, ..Default::default() },
        }
    }

    fn project(&self, project: u32) -> Arc<ProjectState> {
        let st = self.projects.lock().unwrap().entry(project).or_default().clone();
        *st.last_used.lock().unwrap() = Some(Instant::now());
        st
    }

    /// Background refresh: control files, plus the tombstones of recently
    /// queried projects so deletions apply within the TTL without a query
    /// having to pay for the listing.
    pub async fn refresh_tick(&self) {
        if let Err(e) = self.refresh_control().await {
            tracing::warn!(error = %e, "control refresh failed; keeping last good copy");
        }
        let active: Vec<(u32, Arc<ProjectState>)> = self
            .projects
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, st)| st.last_used.lock().unwrap().is_some_and(|t| t.elapsed() < Duration::from_secs(600)))
            .map(|(p, st)| (*p, st.clone()))
            .collect();
        for (project, st) in active {
            let stale = Timed::fresh(&st.tombstones.lock().unwrap(), self.cfg.tombstone_ttl / 2).is_none();
            if stale {
                let _ = self.load_tombstones(project, &st, true).await;
            }
        }
    }

    pub fn spawn_refresher(self: &Arc<Self>) -> tokio::task::JoinHandle<()> {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(this.cfg.control_refresh);
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tick.tick().await;
                this.refresh_tick().await;
            }
        })
    }

    async fn load_tombstones(&self, project: u32, st: &ProjectState, force: bool) -> ApiResult<TombstoneState> {
        if !force {
            if let Some(t) = Timed::fresh(&st.tombstones.lock().unwrap(), self.cfg.tombstone_ttl) {
                return Ok(t);
            }
        }
        match tombstone::load_all(&self.store, project).await {
            Ok(list) => {
                let state = TombstoneState { digest: digest_tombstones(&list).into(), list: Arc::new(list) };
                *st.tombstones.lock().unwrap() = Some(Timed { at: Instant::now(), value: state.clone() });
                Ok(state)
            }
            Err(e) => {
                // Last good copy beats failing every query while the store is down.
                let last = st.tombstones.lock().unwrap().as_ref().map(|t| t.value.clone());
                match last {
                    Some(t) => {
                        tracing::warn!(project, error = %e, "tombstone refresh failed; using last good list");
                        Ok(t)
                    }
                    None => Err(e.into()),
                }
            }
        }
    }

    async fn load_days(&self, project: u32, st: &ProjectState) -> ApiResult<Arc<Vec<String>>> {
        if let Some(d) = Timed::fresh(&st.days.lock().unwrap(), self.cfg.manifest_ttl) {
            return Ok(d);
        }
        let days = Arc::new(manifest::list_days(&self.store, project).await?);
        *st.days.lock().unwrap() = Some(Timed { at: Instant::now(), value: days.clone() });
        Ok(days)
    }

    async fn load_manifest(&self, project: u32, st: &ProjectState, day: &str) -> ApiResult<Arc<DayManifest>> {
        if let Some(t) = st.manifests.lock().unwrap().get(day).filter(|t| t.at.elapsed() < self.cfg.manifest_ttl) {
            return Ok(t.value.clone());
        }
        let m = Arc::new(manifest::load_day(&self.store, project, day).await?);
        st.manifests.lock().unwrap().insert(day.to_owned(), Timed { at: Instant::now(), value: m.clone() });
        Ok(m)
    }

    async fn load_schema(&self, project: u32, st: &ProjectState) -> ApiResult<Arc<ProjectSchema>> {
        if let Some(s) = Timed::fresh(&st.schema.lock().unwrap(), self.cfg.manifest_ttl) {
            return Ok(s);
        }
        let s = Arc::new(ProjectSchema::load(&self.store, project).await?);
        *st.schema.lock().unwrap() = Some(Timed { at: Instant::now(), value: s.clone() });
        Ok(s)
    }

    /// Identity map at least as new as `min_version` (the highest
    /// `identity_version` any data file in the query claims).
    async fn load_identity(&self, project: u32, st: &ProjectState, min_version: u64) -> ApiResult<IdentityState> {
        let mut slot = st.identity.lock().await;
        if let Some(t) = slot.as_ref() {
            if t.at.elapsed() < self.cfg.manifest_ttl && t.value.map.version() >= min_version {
                return Ok(t.value.clone());
            }
        }
        let mut map = slot.as_ref().map(|t| t.value.map.clone()).unwrap_or_default();
        let before = map.version();
        let latest = manifest::latest_version(&self.store, &keys::identity_manifest_dir(project)).await?;
        let mut max_link_version = slot.as_ref().map(|t| t.value.max_link_version).unwrap_or(0);
        if latest != before || slot.is_none() {
            // Copy-on-write: queries in flight keep the map they planned with.
            Arc::make_mut(&mut map).refresh(&self.store, project).await?;
            max_link_version = map.iter().map(|l| l.version).max().unwrap_or(0);
        }
        let state = IdentityState { map, max_link_version };
        *slot = Some(Timed { at: Instant::now(), value: state.clone() });
        Ok(state)
    }

    async fn load_persons(
        &self,
        project: u32,
        st: &ProjectState,
        identity: &Arc<IdentityMap>,
    ) -> ApiResult<Arc<PersonTable>> {
        let mut slot = st.persons.lock().await;
        if let Some(t) = slot.as_ref() {
            if t.at.elapsed() < self.cfg.manifest_ttl && t.value.1 == identity.version() {
                return Ok(t.value.0.clone());
            }
        }
        let latest = manifest::latest_version(&self.store, &keys::persons_manifest_dir(project)).await?;
        let table = match slot.as_ref() {
            Some(t) if t.value.0.version() == latest && t.value.1 == identity.version() => t.value.0.clone(),
            _ => Arc::new(PersonTable::load(&self.store, project, identity).await?),
        };
        *slot = Some(Timed { at: Instant::now(), value: (table.clone(), identity.version()) });
        Ok(table)
    }

    /// Builds the snapshot for `[from_ms, to_ms)`.
    pub async fn snapshot(&self, project: u32, from_ms: i64, to_ms: i64, need_persons: bool) -> ApiResult<Snapshot> {
        if from_ms >= to_ms {
            return Err(ApiError::bad_request("`from` must be before `to`"));
        }
        if to_ms - from_ms > MAX_RANGE_DAYS * time::DAY_MS {
            return Err(ApiError::bad_request(format!("time range exceeds {MAX_RANGE_DAYS} days")));
        }
        let st = self.project(project);
        let existing = self.load_days(project, &st).await?;
        let (first, last) = (time::day_of(from_ms), time::day_of(to_ms - 1));
        let wanted: Vec<&String> =
            existing.iter().filter(|d| d.as_str() >= first.as_str() && d.as_str() <= last.as_str()).collect();
        let mut days = Vec::with_capacity(wanted.len());
        for chunk in wanted.chunks(16) {
            let loaded = futures::future::join_all(chunk.iter().map(|d| self.load_manifest(project, &st, d))).await;
            for m in loaded {
                let m = m?;
                if !m.files.is_empty() {
                    days.push(m);
                }
            }
        }
        let min_identity = days.iter().flat_map(|m| m.files.iter().map(|f| f.identity_version)).max().unwrap_or(0);
        let identity = self.load_identity(project, &st, min_identity).await?;
        let persons = if need_persons { Some(self.load_persons(project, &st, &identity.map).await?) } else { None };
        let tombs = self.load_tombstones(project, &st, false).await?;
        let schema = self.load_schema(project, &st).await?;
        Ok(Snapshot {
            project,
            from_ms,
            to_ms,
            days,
            identity: identity.map,
            max_link_version: identity.max_link_version,
            persons,
            tombstones: tombs.list,
            tombstone_digest: tombs.digest,
            schema,
            config: self.project_config(project),
        })
    }
}

/// Parses `from` / `to` (`spec/query.md` §2) into `[from_ms, to_ms)`.
/// A `YYYY-MM-DD` `to` is inclusive; an RFC 3339 `to` is exclusive.
pub fn parse_range(from: &str, to: &str) -> ApiResult<(i64, i64)> {
    fn instant(s: &str, end_of_day: bool, field: &str) -> ApiResult<i64> {
        if let Some(ms) = time::day_start_ms(s) {
            return Ok(if end_of_day { ms + time::DAY_MS } else { ms });
        }
        chrono::DateTime::parse_from_rfc3339(s)
            .map(|d| d.timestamp_millis())
            .map_err(|_| ApiError::bad_request(format!("`{field}` must be YYYY-MM-DD or an RFC 3339 timestamp")))
    }
    let from_ms = instant(from, false, "from")?;
    let to_ms = instant(to, true, "to")?;
    if from_ms >= to_ms {
        return Err(ApiError::bad_request("`from` must be before `to`"));
    }
    if to_ms - from_ms > MAX_RANGE_DAYS * time::DAY_MS {
        return Err(ApiError::bad_request(format!("time range exceeds {MAX_RANGE_DAYS} days")));
    }
    Ok((from_ms, to_ms))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ranges() {
        let d = time::day_start_ms("2026-11-01").unwrap();
        assert_eq!(parse_range("2026-11-01", "2026-11-01").unwrap(), (d, d + time::DAY_MS));
        assert_eq!(parse_range("2026-11-01T00:00:00Z", "2026-11-01T00:00:01Z").unwrap(), (d, d + 1000));
        assert_eq!(parse_range("2026-11-01T03:00:00+03:00", "2026-11-01").unwrap(), (d, d + time::DAY_MS));
        assert!(parse_range("2026-11-02", "2026-11-01").is_err());
        assert!(parse_range("2026-11-01T00:00:00Z", "2026-11-01T00:00:00Z").is_err());
        assert!(parse_range("yesterday", "2026-11-01").is_err());
        assert!(parse_range("2026-11-01", "../../etc").is_err());
        assert!(parse_range("2025-01-01", "2026-02-04").is_ok());
        assert!(parse_range("2025-01-01", "2026-02-05").is_err(), "401 days");
    }
}
