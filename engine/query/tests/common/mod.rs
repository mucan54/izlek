//! Shared fixtures: writes data files, manifests and side tables exactly the
//! way the shipper/compactor do (through `la_core`).
#![allow(dead_code)]

use la_core::control::sha256_hex;
use la_core::events::{sort_and_dedupe, Cell, EventRow};
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::manifest::commit_day;
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::persons::{self, PropWrite};
use la_core::schema::{ProjectSchema, PromotedColumn, PropType, DIM_COUNTRY, DIM_PLATFORM};
use la_core::tombstone::{tombstone_key, Tombstone};
use la_core::{keys, time, Store};
use la_query::config::Config;
use la_query::state::Engine;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

pub const DAY1: &str = "2026-11-01";
pub const DAY2: &str = "2026-11-02";
pub const DAY3: &str = "2026-11-03";

pub fn day_ms(day: &str) -> i64 {
    time::day_start_ms(day).unwrap()
}

pub fn uuid(n: u64) -> [u8; 16] {
    let mut u = [0u8; 16];
    u[8..].copy_from_slice(&n.to_be_bytes());
    u
}

static NEXT_UUID: AtomicU64 = AtomicU64::new(1);

/// Builder for one event row. `person_key` is derived from the ids unless
/// overridden (to simulate what the shipper knew at write time).
#[derive(Clone)]
pub struct Ev(pub EventRow);

pub fn ev(anon: &str, user: Option<&str>, ts: i64, event: &str) -> Ev {
    let n = NEXT_UUID.fetch_add(1, Ordering::Relaxed);
    Ev(EventRow {
        uuid: uuid(n),
        ts,
        recv_ts: ts + 5,
        event: event.to_owned(),
        person_key: la_core::events::person_key(user, anon),
        anon_id: anon.to_owned(),
        user_id: user.map(str::to_owned),
        props: "{}".to_owned(),
        ..Default::default()
    })
}

impl Ev {
    pub fn uuid(mut self, n: u64) -> Self {
        self.0.uuid = uuid(n);
        self
    }
    pub fn key(mut self, person_key: &str) -> Self {
        self.0.person_key = person_key.to_owned();
        self
    }
    pub fn props(mut self, json: &str) -> Self {
        self.0.props = json.to_owned();
        self
    }
    pub fn platform(mut self, p: &str) -> Self {
        self.0.dims[DIM_PLATFORM] = Some(p.to_owned());
        self
    }
    pub fn country(mut self, c: &str) -> Self {
        self.0.dims[DIM_COUNTRY] = Some(c.to_owned());
        self
    }
    pub fn session(mut self, n: u64) -> Self {
        self.0.session_id = Some(uuid(n));
        self
    }
    pub fn promoted(mut self, cells: Vec<Option<Cell>>) -> Self {
        self.0.promoted = cells;
        self
    }
}

pub fn col(key: &str, ty: PropType, since: u64) -> PromotedColumn {
    PromotedColumn { key: key.into(), ty, since }
}

pub struct FileSpec<'a> {
    pub project: u32,
    pub day: &'a str,
    pub level: u8,
    pub name: &'a str,
    pub identity_version: u64,
    pub schema_version: u64,
    pub promoted: &'a [PromotedColumn],
}

impl<'a> FileSpec<'a> {
    pub fn l0(project: u32, day: &'a str, name: &'a str) -> Self {
        Self { project, day, level: 0, name, identity_version: 0, schema_version: 0, promoted: &[] }
    }
    pub fn identity(mut self, v: u64) -> Self {
        self.identity_version = v;
        self
    }
    pub fn level(mut self, l: u8) -> Self {
        self.level = l;
        self
    }
    pub fn schema(mut self, v: u64, promoted: &'a [PromotedColumn]) -> Self {
        self.schema_version = v;
        self.promoted = promoted;
        self
    }
}

/// Writes one data file and appends it to the day manifest.
pub async fn add_file(store: &Store, spec: FileSpec<'_>, events: Vec<Ev>) -> String {
    let mut rows: Vec<EventRow> = events.into_iter().map(|e| e.0).collect();
    sort_and_dedupe(&mut rows);
    let meta = EventsFileMeta {
        project: spec.project,
        day: spec.day.to_owned(),
        level: spec.level,
        schema_version: spec.schema_version,
        identity_version: spec.identity_version,
    };
    let (bytes, mut entry) = write_events_file(&rows, spec.promoted, &meta).unwrap();
    let key = format!("{}/L{}-{}.parquet", keys::data_day_prefix(spec.project, spec.day), spec.level, spec.name);
    entry.key = key.clone();
    store.put(&key, bytes).await.unwrap();
    commit_day(store, spec.project, spec.day, |m| {
        m.files.push(entry.clone());
        Ok(true)
    })
    .await
    .unwrap();
    key
}

/// Replaces a day's files with one compacted file (new manifest version).
pub async fn replace_day(store: &Store, spec: FileSpec<'_>, events: Vec<Ev>, tombstone_ts: i64) {
    let mut rows: Vec<EventRow> = events.into_iter().map(|e| e.0).collect();
    sort_and_dedupe(&mut rows);
    let meta = EventsFileMeta {
        project: spec.project,
        day: spec.day.to_owned(),
        level: spec.level,
        schema_version: spec.schema_version,
        identity_version: spec.identity_version,
    };
    let (bytes, mut entry) = write_events_file(&rows, spec.promoted, &meta).unwrap();
    let key = format!("{}/L{}-{}.parquet", keys::data_day_prefix(spec.project, spec.day), spec.level, spec.name);
    entry.key = key.clone();
    store.put(&key, bytes).await.unwrap();
    commit_day(store, spec.project, spec.day, |m| {
        m.files = vec![entry.clone()];
        m.tombstone_ts = tombstone_ts;
        Ok(true)
    })
    .await
    .unwrap();
}

/// Adds identity links; returns the identity manifest version afterwards.
pub async fn link(store: &Store, project: u32, plan: u64, links: &[(&str, &str)]) -> u64 {
    let mut map = IdentityMap::load(store, project).await.unwrap();
    let candidates = links
        .iter()
        .enumerate()
        .map(|(i, (a, u))| Candidate {
            anon_id: a.to_string(),
            user_id: u.to_string(),
            ts: i as i64,
            uuid: uuid(i as u64),
        })
        .collect();
    identity::append_delta(store, project, "1-0", plan, candidates, &mut map).await.unwrap().version
}

pub async fn set_person(store: &Store, project: u32, plan: u64, writes: &[(&str, &str, &str, i64)]) {
    let writes = writes
        .iter()
        .map(|(pk, k, v, ts)| PropWrite {
            person_key: pk.to_string(),
            key: k.to_string(),
            value: v.to_string(),
            ts: *ts,
            seq: 0,
        })
        .collect();
    persons::append_delta(store, project, "1-0", plan, writes).await.unwrap();
}

pub async fn tombstone(store: &Store, project: u32, ts_ms: i64, person_keys: &[&str], anon_ids: &[&str]) {
    let t = Tombstone {
        id: format!("del_{ts_ms}"),
        ts_ms,
        person_keys: person_keys.iter().map(|s| s.to_string()).collect(),
        anon_ids: anon_ids.iter().map(|s| s.to_string()).collect(),
    };
    store.put_json(&tombstone_key(project, &t), &t).await.unwrap();
}

pub async fn save_schema(store: &Store, project: u32, version: u64, columns: &[PromotedColumn]) {
    let s = ProjectSchema { version, project, columns: columns.to_vec(), ..Default::default() };
    s.save(store).await.unwrap();
}

/// Registers `sk` as a query key of `project`.
pub async fn add_key(store: &Store, sk: &str, project: u32, scopes: &[&str]) {
    let mut k: la_core::control::Keys = la_core::control::Keys::load(store).await.unwrap();
    k.version += 1;
    k.keys.insert(
        sha256_hex(sk),
        la_core::control::QueryKey { project_id: project, scopes: scopes.iter().map(|s| s.to_string()).collect() },
    );
    store.put_json(keys::KEYS, &k).await.unwrap();
}

/// Test configuration: no metadata caching so every query sees the store's
/// current state.
pub fn test_config() -> Config {
    Config {
        manifest_ttl: Duration::ZERO,
        tombstone_ttl: Duration::ZERO,
        partitions: 2,
        memory_bytes: 256 << 20,
        ..Config::default()
    }
}

pub async fn engine(store: &Store) -> Arc<Engine> {
    engine_with(store, test_config()).await
}

pub async fn engine_with(store: &Store, cfg: Config) -> Arc<Engine> {
    let e = Engine::new(store.clone(), cfg).unwrap();
    e.refresh_control().await.unwrap();
    e
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

use axum::body::Body;
use axum::http::{Request, StatusCode};
use http_body_util::BodyExt;
use la_query::service::Service;
use serde_json::Value;
use tower::ServiceExt;

pub struct App {
    pub svc: Arc<Service>,
    pub router: axum::Router,
}

pub async fn app(store: &Store) -> App {
    app_with(store, test_config()).await
}

pub async fn app_with(store: &Store, cfg: Config) -> App {
    let engine = engine_with(store, cfg).await;
    let svc = Service::new(engine);
    App { router: la_query::http::router(Arc::clone(&svc)), svc }
}

pub struct Reply {
    pub status: StatusCode,
    pub headers: axum::http::HeaderMap,
    pub bytes: Vec<u8>,
}

impl Reply {
    pub fn json(&self) -> Value {
        serde_json::from_slice(&self.bytes)
            .unwrap_or_else(|e| panic!("not JSON ({e}): {:?}", String::from_utf8_lossy(&self.bytes)))
    }
    pub fn code(&self) -> String {
        self.json()["error"]["code"].as_str().unwrap_or_default().to_owned()
    }
}

impl App {
    pub async fn send(&self, req: Request<Body>) -> Reply {
        let resp = self.router.clone().oneshot(req).await.unwrap();
        let status = resp.status();
        let headers = resp.headers().clone();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes().to_vec();
        Reply { status, headers, bytes }
    }

    /// `POST path` with a bearer key and a JSON body.
    pub async fn post(&self, key: &str, path: &str, body: Value) -> Reply {
        let req = Request::post(path)
            .header("authorization", format!("Bearer {key}"))
            .header("content-type", "application/json")
            .body(Body::from(body.to_string()))
            .unwrap();
        self.send(req).await
    }

    /// Like [`post`] but asserts `200` and returns the body.
    pub async fn ok(&self, key: &str, path: &str, body: Value) -> Value {
        let r = self.post(key, path, body).await;
        assert_eq!(r.status, StatusCode::OK, "{path}: {}", String::from_utf8_lossy(&r.bytes));
        r.json()
    }

    pub async fn get(&self, key: &str, path_and_query: &str) -> Reply {
        let req =
            Request::get(path_and_query).header("authorization", format!("Bearer {key}")).body(Body::empty()).unwrap();
        self.send(req).await
    }
}
