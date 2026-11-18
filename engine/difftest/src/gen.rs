//! Seeded generator: builds a small multi-day project the way the shipper and
//! compactor would have left it, and the logical truth it must read back as.
//!
//! Physical variety on purpose (design §17.3):
//! * several shipper "plans", each with its own L0 files and identity version;
//!   links that arrive after earlier files were written (query-time rewrite);
//! * late events (written in a later plan than their timestamp suggests);
//! * the same uuid in more than one file of a day, sometimes resolved
//!   differently or written under a different schema version;
//! * promoted columns introduced mid-way, with values that do not fit the
//!   column type left behind in `props`;
//! * days compacted to L2 (one or two files) at different points, with the
//!   tombstones known at that time physically applied; more L0 files, links
//!   and tombstones afterwards;
//! * events in the same millisecond, exactly on funnel-window boundaries
//!   (±1 ms) and on day/range boundaries.

use crate::model::{PVal, Persons, Row};
use la_core::events::{sort_and_dedupe, Cell, EventRow};
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::manifest::commit_day;
use la_core::parquet_io::{write_events_file, EventsFileMeta};
use la_core::persons::{self, PropWrite};
use la_core::schema::{ProjectSchema, PromotedColumn, PropType, DIM_COUNTRY, DIM_PLATFORM};
use la_core::tombstone::{tombstone_key, Tombstone, TombstoneSet};
use la_core::{keys, time, Store};
use rand::rngs::StdRng;
use rand::{Rng, SeedableRng};
use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};

pub const EVENTS: [&str; 5] = ["signup", "view", "cart", "purchase", "other"];
pub const PLATFORMS: [&str; 3] = ["ios", "android", "web"];
pub const COUNTRIES: [&str; 3] = ["TR", "DE", "US"];
/// Funnel windows (seconds) for which boundary cases are planted.
pub const WINDOWS: [i64; 4] = [1, 60, 3_600, 86_400];
/// First generated day: a Thursday, so five days cross a month and a week boundary.
pub const FIRST_DAY: &str = "2026-10-29";

#[derive(Debug, Clone)]
pub struct GenConfig {
    pub seed: u64,
    pub project: u32,
    pub days: usize,
    pub users: usize,
    pub anon_devices: usize,
    pub max_events: usize,
    pub plans: usize,
}

impl GenConfig {
    pub fn small(seed: u64) -> Self {
        Self { seed, project: 1, days: 5, users: 14, anon_devices: 10, max_events: 10, plans: 4 }
    }
}

#[derive(Debug, Clone, Default)]
pub struct GenStats {
    pub logical_events: usize,
    pub physical_rows: usize,
    pub files: usize,
    pub duplicate_copies: usize,
    pub late_events: usize,
    pub links: usize,
    pub rows_needing_rewrite: usize,
    pub tombstoned_rows: usize,
    pub compacted_days: usize,
}

#[derive(Debug, Clone)]
pub struct Dataset {
    pub project: u32,
    pub days: Vec<String>,
    pub from_ms: i64,
    pub to_ms: i64,
    /// The logical table: alive rows with their final `person_key`, sorted by
    /// `(person_key, ts, uuid)`.
    pub rows: Vec<Row>,
    pub persons: Persons,
    pub promoted: Vec<PromotedColumn>,
    pub stats: GenStats,
}

/// An event as the SDK produced it (identity not resolved).
#[derive(Debug, Clone)]
struct Raw {
    uuid: [u8; 16],
    ts: i64,
    event: String,
    anon: String,
    user: Option<String>,
    session: Option<[u8; 16]>,
    platform: Option<String>,
    country: Option<String>,
    props: BTreeMap<String, PVal>,
    plan: usize,
}

struct Device {
    anon: String,
    user: Option<String>,
    /// Events at or after this instant carry the user id.
    identify_at: Option<i64>,
}

fn pick<'a, T>(rng: &mut StdRng, items: &'a [T]) -> &'a T {
    &items[rng.random_range(0..items.len())]
}

fn random_props(rng: &mut StdRng) -> BTreeMap<String, PVal> {
    let mut p = BTreeMap::new();
    match rng.random_range(0..10) {
        0..=2 => {
            p.insert("amount".into(), PVal::Int(rng.random_range(0..40)));
        }
        3..=4 => {
            p.insert("amount".into(), PVal::Float(rng.random_range(0..80) as f64 / 2.0));
        }
        5 => {
            p.insert("amount".into(), PVal::Str(rng.random_range(0..40).to_string()));
        }
        6 => {
            p.insert("amount".into(), PVal::Str(pick(rng, &["oops", "1e3", "12.", ""]).to_string()));
        }
        7 => {
            p.insert("amount".into(), PVal::Bool(rng.random_bool(0.5)));
        }
        _ => {}
    }
    match rng.random_range(0..8) {
        0..=3 => {
            p.insert("plan".into(), PVal::Str(pick(rng, &["free", "pro", "team"]).to_string()));
        }
        4 => {
            p.insert("plan".into(), PVal::Int(3));
        }
        _ => {}
    }
    if rng.random_bool(0.3) {
        p.insert("n".into(), PVal::Int(rng.random_range(1..5)));
    }
    if rng.random_bool(0.2) {
        p.insert(
            "flag".into(),
            if rng.random_bool(0.8) { PVal::Bool(rng.random_bool(0.5)) } else { PVal::Str("true".into()) },
        );
    }
    if rng.random_bool(0.1) {
        p.insert("we\"ird key".into(), PVal::Str("q\"uo\\te\n".into()));
    }
    p
}

fn physical(e: &Raw, map: &IdentityMap, promoted: &[PromotedColumn]) -> EventRow {
    let mut props = e.props.clone();
    let mut cells = Vec::with_capacity(promoted.len());
    for c in promoted {
        let cell = match (c.ty, props.get(&c.key)) {
            (PropType::String, Some(PVal::Str(s))) => Some(Cell::Str(s.clone())),
            (PropType::Int, Some(PVal::Int(i))) => Some(Cell::Int(*i)),
            (PropType::Float, Some(PVal::Float(f))) => Some(Cell::Float(*f)),
            (PropType::Float, Some(PVal::Int(i))) => Some(Cell::Int(*i)),
            (PropType::Bool, Some(PVal::Bool(b))) => Some(Cell::Bool(*b)),
            // Type mismatch: the value stays in `props`, the column is null.
            _ => None,
        };
        if cell.is_some() {
            props.remove(&c.key);
        }
        cells.push(cell);
    }
    let json: serde_json::Map<String, serde_json::Value> =
        props.iter().map(|(k, v)| (k.clone(), v.to_json())).collect();
    let mut row = EventRow {
        uuid: e.uuid,
        ts: e.ts,
        recv_ts: e.ts + 7,
        event: e.event.clone(),
        person_key: map.person_key(e.user.as_deref(), &e.anon),
        anon_id: e.anon.clone(),
        user_id: e.user.clone(),
        session_id: e.session,
        promoted: cells,
        props: serde_json::Value::Object(json).to_string(),
        ..Default::default()
    };
    row.dims[DIM_PLATFORM] = e.platform.clone();
    row.dims[DIM_COUNTRY] = e.country.clone();
    row
}

#[allow(clippy::too_many_arguments)]
async fn write_file(
    store: &Store,
    project: u32,
    day: &str,
    level: u8,
    name: &str,
    mut rows: Vec<EventRow>,
    promoted: &[PromotedColumn],
    schema_version: u64,
    identity_version: u64,
) -> la_core::manifest::FileEntry {
    sort_and_dedupe(&mut rows);
    let meta = EventsFileMeta { project, day: day.to_owned(), level, schema_version, identity_version };
    let (bytes, mut entry) = write_events_file(&rows, promoted, &meta).expect("write events file");
    entry.key = format!("{}/L{level}-{name}.parquet", keys::data_day_prefix(project, day));
    store.put(&entry.key, bytes).await.expect("put data file");
    entry
}

/// Generates the project into `store` and returns its logical truth.
pub async fn generate(store: &Store, cfg: &GenConfig) -> Dataset {
    let mut rng = StdRng::seed_from_u64(cfg.seed);
    let project = cfg.project;
    let first = time::day_start_ms(FIRST_DAY).expect("first day");
    let days: Vec<String> = (0..cfg.days).map(|i| time::day_of(first + i as i64 * time::DAY_MS)).collect();
    let (from_ms, to_ms) = (first, first + cfg.days as i64 * time::DAY_MS);
    let span = to_ms - from_ms;
    let plan_of = |ts: i64| (((ts - from_ms) as i128 * cfg.plans as i128 / span as i128) as usize).min(cfg.plans - 1);
    let mut stats = GenStats::default();

    // --- people and devices -------------------------------------------------
    let mut devices: Vec<Device> = Vec::new();
    for u in 0..cfg.users {
        let user = format!("user{u:02}");
        for d in 0..rng.random_range(1..=2) {
            let identify_at = match rng.random_range(0..10) {
                0..=2 => Some(from_ms),                          // identified from the start
                3..=8 => Some(rng.random_range(from_ms..to_ms)), // identifies mid-way
                _ => None,                                       // never (stays anonymous)
            };
            devices.push(Device { anon: format!("dev-{u:02}-{d}"), user: Some(user.clone()), identify_at });
        }
    }
    for a in 0..cfg.anon_devices {
        devices.push(Device { anon: format!("anon-{a:02}"), user: None, identify_at: None });
    }

    // --- raw events ---------------------------------------------------------
    let mut next_uuid = 0u64;
    let mut uuid = |rng: &mut StdRng| {
        next_uuid += 1;
        // Random high bytes so uuid order is unrelated to generation order.
        let mut u = [0u8; 16];
        u[..8].copy_from_slice(&rng.random::<u64>().to_be_bytes());
        u[8..].copy_from_slice(&next_uuid.to_be_bytes());
        u
    };
    let mut raws: Vec<Raw> = Vec::new();
    for dev in &devices {
        let session: Option<[u8; 16]> = rng.random_bool(0.7).then(|| {
            let mut s = [0u8; 16];
            s[..8].copy_from_slice(&rng.random::<u64>().to_be_bytes());
            s
        });
        let platform = rng.random_bool(0.85).then(|| pick(&mut rng, &PLATFORMS).to_string());
        let country = rng.random_bool(0.8).then(|| pick(&mut rng, &COUNTRIES).to_string());
        let mut times: Vec<(i64, &str)> = Vec::new();
        let n = if rng.random_bool(0.08) { cfg.max_events * 3 } else { rng.random_range(0..=cfg.max_events) };
        for _ in 0..n {
            let ts = match rng.random_range(0..12) {
                // Day and range boundaries.
                0 => from_ms + rng.random_range(0..cfg.days as i64) * time::DAY_MS + *pick(&mut rng, &[-1i64, 0, 1]),
                1 => to_ms - 1,
                // Coarse grid so different people collide on timestamps.
                2..=4 => from_ms + rng.random_range(0..(span / 3_600_000)) * 3_600_000,
                _ => rng.random_range(from_ms..to_ms),
            };
            times.push((ts.clamp(from_ms, to_ms - 1), *pick(&mut rng, &EVENTS)));
        }
        // Planted funnel shapes around a window boundary.
        if rng.random_bool(0.6) {
            let w = *pick(&mut rng, &WINDOWS) * 1000;
            let t = rng.random_range(from_ms..(to_ms - w - 10).max(from_ms + 1));
            let edge = *pick(&mut rng, &[-1i64, 0, 0, 1]);
            times.push((t, "signup"));
            times.push((t + *pick(&mut rng, &[0i64, 0, 1, w / 2]), "cart"));
            times.push((t + w + edge, "purchase"));
            if rng.random_bool(0.5) {
                times.push((t + w + edge, *pick(&mut rng, &["cart", "purchase", "view"])));
            }
            if rng.random_bool(0.3) {
                times.push((t + *pick(&mut rng, &[1i64, w - 1, w]), "signup"));
            }
        }
        // Same-millisecond bursts.
        if rng.random_bool(0.4) {
            let t = rng.random_range(from_ms..to_ms);
            for _ in 0..rng.random_range(2..5) {
                times.push((t, *pick(&mut rng, &EVENTS)));
            }
        }
        for (ts, event) in times {
            let ts = ts.clamp(from_ms, to_ms - 1);
            let user = match (&dev.user, dev.identify_at) {
                (Some(u), Some(at)) if ts >= at => Some(u.clone()),
                _ => None,
            };
            let mut plan = plan_of(ts);
            if rng.random_bool(0.12) {
                plan = rng.random_range(plan..cfg.plans);
                stats.late_events += usize::from(plan != plan_of(ts));
            }
            raws.push(Raw {
                uuid: uuid(&mut rng),
                ts,
                event: event.to_owned(),
                anon: dev.anon.clone(),
                user,
                session: session.filter(|_| rng.random_bool(0.9)),
                platform: platform.clone().filter(|_| rng.random_bool(0.95)),
                country: country.clone(),
                props: random_props(&mut rng),
                plan,
            });
        }
    }
    stats.logical_events = raws.len();

    // --- person properties ----------------------------------------------------
    // (plan, key at write time, key, json value, ts); unique ts so last-writer is unambiguous.
    let mut person_writes: Vec<(usize, String, String, Option<PVal>, i64)> = Vec::new();
    let mut prop_ts = 1_000i64;
    for dev in &devices {
        for _ in 0..rng.random_range(0..3) {
            let plan = rng.random_range(0..cfg.plans);
            let at = from_ms + (plan as i64 * span / cfg.plans as i64);
            let identified = matches!((&dev.user, dev.identify_at), (Some(_), Some(t)) if at >= t);
            let key_then = match (&dev.user, identified) {
                (Some(u), true) => format!("u:{u}"),
                _ => format!("a:{}", dev.anon),
            };
            let (k, v) = match rng.random_range(0..6) {
                0..=2 => ("tier", Some(PVal::Str(pick(&mut rng, &["gold", "silver", "7"]).to_string()))),
                3 => ("tier", None),
                4 => ("score", Some(PVal::Int(rng.random_range(0..10)))),
                _ => ("score", Some(PVal::Float(rng.random_range(0..20) as f64 / 2.0))),
            };
            prop_ts += 1;
            person_writes.push((plan, key_then, k.to_owned(), v, prop_ts));
        }
    }

    // --- timeline -------------------------------------------------------------
    let promoted_all = vec![
        PromotedColumn { key: "amount".into(), ty: PropType::Float, since: 1 },
        PromotedColumn { key: "plan".into(), ty: PropType::String, since: 1 },
        PromotedColumn { key: "n".into(), ty: PropType::Int, since: 2 },
        PromotedColumn { key: "flag".into(), ty: PropType::Bool, since: 2 },
    ];
    let promote_first = rng.random_range(0..=cfg.plans); // == plans → never promoted
    let promote_second = rng.random_range(promote_first..=cfg.plans);
    let mut map = IdentityMap::default();
    let mut tombstones: Vec<Tombstone> = Vec::new();
    let mut manifests: HashMap<String, Vec<la_core::manifest::FileEntry>> = HashMap::new();
    let mut manifest_tomb: HashMap<String, i64> = HashMap::new();
    let mut written: HashMap<String, Vec<Raw>> = HashMap::new(); // per day, logical events written so far
    let mut compacted: HashSet<String> = HashSet::new();
    let mut schema_version = 0u64;
    let mut promoted: Vec<PromotedColumn> = Vec::new();

    for plan in 0..cfg.plans {
        if plan == promote_first {
            schema_version = 1;
            promoted = promoted_all[..2].to_vec();
        }
        if plan == promote_second && (plan != promote_first || rng.random_bool(0.5)) {
            schema_version = 2;
            promoted = promoted_all.clone();
        }
        // Identity links known by the end of this plan's window.
        let cutoff = from_ms + ((plan as i64 + 1) * span / cfg.plans as i64);
        let candidates: Vec<Candidate> = devices
            .iter()
            .filter_map(|d| match (&d.user, d.identify_at) {
                (Some(u), Some(at)) if at < cutoff && map.resolve(&d.anon).is_none() => {
                    Some(Candidate { anon_id: d.anon.clone(), user_id: u.clone(), ts: at, uuid: [0; 16] })
                }
                _ => None,
            })
            .collect();
        let out = identity::append_delta(store, project, "1-0", plan as u64 + 1, candidates, &mut map)
            .await
            .expect("identity delta");
        stats.links += out.added;
        let identity_version = out.version;

        let writes: Vec<PropWrite> = person_writes
            .iter()
            .filter(|w| w.0 == plan)
            .map(|(_, pk, k, v, ts)| PropWrite {
                person_key: pk.clone(),
                key: k.clone(),
                value: v.as_ref().map_or("null".to_owned(), |v| v.to_json().to_string()),
                ts: *ts,
                seq: 0,
            })
            .collect();
        persons::append_delta(store, project, "1-0", plan as u64 + 1, writes).await.expect("persons delta");

        // L0 files: one or two shards per day; some events are written twice.
        let mut per_day: BTreeMap<String, Vec<Vec<Raw>>> = BTreeMap::new();
        let mut copies: Vec<Raw> = Vec::new();
        for e in raws.iter().filter(|e| e.plan == plan) {
            copies.push(e.clone());
        }
        // Re-sends of events from earlier plans (same uuid, later file).
        for e in raws.iter().filter(|e| e.plan < plan) {
            if rng.random_bool(0.05) {
                copies.push(e.clone());
                stats.duplicate_copies += 1;
            }
        }
        for e in copies {
            let day = time::day_of(e.ts);
            let shards = per_day.entry(day).or_insert_with(|| vec![Vec::new(), Vec::new()]);
            let s = rng.random_range(0..2);
            if rng.random_bool(0.06) {
                shards[1 - s].push(e.clone());
                stats.duplicate_copies += 1;
            }
            shards[s].push(e);
        }
        for (day, shards) in per_day {
            for (i, shard) in shards.into_iter().enumerate() {
                if shard.is_empty() {
                    continue;
                }
                // A shard file may still be on the previous schema (written by
                // a shipper that had not picked up the promotion yet).
                let (cols, sv): (&[PromotedColumn], u64) =
                    if schema_version > 0 && rng.random_bool(0.15) { (&[], 0) } else { (&promoted, schema_version) };
                let rows: Vec<EventRow> = shard.iter().map(|e| physical(e, &map, cols)).collect();
                stats.physical_rows += rows.len();
                stats.rows_needing_rewrite += rows.iter().filter(|r| r.person_key.starts_with("a:")).count();
                let entry =
                    write_file(store, project, &day, 0, &format!("1-0-{plan:04}{i}"), rows, cols, sv, identity_version)
                        .await;
                manifests.entry(day.clone()).or_default().push(entry);
                // The new file has had no tombstone applied, so the manifest
                // may no longer claim any ("applied to all files").
                manifest_tomb.insert(day.clone(), 0);
                written.entry(day.clone()).or_default().extend(shard);
                stats.files += 1;
            }
        }

        // Tombstones issued during this plan.
        if (plan + 1 < cfg.plans || rng.random_bool(0.5)) && rng.random_bool(0.45) {
            let victim = pick(&mut rng, &devices);
            // A user is only deleted once all of their devices' links are
            // known, so "the user's anonymous ids" means the same thing
            // to a compaction now and to a query later.
            let settled = |u: &String| {
                devices
                    .iter()
                    .filter(|d| d.user.as_ref() == Some(u))
                    .all(|d| d.identify_at.is_none() || map.resolve(&d.anon).is_some())
            };
            let t = match &victim.user {
                Some(u) if settled(u) && rng.random_bool(0.7) => Tombstone {
                    id: format!("del{plan}"),
                    ts_ms: (plan as i64 + 1) * 1000,
                    person_keys: vec![format!("u:{u}")],
                    anon_ids: vec![],
                },
                _ => Tombstone {
                    id: format!("del{plan}"),
                    ts_ms: (plan as i64 + 1) * 1000,
                    person_keys: if rng.random_bool(0.5) { vec![format!("a:{}", victim.anon)] } else { vec![] },
                    anon_ids: vec![victim.anon.clone()],
                },
            };
            store.put_json(&tombstone_key(project, &t), &t).await.expect("tombstone");
            tombstones.push(t);
        }

        // Compaction of some days: current identity, dedupe, tombstones so far.
        for day in &days {
            let Some(events) = written.get(day) else { continue };
            if !rng.random_bool(0.3) {
                continue;
            }
            let set = TombstoneSet::build(&tombstones, &map, 0);
            let mut seen: HashSet<[u8; 16]> = HashSet::new();
            let mut rows: Vec<EventRow> = events
                .iter()
                .filter(|e| seen.insert(e.uuid))
                .map(|e| physical(e, &map, &promoted))
                .filter(|r| !set.matches(&r.person_key, &r.anon_id, r.user_id.as_deref()))
                .collect();
            rows.sort_by(|a, b| a.sort_key_cmp(b));
            let mut entries = Vec::new();
            if !rows.is_empty() {
                // Large days are split by person range into several L2 files.
                let split = if rows.len() > 6 && rng.random_bool(0.4) {
                    let mid = rows.len() / 2;
                    let key = rows[mid].person_key.clone();
                    rows.iter().position(|r| r.person_key == key).filter(|p| *p > 0)
                } else {
                    None
                };
                let parts: Vec<Vec<EventRow>> = match split {
                    Some(p) => {
                        let tail = rows.split_off(p);
                        vec![rows, tail]
                    }
                    None => vec![rows],
                };
                for (i, part) in parts.into_iter().enumerate() {
                    let name = format!("{:08}-{i:03}", plan + 1);
                    entries.push(
                        write_file(store, project, day, 2, &name, part, &promoted, schema_version, map.version()).await,
                    );
                    stats.files += 1;
                }
            }
            manifests.insert(day.clone(), entries);
            manifest_tomb.insert(day.clone(), set.max_ts);
            compacted.insert(day.clone());
        }

        // Publish the manifests as they stand after this plan.
        for day in &days {
            let (Some(files), tomb) = (manifests.get(day).cloned(), manifest_tomb.get(day).copied().unwrap_or(0))
            else {
                continue;
            };
            commit_day(store, project, day, |m| {
                if m.files == files && m.tombstone_ts == tomb {
                    return Ok(false);
                }
                m.files = files.clone();
                m.tombstone_ts = tomb;
                Ok(true)
            })
            .await
            .expect("manifest");
        }
    }
    stats.compacted_days = compacted.len();
    let schema = ProjectSchema { version: schema_version, project, columns: promoted.clone(), ..Default::default() };
    schema.save(store).await.expect("schema");

    // --- truth ------------------------------------------------------------------
    let set = TombstoneSet::build(&tombstones, &map, 0);
    let mut seen: BTreeSet<[u8; 16]> = BTreeSet::new();
    let mut rows: Vec<Row> = Vec::new();
    for e in &raws {
        if !seen.insert(e.uuid) {
            continue;
        }
        let person_key = map.person_key(e.user.as_deref(), &e.anon);
        if set.matches(&person_key, &e.anon, e.user.as_deref()) {
            stats.tombstoned_rows += 1;
            continue;
        }
        rows.push(Row {
            uuid: e.uuid,
            ts: e.ts,
            recv_ts: e.ts + 7,
            event: e.event.clone(),
            person_key,
            anon_id: e.anon.clone(),
            user_id: e.user.clone(),
            session_id: e.session,
            platform: e.platform.clone(),
            country: e.country.clone(),
            props: e.props.clone(),
        });
    }
    rows.sort_by(|a, b| (a.person_key.as_bytes(), a.ts, a.uuid).cmp(&(b.person_key.as_bytes(), b.ts, b.uuid)));

    // Person properties: re-key through the final identity map, last write wins.
    let mut latest: HashMap<(String, String), (i64, Option<PVal>)> = HashMap::new();
    for (_, pk, k, v, ts) in &person_writes {
        let key = match pk.strip_prefix("a:").and_then(|a| map.resolve(a)) {
            Some(u) => format!("u:{u}"),
            None => pk.clone(),
        };
        let slot = latest.entry((key, k.clone())).or_insert((i64::MIN, None));
        if *ts > slot.0 {
            *slot = (*ts, v.clone());
        }
    }
    let mut persons_truth: Persons = HashMap::new();
    for ((pk, k), (_, v)) in latest {
        if let Some(v) = v {
            persons_truth.entry(pk).or_default().insert(k, v);
        }
    }
    // Deleted persons disappear from the person table as well.
    persons_truth.retain(|k, _| !set.person_keys.contains(k));
    Dataset { project, days, from_ms, to_ms, rows, persons: persons_truth, promoted, stats }
}
