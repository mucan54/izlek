//! Person deletion end to end (design §10.5, `spec/manifest.md` §9).

mod common;

use common::*;
use la_compactor::job::{self, CommitPlan, Outcome, Replace, Staged, TombstoneAdvance};
use la_compactor::merge::REPLAY_CHUNK_EVENT;
use la_compactor::tombstones::{self, TombstoneState};
use la_compactor::{inspect, run};
use la_core::events::EventRow;
use la_core::identity::{self, Candidate, IdentityMap};
use la_core::persons::{self, PersonTable, PropWrite};
use la_core::tombstone::{self, tombstone_key, Tombstone};
use la_core::{keys, manifest, Store};
use std::collections::HashSet;

const YESTERDAY: &str = "2026-11-17";
const K_ALICE: &str = "replay/p=7/d=2026-11-17/aa/000000.bin.zst";
const K_BOB: &str = "replay/p=7/d=2026-11-17/bb/000000.bin.zst";

fn chunk(id: u64, person: &str, anon: &str, key: &str) -> EventRow {
    let mut r = row(id, person, anon, t0() - DAY_MS + id as i64);
    r.event = REPLAY_CHUNK_EVENT.into();
    r.props = format!("{{\"bytes\":1,\"key\":\"{key}\",\"seq\":0}}");
    r
}

async fn seed(store: &Store) -> (String, String) {
    let mut map = IdentityMap::default();
    let link = |a: &str, u: &str, id: u8| Candidate { anon_id: a.into(), user_id: u.into(), ts: 1, uuid: [id; 16] };
    identity::append_delta(store, 7, "1-0", 1, vec![link("dev-1", "alice", 1), link("dev-9", "zed", 2)], &mut map)
        .await
        .unwrap();
    let w = |pk: &str, k: &str| PropWrite { person_key: pk.into(), key: k.into(), value: "1".into(), ts: 5, seq: 0 };
    persons::append_delta(store, 7, "1-0", 1, vec![w("u:alice", "plan"), w("a:dev-1", "theme"), w("u:bob", "x")])
        .await
        .unwrap();

    // Yesterday: one file with rows of several people.
    let y = t0() - DAY_MS;
    let mut carol_on_alices_device = row(13, "u:carol", "dev-1", y + 3);
    carol_on_alices_device.user_id = Some("carol".into());
    let mut malicious = chunk(16, "u:alice", "dev-1", "control/projects.json");
    malicious.ts += 1;
    add_l0(
        store,
        7,
        YESTERDAY,
        1,
        vec![
            row(10, "u:alice", "dev-1", y),
            row(11, "a:dev-1", "dev-1", y + 1), // written before the link existed
            row(12, "u:bob", "dev-2", y + 2),
            carol_on_alices_device,
            chunk(14, "u:alice", "dev-1", K_ALICE),
            chunk(15, "u:bob", "dev-2", K_BOB),
            malicious,
        ],
    )
    .await;
    // Today: one affected and one clean L0.
    let affected =
        add_l0(store, 7, DAY, 2, vec![row(20, "u:alice", "dev-1", t0()), row(21, "u:bob", "dev-2", t0())]).await;
    let clean =
        add_l0(store, 7, DAY, 3, vec![row(22, "u:bob", "dev-2", t0() + 1), row(23, "u:zed", "dev-9", t0() + 2)]).await;
    store.put(K_ALICE, &b"alice chunk"[..]).await.unwrap();
    store.put(K_BOB, &b"bob chunk"[..]).await.unwrap();
    store.put(keys::PROJECTS, &br#"{"version":1,"projects":{"7":{"name":"Acme"}}}"#[..]).await.unwrap();
    (affected, clean)
}

fn is_alice(r: &inspect::LogicalRow) -> bool {
    r.person_key == "u:alice" || r.anon_id == "dev-1" || r.user_id.as_deref() == Some("alice")
}

#[tokio::test]
async fn tombstone_flow_end_to_end() {
    let store = Store::memory();
    let (affected, clean) = seed(&store).await;
    // Yesterday is closed: let the compactor finalize it into L2 first.
    let c = ctx(&store);
    let all = |_: u32| true;
    let r = run::cycle(&c, open_now(), true, false, &all).await.unwrap();
    assert_eq!((r.compactions, r.tombstones_pending), (1, 0));
    assert_eq!(day(&store, 7, YESTERDAY).await.files[0].level, 2);
    assert_eq!(visible(&store, 7, YESTERDAY).await.len(), 7);

    let ts = open_now() - 60_000;
    let t = Tombstone { id: "del_1".into(), ts_ms: ts, person_keys: vec!["u:alice".into()], anon_ids: vec![] };
    store.put_json(&tombstone_key(7, &t), &t).await.unwrap();
    // Queries already hide her (row filter) …
    assert!(visible(&store, 7, YESTERDAY).await.iter().all(|r| !is_alice(r)));
    // … but the bytes are still there until the compactor rewrites the days.
    let identity = IdentityMap::load(&store, 7).await.unwrap();
    let raw = inspect::day_rows(&store, 7, YESTERDAY, &identity, None).await.unwrap();
    assert_eq!(raw.iter().filter(|r| is_alice(r)).count(), 5);

    let r = run::cycle(&c, open_now(), true, false, &all).await.unwrap();
    assert_eq!((r.errors, r.tombstones_pending), (0, 0), "{r:?}");

    // Data: rewritten without her rows, physically.
    let identity = IdentityMap::load(&store, 7).await.unwrap();
    for (d, expect) in [(YESTERDAY, vec![12u64, 15]), (DAY, vec![21, 22, 23])] {
        let m = day(&store, 7, d).await;
        assert_eq!(m.tombstone_ts, ts, "{d}");
        let rows = inspect::day_rows(&store, 7, d, &identity, None).await.unwrap();
        assert!(rows.iter().all(|r| !is_alice(r)), "{d}");
        let mut got = ids(&rows);
        got.sort_unstable();
        assert_eq!(got, expect, "{d}");
        assert_manifest_consistent(&store, 7, d).await;
    }
    // Shape: yesterday's L2 was rewritten as L2; today the affected L0 became an L1, the clean L0 was not touched.
    assert_eq!(day(&store, 7, YESTERDAY).await.files.iter().map(|f| f.level).collect::<Vec<_>>(), [2]);
    let today = day(&store, 7, DAY).await;
    let listed: HashSet<&str> = today.files.iter().map(|f| f.key.as_str()).collect();
    assert!(listed.contains(clean.as_str()) && !listed.contains(affected.as_str()));
    assert_eq!(today.files.iter().map(|f| f.level).collect::<Vec<_>>(), [0, 1]);
    assert_eq!(today.plans.get("1-0"), Some(&3));

    // Replay objects: hers is gone, others stay; a client-supplied key outside replay/ is never honoured.
    assert!(!store.exists(K_ALICE).await.unwrap());
    assert!(store.exists(K_BOB).await.unwrap());
    assert!(store.exists(keys::PROJECTS).await.unwrap());
    assert_eq!(c.metrics.counter_value("replay_objects_deleted_total", &[]), 1);

    // Side tables.
    let people = PersonTable::load(&store, 7, &identity).await.unwrap();
    assert!(people.person("u:alice").is_none() && people.person("a:dev-1").is_none());
    assert_eq!(people.get("u:bob", "x"), Some("1"));
    assert_eq!(identity.resolve("dev-1"), None);
    assert_eq!(identity.resolve("dev-9"), Some("zed"));
    // The expansion was frozen into an extra tombstone before the link was deleted,
    // so late or rebuilt rows of her anonymous id are still recognized.
    let tombs = tombstone::load_all(&store, 7).await.unwrap();
    assert_eq!(tombs.len(), 2);
    assert_eq!(
        (tombs[1].id.as_str(), tombs[1].ts_ms, tombs[1].anon_ids.clone()),
        ("del_1-x", ts, vec!["dev-1".to_owned()])
    );
    let set = tombstone::TombstoneSet::build(&tombs, &identity, 0);
    assert!(set.matches("a:dev-1", "dev-1", None));
    let state: TombstoneState = store.get_json(&keys::tombstone_state(7)).await.unwrap().unwrap();
    assert_eq!(state.side_ts, ts);
    assert!(c.metrics.render().contains("la_compactor_tombstones_pending 0"));
    assert!(c.metrics.counter_value("tombstoned_rows_total", &[]) >= 6);

    // Nothing left to do: another cycle writes no manifest.
    let versions = (day(&store, 7, YESTERDAY).await.version, day(&store, 7, DAY).await.version);
    let r = run::cycle(&c, open_now(), true, false, &all).await.unwrap();
    assert_eq!((r.compactions, r.tombstones_pending, r.errors), (0, 0, 0));
    assert_eq!((day(&store, 7, YESTERDAY).await.version, day(&store, 7, DAY).await.version), versions);

    // A file that slipped in without the tombstone applied (the manifest says so by
    // lowering tombstone_ts) is cleaned on the next ordinary cycle.
    add_l0(&store, 7, DAY, 4, vec![row(30, "a:dev-1", "dev-1", t0() + 9), row(31, "u:bob", "dev-2", t0() + 9)]).await;
    manifest::commit_day(&store, 7, DAY, |m| {
        m.tombstone_ts = 0;
        Ok(true)
    })
    .await
    .unwrap();
    run::cycle(&c, open_now(), false, false, &all).await.unwrap();
    let rows = inspect::day_rows(&store, 7, DAY, &identity, None).await.unwrap();
    assert!(rows.iter().all(|r| !is_alice(r)));
    assert_eq!(rows.len(), 4);
    assert_eq!(day(&store, 7, DAY).await.tombstone_ts, ts);
}

/// `tombstone_ts` only moves when every listed file is known to be clean.
#[tokio::test]
async fn tombstone_ts_does_not_advance_past_unchecked_files() {
    let store = Store::memory();
    let a = add_l0(&store, 7, DAY, 1, vec![row(1, "u:a", "d", t0())]).await;
    add_l0(&store, 7, DAY, 2, vec![row(2, "u:b", "d", t0())]).await;
    let c = ctx(&store);
    let staged = Staged::empty(&c).unwrap();
    let plan = |verified: &[&str]| CommitPlan {
        replace: Replace::Inputs,
        set_plans: Default::default(),
        tombstone: Some(TombstoneAdvance { ts: 9000, verified: verified.iter().map(|s| s.to_string()).collect() }),
    };
    // Only one of the two files was scanned: nothing may be written.
    let out = job::commit(&c, 7, DAY, &staged, &plan(&[&a])).await.unwrap();
    assert!(matches!(out, Outcome::Unchanged(_)));
    assert_eq!((day(&store, 7, DAY).await.tombstone_ts, day(&store, 7, DAY).await.version), (0, 2));
    let b = day(&store, 7, DAY).await.files[1].key.clone();
    let out = job::commit(&c, 7, DAY, &staged, &plan(&[&a, &b])).await.unwrap();
    assert!(matches!(out, Outcome::Committed { .. }));
    let m = day(&store, 7, DAY).await;
    assert_eq!((m.tombstone_ts, m.version, m.files.len()), (9000, 3, 2));
    // Never moves backwards.
    let mut lower = plan(&[&a, &b]);
    lower.tombstone.as_mut().unwrap().ts = 10;
    assert!(matches!(job::commit(&c, 7, DAY, &staged, &lower).await.unwrap(), Outcome::Unchanged(_)));
}

#[tokio::test]
async fn replay_keys_outside_the_project_prefix_are_ignored() {
    let store = Store::memory();
    for k in [
        "replay/p=7/d=2026-11-18/aa/000000.bin.zst",
        "replay/p=8/d=2026-11-18/aa/000000.bin.zst",
        "control/tokens.json",
    ] {
        store.put(k, &b"x"[..]).await.unwrap();
    }
    let keys: Vec<String> = [
        "replay/p=7/d=2026-11-18/aa/000000.bin.zst",
        "replay/p=8/d=2026-11-18/aa/000000.bin.zst",
        "replay/p=7/../../control/tokens.json",
        "control/tokens.json",
        "replay/p=7//x",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect();
    assert_eq!(tombstones::delete_replay_keys(&store, 7, &keys).await.unwrap(), 1);
    assert_eq!(all_keys(&store, "").await, ["control/tokens.json", "replay/p=8/d=2026-11-18/aa/000000.bin.zst"]);
}

#[tokio::test]
async fn old_tombstones_expire() {
    let store = Store::memory();
    let now = open_now();
    let old =
        Tombstone { id: "old".into(), ts_ms: now - 401 * DAY_MS, person_keys: vec!["u:x".into()], anon_ids: vec![] };
    let new =
        Tombstone { id: "new".into(), ts_ms: now - 399 * DAY_MS, person_keys: vec!["u:y".into()], anon_ids: vec![] };
    store.put_json(&tombstone_key(7, &old), &old).await.unwrap();
    store.put_json(&tombstone_key(7, &new), &new).await.unwrap();
    store.put_json(&keys::tombstone_state(7), &TombstoneState { side_ts: new.ts_ms }).await.unwrap();
    assert_eq!(tombstones::expire_tombstones(&ctx(&store), 7, now).await.unwrap(), 1);
    assert_eq!(tombstone::load_all(&store, 7).await.unwrap(), [new]);
    assert!(store.exists(&keys::tombstone_state(7)).await.unwrap());
}
