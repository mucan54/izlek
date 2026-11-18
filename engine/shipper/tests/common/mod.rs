//! Fixture builders shared by the integration tests.
#![allow(dead_code)]

use la_codec::wal::{Envelope, ReplayIndex, SegmentWriter, FORMAT_NATIVE, FORMAT_POSTHOG, FORMAT_REPLAY_INDEX};
use la_codec::wire::{self, Header, Prop, Record, Value, KIND_EVENT, KIND_IDENTIFY, KIND_SET_PERSON};
use la_compactor::inspect::{self, LogicalRow};
use la_core::identity::IdentityMap;
use la_core::persons::PersonTable;
use la_core::schema::ProjectSchema;
use la_core::time::day_start_ms;
use la_core::Store;
use la_shipper::{ShipConfig, ShipCtx};
use std::collections::BTreeMap;

pub const DAY: &str = "2026-11-18";
pub const HOUR_MS: i64 = 3_600_000;

/// 2026-11-18T10:00:00Z — a convenient "event time" inside [`DAY`].
pub fn t0() -> i64 {
    day_start_ms(DAY).unwrap() + 10 * HOUR_MS
}

/// The shipper's wall clock in tests: later the same day.
pub fn now() -> i64 {
    day_start_ms(DAY).unwrap() + 22 * HOUR_MS
}

pub fn ctx(store: &Store) -> ShipCtx {
    let cfg = ShipConfig { tombstone_refresh_ms: 0, ..ShipConfig::default() };
    ShipCtx::new(store.clone(), cfg).with_now(now())
}

pub fn uuid(n: u64) -> [u8; 16] {
    let mut u = [0u8; 16];
    u[0] = 0x01;
    u[8..].copy_from_slice(&n.to_be_bytes());
    u
}

pub fn env(format: u8, project: u32, recv_ts: i64) -> Envelope {
    Envelope { format, project_id: project, recv_ts_ms: recv_ts as u64, country: *b"TR", flags: 0 }
}

pub fn s(v: &str) -> Value {
    Value::Str(v.to_owned())
}

pub fn prop(k: &str, v: Value) -> Prop {
    Prop::new(k, v)
}

pub fn header(anon: &str, user: &str, sent_at: i64) -> Header {
    Header {
        batch_id: [7; 16],
        sent_at: sent_at as u64,
        sdk: "la-kmp/1.2.0".into(),
        anon_id: anon.into(),
        user_id: user.into(),
        session_id: [0; 16],
        context: vec![prop("platform", s("android")), prop("os", s("Android")), prop("app_version", s("3.1"))],
    }
}

pub fn event(id: u64, ts: i64, name: &str, props: Vec<Prop>) -> Record {
    Record { kind: KIND_EVENT, uuid: uuid(id), ts: ts as u64, name: name.into(), props }
}

pub fn identify(id: u64, ts: i64, user: &str, mut props: Vec<Prop>) -> Record {
    props.push(prop("user_id", s(user)));
    Record { kind: KIND_IDENTIFY, uuid: uuid(id), ts: ts as u64, name: String::new(), props }
}

pub fn set_person(id: u64, ts: i64, props: Vec<Prop>) -> Record {
    Record { kind: KIND_SET_PERSON, uuid: uuid(id), ts: ts as u64, name: String::new(), props }
}

/// A WAL entry: envelope + body.
pub type Entry = (Envelope, Vec<u8>);

pub fn native(project: u32, recv_ts: i64, h: &Header, records: &[Record]) -> Entry {
    (env(FORMAT_NATIVE, project, recv_ts), wire::encode_batch(h, records))
}

pub fn posthog(project: u32, recv_ts: i64, json: &str) -> Entry {
    (env(FORMAT_POSTHOG, project, recv_ts), json.as_bytes().to_vec())
}

pub fn replay(project: u32, recv_ts: i64, idx: &ReplayIndex) -> Entry {
    (env(FORMAT_REPLAY_INDEX, project, recv_ts), idx.encode())
}

pub fn segment_bytes(entries: &[Entry]) -> Vec<u8> {
    let mut w = SegmentWriter::new();
    for (e, body) in entries {
        w.append(e, body);
    }
    w.finish()
}

pub fn compress(raw: &[u8]) -> Vec<u8> {
    zstd::stream::encode_all(raw, 3).unwrap()
}

pub fn seg_key(hh: u32, node: &str, seq: u64) -> String {
    format!("wal/dt={DAY}/hh={hh:02}/{node}-{seq:012}.wal.zst")
}

pub async fn put_segment(store: &Store, key: &str, entries: &[Entry]) {
    store.put(key, compress(&segment_bytes(entries))).await.unwrap();
}

/// Everything the crash-safety and idempotency tests compare.
#[derive(Debug, Clone, PartialEq)]
pub struct Snapshot {
    pub rows: BTreeMap<String, Vec<LogicalRow>>,
    pub identity: Vec<(String, String)>,
    pub persons: BTreeMap<String, BTreeMap<String, String>>,
    pub schema: ProjectSchema,
}

pub async fn snapshot(store: &Store, project: u32) -> Snapshot {
    let identity = IdentityMap::load(store, project).await.unwrap();
    let rows = inspect::project_rows(store, project, &identity, None).await.unwrap();
    let persons = PersonTable::load(store, project, &identity).await.unwrap();
    let mut links: Vec<(String, String)> = identity.iter().map(|l| (l.anon_id, l.user_id)).collect();
    links.sort();
    Snapshot {
        rows,
        identity: links,
        persons: persons.iter().map(|(k, v)| (k.clone(), v.clone())).collect(),
        schema: ProjectSchema::load(store, project).await.unwrap(),
    }
}

pub fn total_rows(s: &Snapshot) -> usize {
    s.rows.values().map(Vec::len).sum()
}

/// `props[key]` of a logical row as a JSON value.
pub fn p<'a>(row: &'a LogicalRow, key: &str) -> Option<&'a serde_json::Value> {
    row.props.get(key)
}

/// Deterministic xorshift generator for the property tests.
pub struct Rng(pub u64);

impl Rng {
    pub fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
    pub fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
    pub fn shuffle<T>(&mut self, v: &mut [T]) {
        for i in (1..v.len()).rev() {
            v.swap(i, self.below(i as u64 + 1) as usize);
        }
    }
}
