//! End-to-end ship loop: plans, L0 files, manifests, identity, persons,
//! promotion, shards, quarantine (`spec/manifest.md` §6, design §8, §17.4).

mod common;

use common::*;
use la_codec::wal::{ReplayIndex, ENV_FLAG_WEB, REPLAY_ENC_GZIP};
use la_codec::wire::Value;
use la_compactor::inspect;
use la_core::control::{ShardMap, Shards};
use la_core::identity::IdentityMap;
use la_core::manifest;
use la_core::parquet_io::read_kv_metadata;
use la_core::schema::{ProjectSchema, PropType};
use la_core::time::day_start_ms;
use la_core::tombstone::{tombstone_key, Tombstone};
use la_core::{keys, Store};
use la_shipper::plan::{load_plan, Plan};
use la_shipper::ship::{self, ship_once};

async fn all_keys(store: &Store) -> Vec<String> {
    store.list("").await.unwrap().into_iter().map(|o| o.key).collect()
}

fn basic_entries() -> Vec<Entry> {
    let anon = header("dev-1", "", t0());
    let known = header("dev-2", "user-2", t0());
    let yesterday = t0() - 24 * HOUR_MS;
    vec![
        native(
            7,
            t0(),
            &anon,
            &[
                event(1, t0() - 3000, "open", vec![prop("plan", s("free"))]),
                event(2, t0() - 2000, "view", vec![prop("path", s("/home"))]),
                identify(3, t0() - 1000, "user-1", vec![prop("plan", s("pro"))]),
                set_person(4, t0() - 900, vec![prop("theme", s("dark"))]),
            ],
        ),
        native(7, t0(), &known, &[event(5, t0() - 500, "open", vec![]), event(6, yesterday, "old", vec![])]),
        posthog(
            7,
            t0(),
            r#"{"api_key":"pk","batch":[
                {"event":"$pageview","distinct_id":"ph-anon","properties":{"$os":"iOS","$lib":"web"}},
                {"event":"$identify","distinct_id":"user-3","properties":{"$anon_distinct_id":"ph-anon","$set":{"plan":"team"}}}
            ]}"#,
        ),
        replay(
            7,
            t0(),
            &ReplayIndex {
                session_id: [5; 16],
                seq: 0,
                start_ts: (t0() - 100) as u64,
                duration_ms: 5000,
                bytes: 99,
                encoding: REPLAY_ENC_GZIP,
                anon_id: "dev-1".into(),
                user_id: String::new(),
                key: "replay/p=7/d=2026-11-18/05050505050505050505050505050505/000000.bin.gz".into(),
            },
        ),
        native(8, t0(), &header("other", "", 0), &[event(1, t0() - 1, "e", vec![])]),
    ]
}

#[tokio::test]
async fn ships_segments_into_sorted_l0_files() {
    let store = Store::memory();
    let seg = seg_key(10, "ing-01", 1);
    put_segment(&store, &seg, &basic_entries()).await;
    let ctx = ctx(&store);
    let plans = ship_once(&ctx).await.unwrap();
    assert_eq!(plans.len(), 1);
    let out = &plans[0];
    assert_eq!((out.plan, out.segments, out.records_out, out.files), (1, 1, 8, 3));
    assert_eq!((out.links, out.identity_conflicts, out.person_writes), (3, 0, 3));
    assert!(out.quarantined.is_empty() && out.decode.rejected.is_empty());

    // Plan object and `_done` marker.
    let plan: Plan = load_plan(&store, "1-0", 1).await.unwrap().unwrap();
    assert_eq!((plan.segments.clone(), plan.shardkey.as_str()), (vec![seg.clone()], "1-0"));
    assert!(store.exists("wal/_done/1-0/ing-01-000000000001.ok").await.unwrap());
    assert_eq!(store.get("wal/_done/1-0/ing-01-000000000001.ok").await.unwrap().unwrap().len(), 0);

    // Day manifests: deterministic L0 names, plans map, per-file metadata.
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert_eq!((m.version, m.plans.get("1-0"), m.files.len()), (1, Some(&1), 1));
    let f = &m.files[0];
    assert_eq!(f.key, format!("data/p=7/d={DAY}/L0-1-0-0000000001.parquet"));
    assert_eq!((f.level, f.rows, f.identity_version, f.schema_version), (0, 6, 1, 0));
    assert_eq!((f.min_person.as_str(), f.max_person.as_str()), ("u:user-1", "u:user-3"));
    assert_eq!((f.min_ts, f.max_ts), (t0() - 3000, t0()));
    let kv = read_kv_metadata(store.get(&f.key).await.unwrap().unwrap()).unwrap();
    assert!(kv.contains(&("la.level".into(), "0".into())) && kv.contains(&("la.day".into(), DAY.into())));
    assert_eq!(manifest::load_day(&store, 7, "2026-11-17").await.unwrap().files[0].rows, 1);
    assert_eq!(manifest::list_days(&store, 8).await.unwrap(), [DAY]);

    // Rows: stage-1 identity resolution already used the links of this very plan.
    let rows = inspect::file_rows(&store, &f.key).await.unwrap();
    let brief: Vec<_> = rows.iter().map(|r| (r.person_key.as_str(), r.event.as_str(), r.anon_id.as_str())).collect();
    assert_eq!(
        brief[..4],
        [
            ("u:user-1", "open", "dev-1"),
            ("u:user-1", "view", "dev-1"),
            ("u:user-1", "session_replay_chunk", "dev-1"),
            ("u:user-2", "open", "dev-2"),
        ]
    );
    // The PostHog page view happened before the $identify of the same plan and is resolved all the same.
    let pageview = rows.iter().find(|r| r.event == "$pageview").unwrap();
    assert_eq!((pageview.person_key.as_str(), pageview.anon_id.as_str(), pageview.user_id.as_deref()), ("u:user-3", "ph-anon", None));
    assert_eq!(pageview.dims[la_core::schema::DIM_PLATFORM].as_deref(), Some("web"));
    assert_eq!(rows.len(), 6);
    assert!(rows.iter().all(|r| r.user_id.as_deref() != Some("user-1")), "user_id keeps what the event carried");
    assert_eq!(rows[1].dims[la_core::schema::DIM_URL_PATH].as_deref(), Some("/home"));
    assert_eq!(p(&rows[2], "key").unwrap(), "replay/p=7/d=2026-11-18/05050505050505050505050505050505/000000.bin.gz");

    // Identity and persons side tables.
    let snap = snapshot(&store, 7).await;
    assert_eq!(
        snap.identity,
        [
            ("dev-1".to_owned(), "user-1".to_owned()),
            ("ph-anon".to_owned(), "user-3".to_owned()),
            ("user-3".to_owned(), "user-3".to_owned())
        ]
    );
    assert_eq!(snap.persons["u:user-1"]["plan"], "\"pro\"");
    assert_eq!(snap.persons["u:user-1"]["theme"], "\"dark\"", "anonymous write follows the identity merge");
    assert_eq!(snap.persons["u:user-3"]["plan"], "\"team\"");
    // The $identify event is an ordinary row too.
    let all = &snap.rows[DAY];
    assert!(all.iter().any(|r| r.event == "$identify" && r.person_key == "u:user-3"));
    assert_eq!(snap.schema.applied.get("1-0"), Some(&1));
    assert_eq!(snap.schema.stats.days[DAY].events, 5, "replay chunks are not counted");

    // Metrics.
    assert_eq!(ctx.metrics.counter_value("records_out_total", &[]), 8);
    assert_eq!(ctx.metrics.counter_value("identity_links_total", &[]), 3);
    assert!(ctx.metrics.counter_value("parquet_bytes_total", &[]) > 0);
}

/// Design §17.4: running the shipper again changes nothing.
#[tokio::test]
async fn rerunning_the_shipper_changes_nothing() {
    let store = Store::memory();
    put_segment(&store, &seg_key(10, "ing-01", 1), &basic_entries()).await;
    ship_once(&ctx(&store)).await.unwrap();
    let before = all_keys(&store).await;
    let snap = snapshot(&store, 7).await;
    for _ in 0..3 {
        assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
    }
    assert_eq!(all_keys(&store).await, before, "no object was added or removed");
    assert_eq!(snapshot(&store, 7).await, snap);
    // Uploading the very same segment again (ingestion retry) is not new work either.
    put_segment(&store, &seg_key(10, "ing-01", 1), &basic_entries()).await;
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
}

fn random_segments(rng: &mut Rng, n_segments: usize) -> Vec<Vec<Entry>> {
    let mut next_id = 1u64;
    let mut sent: Vec<(String, u64, i64)> = Vec::new();
    (0..n_segments)
        .map(|si| {
            (0..6)
                .map(|bi| {
                    let project = 7 + rng.below(2) as u32;
                    let anon = format!("dev-{}", rng.below(12));
                    // A device that identified sends its user id from then on.
                    let user = if rng.below(4) == 0 { format!("user-{}", rng.below(5)) } else { String::new() };
                    let recv = t0() + (si * 6 + bi) as i64 * 1000;
                    let mut recs = Vec::new();
                    for _ in 0..(1 + rng.below(8)) {
                        // Occasionally re-send an earlier event verbatim (same uuid, same ts).
                        if !sent.is_empty() && rng.below(6) == 0 {
                            let (a, id, ts) = sent[rng.below(sent.len() as u64) as usize].clone();
                            if a == anon {
                                recs.push(event(id, ts, "tap", vec![prop("n", Value::Int(id as i64))]));
                                continue;
                            }
                        }
                        let ts = recv - rng.below(30 * HOUR_MS as u64) as i64;
                        recs.push(event(next_id, ts, "tap", vec![prop("n", Value::Int(next_id as i64))]));
                        sent.push((anon.clone(), next_id, ts));
                        next_id += 1;
                    }
                    // Each device identifies as one fixed user (no conflicting links).
                    if rng.below(5) == 0 {
                        let u = format!("owner-of-{anon}");
                        recs.push(identify(next_id, recv - 1, &u, vec![]));
                        next_id += 1;
                    }
                    native(project, recv, &header(&anon, &user, 0), &recs)
                })
                .collect()
        })
        .collect()
}

/// Design §17.4: the order and batching in which segments are processed does
/// not change the event rows.
#[tokio::test]
async fn batching_and_order_do_not_change_the_rows() {
    for seed in [3u64, 17, 99] {
        let segments = random_segments(&mut Rng(seed), 6);
        let key = |i: usize| seg_key(10, "ing-01", i as u64 + 1);

        // (a) everything in one plan
        let one = Store::memory();
        for (i, seg) in segments.iter().enumerate() {
            put_segment(&one, &key(i), seg).await;
        }
        assert_eq!(ship_once(&ctx(&one)).await.unwrap().len(), 1);

        // (b) one plan per segment, in key order
        let each = Store::memory();
        for (i, seg) in segments.iter().enumerate() {
            put_segment(&each, &key(i), seg).await;
            assert_eq!(ship_once(&ctx(&each)).await.unwrap().len(), 1);
        }

        // (c) one plan per segment, shuffled arrival
        let shuffled = Store::memory();
        let mut order: Vec<usize> = (0..segments.len()).collect();
        Rng(seed ^ 0xabcdef).shuffle(&mut order);
        for i in order {
            put_segment(&shuffled, &key(i), &segments[i]).await;
            ship_once(&ctx(&shuffled)).await.unwrap();
        }

        // (d) a tiny size budget: several plans cut from one listing
        let small = Store::memory();
        for (i, seg) in segments.iter().enumerate() {
            put_segment(&small, &key(i), seg).await;
        }
        let mut c = ctx(&small);
        c.cfg.batch_bytes = 1;
        assert_eq!(ship_once(&c).await.unwrap().len(), segments.len());

        for project in [7u32, 8] {
            let reference = snapshot(&one, project).await;
            assert!(total_rows(&reference) > 20, "seed {seed}: fixture too small");
            for (name, store) in [("per-segment", &each), ("shuffled", &shuffled), ("small-batches", &small)] {
                let got = snapshot(store, project).await;
                assert_eq!(got.rows, reference.rows, "seed {seed} project {project} {name}");
                assert_eq!(got.identity, reference.identity, "seed {seed} project {project} {name}");
            }
        }
    }
}

/// The one documented exception to order independence: of two conflicting
/// identify links for the same anonymous id the first one **processed** wins.
#[tokio::test]
async fn conflicting_links_are_first_processed_wins() {
    let a = vec![native(7, t0(), &header("dev", "", 0), &[identify(1, t0() - 10, "alice", vec![])])];
    let b = vec![native(7, t0(), &header("dev", "", 0), &[identify(2, t0() - 20, "bob", vec![])])];
    let mut winners = Vec::new();
    for first_is_a in [true, false] {
        let store = Store::memory();
        let (x, y) = if first_is_a { (&a, &b) } else { (&b, &a) };
        put_segment(&store, &seg_key(10, "n", 1), x).await;
        ship_once(&ctx(&store)).await.unwrap();
        put_segment(&store, &seg_key(10, "n", 2), y).await;
        let out = ship_once(&ctx(&store)).await.unwrap();
        assert_eq!(out[0].identity_conflicts, 1);
        winners.push(IdentityMap::load(&store, 7).await.unwrap().resolve("dev").unwrap().to_owned());
    }
    assert_eq!(winners, ["alice", "bob"]);
    // Inside a single plan the choice is deterministic: smallest (ts, uuid).
    let store = Store::memory();
    put_segment(&store, &seg_key(10, "n", 1), &a).await;
    put_segment(&store, &seg_key(10, "n", 2), &b).await;
    ship_once(&ctx(&store)).await.unwrap();
    assert_eq!(IdentityMap::load(&store, 7).await.unwrap().resolve("dev"), Some("bob"));
}

/// Stats accumulate → a column appears → later files carry `p_*`.
#[tokio::test]
async fn promotion_end_to_end() {
    let store = Store::memory();
    let batch = |base: u64, n: u64| -> Vec<Entry> {
        (0..n / 100)
            .map(|b| {
                let recs: Vec<_> = (0..100)
                    .map(|i| {
                        let id = base + b * 100 + i;
                        let mut props = vec![
                            prop("plan", s(if id % 2 == 0 { "pro" } else { "free" })),
                            // inconsistent types → never promoted, however frequent
                            prop("mixed", if id % 2 == 0 { s("x") } else { Value::Int(1) }),
                        ];
                        if id % 5 != 0 {
                            // int and float mixed → float column
                            props.push(prop("amount", if id % 2 == 0 { Value::Int(id as i64) } else { Value::Float(0.5) }));
                        }
                        if id % 50 == 0 {
                            props.push(prop("rare", Value::Bool(true))); // below 5 % and below 1000 events
                        }
                        event(id, t0() - 1000, "purchase", props)
                    })
                    .collect();
                native(7, t0(), &header(&format!("dev-{b}"), "", 0), &recs)
            })
            .collect()
    };
    // Plan 1: 600 events — not enough volume yet.
    put_segment(&store, &seg_key(10, "n", 1), &batch(0, 600)).await;
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert!(out[0].promoted.is_empty());
    let schema = ProjectSchema::load(&store, 7).await.unwrap();
    assert_eq!((schema.version, schema.columns.len(), schema.stats.days[DAY].events), (0, 0, 600));
    assert_eq!(schema.stats.days[DAY].keys["plan"].n, 600);
    assert_eq!(schema.stats.days[DAY].names["purchase"], 600);

    // Plan 2: 900 more → 1500 events; `plan` (1500) and `amount` (1200) cross the thresholds.
    put_segment(&store, &seg_key(10, "n", 2), &batch(600, 900)).await;
    let out = ship_once(&ctx(&store)).await.unwrap();
    let mut promoted: Vec<&str> = out[0].promoted.iter().map(|(_, k)| k.as_str()).collect();
    promoted.sort();
    assert_eq!(promoted, ["amount", "plan"]);
    let schema = ProjectSchema::load(&store, 7).await.unwrap();
    assert_eq!(schema.version, 1);
    assert_eq!(schema.column("plan").unwrap().ty, PropType::String);
    assert_eq!(schema.column("amount").unwrap().ty, PropType::Float);
    assert!(schema.column("mixed").is_none() && schema.column("rare").is_none());
    assert_eq!(schema.stats.days[DAY].events, 1500);
    // The files written so far predate the columns.
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert!(m.files.iter().all(|f| f.schema_version == 0));

    // Plan 3: the new file carries the columns.
    let mut third = batch(1500, 100);
    third.push(native(
        7,
        t0(),
        &header("dev-x", "", 0),
        &[event(9001, t0() - 5, "purchase", vec![prop("plan", Value::Int(7)), prop("amount", s("n/a"))])],
    ));
    put_segment(&store, &seg_key(10, "n", 3), &third).await;
    ship_once(&ctx(&store)).await.unwrap();
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    let f = m.files.iter().find(|f| f.key.ends_with("0000000003.parquet")).unwrap();
    assert_eq!(f.schema_version, 1);
    let data = store.get(&f.key).await.unwrap().unwrap();
    let batches = la_core::parquet_io::read_parquet(data).unwrap();
    let file_schema = batches[0].schema();
    let names: Vec<&str> = file_schema.fields().iter().map(|f| f.name().as_str()).collect();
    let flags = names.iter().position(|n| *n == "flags").unwrap();
    assert_eq!(&names[flags + 1..], ["p_plan", "p_amount", "props"], "promoted columns sit between flags and props");
    let rows: Vec<_> = batches
        .iter()
        .flat_map(|b| la_core::events::batch_to_rows(b, &schema.columns).unwrap())
        .collect();
    use la_core::events::Cell;
    let by_id = |id: u64| rows.iter().find(|r| r.uuid == uuid(id)).unwrap();
    assert_eq!(by_id(1502).promoted, [Some(Cell::Str("pro".into())), Some(Cell::Float(1502.0))], "int widened");
    assert_eq!(by_id(1502).props, r#"{"mixed":"x"}"#, "promoted values left props");
    assert_eq!(by_id(1501).promoted, [Some(Cell::Str("free".into())), Some(Cell::Float(0.5))]);
    assert_eq!(by_id(1500).promoted, [Some(Cell::Str("pro".into())), None]);
    assert_eq!(by_id(1500).props, r#"{"mixed":"x","rare":true}"#);
    // Type conflicts stay in props, the column is null.
    assert_eq!(by_id(9001).promoted, [None, None]);
    assert_eq!(by_id(9001).props, r#"{"amount":"n/a","plan":7}"#);
    // Logical content is unaffected by where a value is stored.
    let snap = snapshot(&store, 7).await;
    assert_eq!(snap.rows[DAY].len(), 1601);
    assert!(snap.rows[DAY].iter().all(|r| r.props.contains_key("plan")));
}

#[tokio::test]
async fn a_shipper_only_processes_projects_of_its_shards() {
    let store = Store::memory();
    store.put_json(keys::SHARDS, &Shards { maps: vec![ShardMap { version: 1, from_ms: 0, shards: 2 }] }).await.unwrap();
    let projects: Vec<u32> = (1..=8).collect();
    let shard = |p: u32| la_codec::murmur3::shard_of(p, 2);
    assert!(projects.iter().any(|p| shard(*p) == 0) && projects.iter().any(|p| shard(*p) == 1));
    let entries: Vec<Entry> = projects
        .iter()
        .map(|p| native(*p, t0(), &header("dev", "", 0), &[event(*p as u64, t0() - 1, "e", vec![])]))
        .collect();
    put_segment(&store, &seg_key(10, "n", 1), &entries).await;

    let mut only0 = ctx(&store);
    only0.cfg.shards = vec!["0".into()];
    let out = ship_once(&only0).await.unwrap();
    assert_eq!(out.len(), 1);
    let shipped = manifest::list_projects(&store).await.unwrap();
    assert_eq!(shipped, projects.iter().copied().filter(|p| shard(*p) == 0).collect::<Vec<_>>());
    assert!(store.exists("wal/_done/1-0/n-000000000001.ok").await.unwrap());
    assert!(!store.exists("wal/_done/1-1/n-000000000001.ok").await.unwrap(), "`_done` is per shard");
    assert!(store.exists("wal/_plan/1-0/0000000001.json").await.unwrap());

    ship_once(&ctx(&store)).await.unwrap();
    assert_eq!(manifest::list_projects(&store).await.unwrap(), projects);
    for p in &projects {
        let m = manifest::load_day(&store, *p, DAY).await.unwrap();
        assert_eq!(m.plans.keys().collect::<Vec<_>>(), [&format!("1-{}", shard(*p))]);
        assert!(m.files[0].key.contains(&format!("L0-1-{}-", shard(*p))));
    }
}

#[tokio::test]
async fn a_shard_map_change_applies_by_segment_hour() {
    let store = Store::memory();
    let switch = day_start_ms(DAY).unwrap() + 12 * HOUR_MS;
    let maps = vec![ShardMap { version: 1, from_ms: 0, shards: 1 }, ShardMap { version: 2, from_ms: switch, shards: 2 }];
    store.put_json(keys::SHARDS, &Shards { maps }).await.unwrap();
    let e = |id: u64| vec![native(7, t0(), &header("dev", "", 0), &[event(id, t0() - 1, "e", vec![])])];
    put_segment(&store, &seg_key(11, "n", 1), &e(1)).await; // closed under map 1
    put_segment(&store, &seg_key(12, "n", 2), &e(2)).await; // closed under map 2
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!(out.len(), 3, "one plan per shardkey that had a segment to look at");
    let new_key = format!("2-{}", la_codec::murmur3::shard_of(7, 2));
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    assert_eq!(m.plans.keys().cloned().collect::<Vec<_>>(), ["1-0".to_owned(), new_key.clone()]);
    assert_eq!(m.total_rows(), 2);
    assert!(store.exists("wal/_done/1-0/n-000000000001.ok").await.unwrap());
    assert!(!store.exists("wal/_done/1-0/n-000000000002.ok").await.unwrap());
    assert!(store.exists(&format!("wal/_done/{new_key}/n-000000000002.ok")).await.unwrap());
}

/// A corrupt or truncated segment and an unknown entry format do not stall
/// the pipeline (design §18.7 "bozuk segment").
#[tokio::test]
async fn corrupt_segments_are_quarantined_and_the_rest_ships() {
    let store = Store::memory();
    let good = |id: u64| native(7, t0(), &header("dev", "", 0), &[event(id, t0() - 1, "e", vec![])]);
    // 1: not zstd at all
    store.put(&seg_key(10, "n", 1), &b"this is not a zstd frame"[..]).await.unwrap();
    // 2: valid zstd, wrong magic
    store.put(&seg_key(10, "n", 2), compress(b"LAW9 whatever")).await.unwrap();
    // 3: torn tail — the last entry is cut in the middle
    let raw = segment_bytes(&[good(31), good(32), good(33)]);
    store.put(&seg_key(10, "n", 3), compress(&raw[..raw.len() - 7])).await.unwrap();
    // 4: a flipped bit in the middle entry — everything from there on is ignored
    let mut raw = segment_bytes(&[good(41), good(42), good(43)]);
    let third = raw.len() / 2;
    raw[third] ^= 0x40;
    store.put(&seg_key(10, "n", 4), compress(&raw)).await.unwrap();
    // 5: an entry of a format this shipper does not know, between good ones
    let mut future = good(999);
    future.0.format = 42;
    put_segment(&store, &seg_key(10, "n", 5), &[good(51), future, good(52)]).await;
    // 6: zstd stream cut short
    let z = compress(&segment_bytes(&[good(61), good(62)]));
    store.put(&seg_key(10, "n", 6), z[..z.len() - 5].to_vec()).await.unwrap();
    // 7: empty segment (magic only), 8: plain good one
    put_segment(&store, &seg_key(10, "n", 7), &[]).await;
    put_segment(&store, &seg_key(10, "n", 8), &[good(81)]).await;

    let c = ctx(&store);
    let out = ship_once(&c).await.unwrap();
    assert_eq!(out.len(), 1);
    let mut reasons: Vec<(String, String)> = out[0].quarantined.clone();
    reasons.sort();
    assert_eq!(
        reasons,
        [
            (seg_key(10, "n", 1), "zstd".to_owned()),
            (seg_key(10, "n", 2), "bad_magic".to_owned()),
            (seg_key(10, "n", 6), "zstd".to_owned())
        ]
    );
    assert_eq!(out[0].corrupt_tails, 2);
    assert_eq!(out[0].decode.unknown_format, 1);
    let ids: Vec<[u8; 16]> = snapshot(&store, 7).await.rows[DAY].iter().map(|r| r.uuid).collect();
    let mut expect: Vec<[u8; 16]> = [31, 32, 41, 51, 52, 81].iter().map(|i| uuid(*i)).collect();
    expect.sort();
    let mut got = ids.clone();
    got.sort();
    assert_eq!(got, expect);
    // Every segment is marked done (quarantined ones too) and the record names the reason.
    for i in 1..=8 {
        assert!(store.exists(&format!("wal/_done/1-0/n-{i:012}.ok")).await.unwrap(), "segment {i}");
    }
    let q: serde_json::Value = store.get_json(&keys::wal_quarantine("1-0", "n-000000000002")).await.unwrap().unwrap();
    assert_eq!((q["reason"].as_str(), q["plan"].as_u64()), (Some("bad_magic"), Some(1)));
    assert_eq!(c.metrics.counter_value("segments_quarantined_total", &[("reason", "zstd")]), 2);
    assert_eq!(c.metrics.counter_value("wal_corrupt_tail_total", &[]), 2);
    assert_eq!(c.metrics.counter_value("entries_skipped_total", &[("reason", "unknown_format")]), 1);
    // Nothing is retried forever.
    assert!(ship_once(&ctx(&store)).await.unwrap().is_empty());
}

#[tokio::test]
async fn project_config_drop_keys_unknown_projects_and_bots() {
    let store = Store::memory();
    store
        .put(keys::PROJECTS, &br#"{"version":1,"projects":{"7":{"name":"Acme","drop_keys":["email"]}}}"#[..])
        .await
        .unwrap();
    let mut bot_header = header("crawler", "", 0);
    bot_header.context.push(prop("ua", s("Googlebot/2.1")));
    let mut bot = native(7, t0(), &bot_header, &[event(3, t0() - 1, "pageview", vec![])]);
    bot.0.flags = ENV_FLAG_WEB;
    let entries = vec![
        native(7, t0(), &header("dev", "", 0), &[event(1, t0() - 1, "e", vec![prop("email", s("a@b.c")), prop("k", Value::Int(1))])]),
        native(8, t0(), &header("dev", "", 0), &[event(2, t0() - 1, "e", vec![]), event(4, t0() - 1, "e", vec![])]),
        bot,
        native(7, t0(), &header("dev", "", 0), &[event(5, t0() - 1, "", vec![])]),
    ];
    put_segment(&store, &seg_key(10, "n", 1), &entries).await;
    let c = ctx(&store);
    let out = ship_once(&c).await.unwrap();
    let rejected: Vec<_> = out[0].decode.rejected.iter().map(|(k, v)| (*k, *v)).collect();
    assert_eq!(rejected, [("bot", 1), ("empty_name", 1), ("unknown_project", 2)]);
    for (reason, n) in rejected {
        assert_eq!(c.metrics.counter_value("records_rejected_total", &[("reason", reason)]), n);
    }
    assert_eq!(manifest::list_projects(&store).await.unwrap(), [7]);
    let rows = &snapshot(&store, 7).await.rows[DAY];
    assert_eq!(rows.len(), 1);
    assert!(rows[0].props.contains_key("k") && !rows[0].props.contains_key("email"));

    // With `accept`, projects the control plane has not published still ship.
    let store2 = Store::memory();
    store2.put(keys::PROJECTS, store.get(keys::PROJECTS).await.unwrap().unwrap()).await.unwrap();
    put_segment(&store2, &seg_key(10, "n", 1), &entries).await;
    let mut c2 = ctx(&store2);
    c2.cfg.unknown_projects = la_shipper::config::UnknownProjects::Accept;
    ship_once(&c2).await.unwrap();
    assert_eq!(manifest::list_projects(&store2).await.unwrap(), [7, 8]);
}

/// Rows of deleted people never reach an L0 file, and the manifest only
/// claims the tombstones that were really applied.
#[tokio::test]
async fn tombstoned_people_are_filtered_at_write_time() {
    let store = Store::memory();
    let chunk_key = "replay/p=7/d=2026-11-18/05050505050505050505050505050505/000000.bin.gz";
    store.put(chunk_key, &b"chunk"[..]).await.unwrap();
    let entries = vec![
        native(7, t0(), &header("dev-gone", "gone", 0), &[event(1, t0() - 5, "e", vec![])]),
        native(7, t0(), &header("dev-anon", "", 0), &[event(2, t0() - 4, "e", vec![])]),
        native(7, t0(), &header("dev-keep", "keep", 0), &[event(3, t0() - 3, "e", vec![])]),
        replay(
            7,
            t0(),
            &ReplayIndex {
                session_id: [5; 16],
                seq: 0,
                start_ts: (t0() - 100) as u64,
                duration_ms: 1,
                bytes: 5,
                encoding: REPLAY_ENC_GZIP,
                anon_id: "dev-gone".into(),
                user_id: "gone".into(),
                key: chunk_key.into(),
            },
        ),
    ];
    // First plan: no tombstones yet.
    put_segment(&store, &seg_key(9, "n", 1), &entries[2..3]).await;
    ship_once(&ctx(&store)).await.unwrap();
    assert_eq!(manifest::load_day(&store, 7, DAY).await.unwrap().tombstone_ts, 0);

    let t = Tombstone { id: "del_1".into(), ts_ms: 5000, person_keys: vec!["u:gone".into()], anon_ids: vec!["dev-anon".into()] };
    store.put_json(&tombstone_key(7, &t), &t).await.unwrap();
    put_segment(&store, &seg_key(10, "n", 2), &entries).await;
    let out = ship_once(&ctx(&store)).await.unwrap();
    assert_eq!((out[0].tombstoned, out[0].records_out), (3, 1));
    let m = manifest::load_day(&store, 7, DAY).await.unwrap();
    // The older file never had the tombstone applied: the day must not claim it.
    assert_eq!(m.tombstone_ts, 0);
    let rows = &snapshot(&store, 7).await.rows[DAY];
    assert!(rows.iter().all(|r| r.person_key == "u:keep"), "{rows:?}");
    assert!(!store.exists(chunk_key).await.unwrap(), "replay object of the deleted person is removed");

    // A day created after the tombstone starts out with it applied.
    let store2 = Store::memory();
    store2.put_json(&tombstone_key(7, &t), &t).await.unwrap();
    put_segment(&store2, &seg_key(10, "n", 2), &entries).await;
    ship_once(&ctx(&store2)).await.unwrap();
    assert_eq!(manifest::load_day(&store2, 7, DAY).await.unwrap().tombstone_ts, 5000);
}

/// The long-running loop: takes the lease, ships, exports metrics, stops.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn run_loop_holds_the_lease_and_ships() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::local(dir.path()).unwrap();
    // The loop uses the real clock: place the segment in the current hour.
    let now = la_core::time::now_ms();
    let (dt, hh) = la_core::time::dt_hh(now);
    let key = format!("wal/dt={dt}/hh={hh}/ing-01-000000000001.wal.zst");
    let entries = vec![native(7, now, &header("dev", "", 0), &[event(1, now - 10, "e", vec![])])];
    store.put(&key, compress(&segment_bytes(&entries))).await.unwrap();

    let mut c = la_shipper::ShipCtx::new(store.clone(), la_shipper::ShipConfig::default());
    c.cfg.poll_interval_ms = 20;
    c.cfg.batch_interval_ms = 0;
    c.holder = "ship-a".into();
    let metrics = c.metrics.clone();
    let (tx, rx) = tokio::sync::watch::channel(false);
    let task = tokio::spawn(ship::run(c, rx));
    let day = la_core::time::day_of(now - 10);
    let mut shipped = false;
    for _ in 0..200 {
        tokio::time::sleep(std::time::Duration::from_millis(25)).await;
        if manifest::load_day(&store, 7, &day).await.unwrap().total_rows() == 1 {
            shipped = true;
            break;
        }
    }
    assert!(shipped, "the loop never shipped the segment");
    let lease = la_core::lease::Lease::new(store.clone(), "shipper-1-0", "someone-else");
    assert_eq!(lease.current().await.unwrap().unwrap().holder, "ship-a");
    assert!(!lease.try_hold(la_core::time::now_ms()).await.unwrap(), "a standby cannot take a live lease");
    let text = metrics.render();
    assert!(text.contains("la_shipper_lease_held{shard=\"1-0\"} 1"), "{text}");
    assert!(text.contains("la_shipper_segments_pending{shard=\"1-0\"} 0"), "{text}");
    assert!(text.contains("la_shipper_ship_lag_seconds{shard=\"1-0\"} 0"), "{text}");
    assert!(text.contains("la_shipper_records_out_total 1"), "{text}");
    tx.send(true).unwrap();
    task.await.unwrap().unwrap();
}
