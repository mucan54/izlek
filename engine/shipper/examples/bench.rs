//! Synthetic measurements for the README (bytes/event, shipper throughput,
//! compaction throughput and peak RSS).
//!
//! ```text
//! cargo run --release -p la-shipper --example bench -- ship    <dir> <events> <plans>
//! cargo run --release -p la-shipper --example bench -- compact <dir> <level>
//! ```
//!
//! `ship` fills `<dir>` (a `file://` store) with a synthetic one-day WAL for
//! project 1, ships it in `<plans>` plans and prints sizes and throughput.
//! `compact` merges every file of every day into one run of `<level>` in a
//! fresh process, so the peak RSS it prints is the compaction's own.

use la_codec::wal::{Envelope, SegmentWriter, FORMAT_NATIVE};
use la_codec::wire::{self, Header, Prop, Record, Value, KIND_EVENT, KIND_IDENTIFY};
use la_compactor::job::{self, Group, Outcome, ProjectEnv};
use la_core::time::{day_of, day_start_ms, dt_hh, now_ms};
use la_core::{manifest, Store};
use la_shipper::ship::ship_once;
use la_shipper::{ShipConfig, ShipCtx};
use std::sync::Arc;
use std::time::Instant;

const PROJECT: u32 = 1;
const EVENTS_PER_BATCH: u64 = 40;
const BATCHES_PER_SEGMENT: u64 = 2_500;

struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
    fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
}

const NAMES: [&str; 12] = [
    "screen",
    "tap",
    "purchase",
    "search",
    "add_to_cart",
    "app_open",
    "app_background",
    "scroll",
    "share",
    "login",
    "notification_open",
    "checkout_step",
];
const SCREENS: [&str; 8] = ["Home", "Search", "Product", "Cart", "Checkout", "Profile", "Settings", "Orders"];
const MODELS: [&str; 6] = ["Pixel 8", "SM-S921B", "iPhone15,2", "iPhone14,5", "Redmi Note 13", "SM-A546E"];

fn s(v: &str) -> Value {
    Value::Str(v.to_owned())
}

/// One SDK batch of a device: shared context in the header, a handful of
/// typed properties per event.
fn batch(rng: &mut Rng, next_id: &mut u64, devices: u64, day_start: i64, span_ms: i64, recv: i64) -> Vec<u8> {
    let device = rng.below(devices);
    let identified = device % 10 < 3;
    let ios = device.is_multiple_of(3);
    let header = Header {
        batch_id: (rng.next() as u128 | ((rng.next() as u128) << 64)).to_be_bytes(),
        sent_at: recv as u64,
        sdk: if ios { "la-kmp-ios/1.4.2".into() } else { "la-kmp-android/1.4.2".into() },
        anon_id: format!("0193f1a2-{:04x}-7{:03x}-8a9b-{:012x}", device >> 32, device & 0xfff, device),
        user_id: if identified { format!("user-{device}") } else { String::new() },
        session_id: (rng.next() as u128 | ((device as u128) << 64)).to_be_bytes(),
        context: vec![
            Prop::new("platform", s(if ios { "ios" } else { "android" })),
            Prop::new("os", s(if ios { "iOS" } else { "Android" })),
            Prop::new("os_version", s(if ios { "18.1" } else { "14" })),
            Prop::new("device_model", s(MODELS[(device % 6) as usize])),
            Prop::new("device_type", s("phone")),
            Prop::new("app_version", s(["3.1.0", "3.1.1", "3.2.0"][(device % 3) as usize])),
            Prop::new("locale", s(["tr-TR", "en-US", "de-DE"][(device % 3) as usize])),
        ],
    };
    let start = day_start + rng.below(span_ms.max(1) as u64) as i64;
    let records: Vec<Record> = (0..EVENTS_PER_BATCH)
        .map(|i| {
            *next_id += 1;
            let name = NAMES[rng.below(NAMES.len() as u64) as usize];
            let mut props = vec![
                Prop::new("screen", s(SCREENS[rng.below(8) as usize])),
                Prop::new("plan", s(["free", "pro", "team"][(device % 3) as usize])),
                Prop::new("seq", Value::Int(i as i64)),
                Prop::new("ab_group", s(if device.is_multiple_of(2) { "control" } else { "variant" })),
            ];
            match name {
                "purchase" | "add_to_cart" | "checkout_step" => {
                    props.push(Prop::new("amount", Value::Float(rng.below(100_000) as f64 / 100.0)));
                    props.push(Prop::new("currency", s("TRY")));
                    props.push(Prop::new("sku", s(&format!("sku-{:05}", rng.below(20_000)))));
                }
                "search" => props.push(Prop::new("query_len", Value::Int(rng.below(40) as i64))),
                "scroll" => props.push(Prop::new("depth", Value::Float(rng.below(1000) as f64 / 1000.0))),
                _ => props.push(Prop::new("online", Value::Bool(rng.below(20) != 0))),
            }
            // UUIDv7 shape: 48-bit millisecond timestamp + 74 random bits (as real SDKs send).
            let ts = (start + i as i64 * 700).min(recv);
            let mut uuid = [0u8; 16];
            uuid[..6].copy_from_slice(&(ts as u64).to_be_bytes()[2..]);
            uuid[6..8].copy_from_slice(&(0x7000 | (rng.next() as u16 & 0x0fff)).to_be_bytes());
            uuid[8..].copy_from_slice(&((rng.next() & 0x3fff_ffff_ffff_ffff) | 0x8000_0000_0000_0000).to_be_bytes());
            Record { kind: KIND_EVENT, uuid, ts: ts as u64, name: name.into(), props }
        })
        .collect();
    wire::encode_batch(&header, &records)
}

fn rss_peak_mb() -> f64 {
    std::fs::read_to_string("/proc/self/status")
        .ok()
        .and_then(|s| {
            s.lines().find(|l| l.starts_with("VmHWM:")).and_then(|l| l.split_whitespace().nth(1)?.parse::<f64>().ok())
        })
        .map(|kb| kb / 1024.0)
        .unwrap_or(0.0)
}

async fn data_bytes(store: &Store) -> (u64, u64) {
    let mut bytes = 0;
    let mut rows = 0;
    for day in manifest::list_days(store, PROJECT).await.unwrap() {
        let m = manifest::load_day(store, PROJECT, &day).await.unwrap();
        bytes += m.files.iter().map(|f| f.bytes).sum::<u64>();
        rows += m.total_rows();
    }
    (bytes, rows)
}

async fn ship(dir: &str, events: u64, plans: u64) {
    let store = Store::local(dir).unwrap();
    let now = now_ms();
    let day_start = day_start_ms(&day_of(now)).unwrap();
    let (dt, hh) = dt_hh(now);
    let mut ctx = ShipCtx::new(store.clone(), ShipConfig::default());
    ctx.cfg.batch_bytes = u64::MAX; // one plan per call below
    let devices = (events / 25).max(1000);
    let mut rng = Rng(0x9e37_79b9_7f4a_7c15);
    let (mut next_id, mut seq) = (0u64, 0u64);
    let (mut raw_bytes, mut zst_bytes, mut shipped, mut ship_secs) = (0u64, 0u64, 0u64, 0.0f64);
    let per_plan = events / plans.max(1);
    for plan in 0..plans.max(1) {
        let target = next_id + per_plan;
        while next_id < target {
            let mut w = SegmentWriter::new();
            for _ in 0..BATCHES_PER_SEGMENT {
                if next_id >= target {
                    break;
                }
                let env = Envelope {
                    format: FORMAT_NATIVE,
                    project_id: PROJECT,
                    recv_ts_ms: now as u64,
                    country: *b"TR",
                    flags: 0,
                };
                w.append(&env, &batch(&mut rng, &mut next_id, devices, day_start, now - day_start, now));
            }
            if plan + 1 == plans.max(1) && next_id >= target {
                // The last plan also carries identify records: 5 % of the
                // anonymous devices get linked, so compaction has rows to re-key.
                for device in (0..devices).filter(|d| d % 10 >= 3 && d % 20 == 5) {
                    let header = Header {
                        batch_id: [1; 16],
                        anon_id: format!("0193f1a2-{:04x}-7{:03x}-8a9b-{:012x}", device >> 32, device & 0xfff, device),
                        ..Default::default()
                    };
                    let rec = Record {
                        kind: KIND_IDENTIFY,
                        uuid: (device as u128).to_be_bytes(),
                        ts: now as u64,
                        name: String::new(),
                        props: vec![Prop::new("user_id", s(&format!("late-user-{device}")))],
                    };
                    let env = Envelope {
                        format: FORMAT_NATIVE,
                        project_id: PROJECT,
                        recv_ts_ms: now as u64,
                        country: *b"TR",
                        flags: 0,
                    };
                    w.append(&env, &wire::encode_batch(&header, &[rec]));
                }
            }
            let raw = w.finish();
            let z = zstd::stream::encode_all(&raw[..], 3).unwrap();
            raw_bytes += raw.len() as u64;
            zst_bytes += z.len() as u64;
            seq += 1;
            store.put(&format!("wal/dt={dt}/hh={hh}/bench-{seq:012}.wal.zst"), z).await.unwrap();
        }
        let started = Instant::now();
        let out = ship_once(&ctx).await.unwrap();
        ship_secs += started.elapsed().as_secs_f64();
        shipped += out.iter().map(|o| o.records_out).sum::<u64>();
        assert!(out.iter().all(|o| o.decode.rejected.is_empty() && o.quarantined.is_empty()));
    }
    let (l0_bytes, rows) = data_bytes(&store).await;
    println!("events generated          : {next_id}");
    println!("rows shipped              : {shipped} ({rows} listed in manifests, {plans} plans)");
    println!("WAL raw bytes/event       : {:.1}", raw_bytes as f64 / next_id as f64);
    println!("WAL zstd bytes/event (w)  : {:.1}", zst_bytes as f64 / next_id as f64);
    println!("L0 parquet bytes/event    : {:.1}", l0_bytes as f64 / rows as f64);
    println!("ship time                 : {ship_secs:.1} s");
    println!("shipper throughput        : {:.0} events/s", shipped as f64 / ship_secs);
    println!("peak RSS (generator+ship) : {:.0} MB", rss_peak_mb());
}

async fn compact(dir: &str, level: u8) {
    let store = Store::local(dir).unwrap();
    let mut cfg = la_compactor::CompactorConfig::from_env();
    cfg.spool_dir = Some(std::path::Path::new(dir).join("_spool"));
    let ctx = la_compactor::Ctx::new(store.clone(), cfg);
    let env = Arc::new(ProjectEnv::load(&store, PROJECT).await.unwrap());
    for day in manifest::list_days(&store, PROJECT).await.unwrap() {
        let m = manifest::load_day(&store, PROJECT, &day).await.unwrap();
        let (in_files, in_bytes, in_rows) =
            (m.files.len(), m.files.iter().map(|f| f.bytes).sum::<u64>(), m.total_rows());
        let started = Instant::now();
        let out = job::compact(&ctx, &env, &day, Group { inputs: m.files.clone(), level }).await.unwrap();
        let secs = started.elapsed().as_secs_f64();
        let Outcome::Committed { manifest: after, added } = out else {
            panic!("compaction of {day} did not commit: {out:?}");
        };
        let out_bytes: u64 = added.iter().map(|f| f.bytes).sum();
        println!("day {day}: {in_files} files, {in_rows} rows, {:.1} MB in", in_bytes as f64 / 1e6);
        println!(
            "  → {} L{level} file(s), {} rows, {:.1} MB out",
            added.len(),
            after.total_rows(),
            out_bytes as f64 / 1e6
        );
        println!("  L{level} parquet bytes/event (b): {:.1}", out_bytes as f64 / after.total_rows() as f64);
        println!(
            "  time {secs:.1} s → {:.0} rows/s, {:.1} MB/s (input)",
            in_rows as f64 / secs,
            in_bytes as f64 / 1e6 / secs
        );
    }
    println!("identity links applied    : {}", env.identity.len());
    println!("peak RSS (compaction)     : {:.0} MB", rss_peak_mb());
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build().unwrap();
    match args.iter().map(String::as_str).collect::<Vec<_>>()[..] {
        ["ship", dir, events, plans] => rt.block_on(ship(dir, events.parse().unwrap(), plans.parse().unwrap())),
        ["compact", dir, level] => rt.block_on(compact(dir, level.parse().unwrap())),
        _ => {
            eprintln!("usage: bench ship <dir> <events> <plans> | bench compact <dir> <level>");
            std::process::exit(2);
        }
    }
}
