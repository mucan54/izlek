//! Store contract against a real S3 implementation (one that verifies SigV4
//! signatures and enforces `If-None-Match: *`). Skipped unless
//! `LA_TEST_S3_ENDPOINT` is set:
//!
//! ```text
//! LA_TEST_S3_ENDPOINT=http://127.0.0.1:9000 LA_TEST_S3_BUCKET=la-test \
//! LA_TEST_S3_ACCESS_KEY=… LA_TEST_S3_SECRET_KEY=… cargo test -p la-core --test s3_real
//! ```
//!
//! The bucket must exist. Every test works under its own random prefix.

use la_core::manifest::{self, DayManifest};
use la_core::store::S3Options;
use la_core::{keys, Store};
use std::time::{SystemTime, UNIX_EPOCH};

fn real_store(name: &str) -> Option<Store> {
    let get = |k: &str| std::env::var(k).ok().filter(|v| !v.is_empty());
    let endpoint = get("LA_TEST_S3_ENDPOINT")?;
    let bucket = get("LA_TEST_S3_BUCKET").expect("LA_TEST_S3_BUCKET");
    let nanos = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
    let url = format!("s3://{bucket}/rs-{nanos}-{name}/x=1");
    let opts = S3Options {
        endpoint: Some(endpoint),
        region: get("LA_TEST_S3_REGION"),
        access_key: get("LA_TEST_S3_ACCESS_KEY"),
        secret_key: get("LA_TEST_S3_SECRET_KEY"),
    };
    Some(Store::from_url(&url, opts).expect("store"))
}

#[tokio::test]
async fn basic_contract() {
    let Some(store) = real_store("basic") else { return };
    assert_eq!(store.get("a/missing").await.unwrap(), None);
    assert!(!store.exists("a/missing").await.unwrap());
    store.put("a/one.json", &b"1"[..]).await.unwrap();
    store.put("a/one.json", &b"11"[..]).await.unwrap(); // overwrite
    store.put("a/b/two=2.bin", vec![7u8; 100_000]).await.unwrap();
    store.put("ab/three", &b"3"[..]).await.unwrap();
    assert_eq!(store.get("a/one.json").await.unwrap().as_deref(), Some(&b"11"[..]));
    assert_eq!(store.get("a/b/two=2.bin").await.unwrap().unwrap().len(), 100_000);
    assert!(store.exists("ab/three").await.unwrap());

    // Listing is by path segment: "a" does not match "ab/…".
    let keys_of = |v: Vec<la_core::store::ObjInfo>| v.into_iter().map(|o| (o.key, o.size)).collect::<Vec<_>>();
    assert_eq!(
        keys_of(store.list("a").await.unwrap()),
        [("a/b/two=2.bin".to_owned(), 100_000), ("a/one.json".to_owned(), 2)]
    );
    assert_eq!(store.list("").await.unwrap().len(), 3);
    assert_eq!(store.list_dirs("a").await.unwrap(), ["b"]);
    assert!(store.list("nothing/here").await.unwrap().is_empty());

    store.delete("a/one.json").await.unwrap();
    store.delete("a/one.json").await.unwrap(); // deleting a missing key is fine
    assert_eq!(store.get("a/one.json").await.unwrap(), None);
}

#[tokio::test]
async fn put_if_absent_never_overwrites() {
    let Some(store) = real_store("absent") else { return };
    assert!(store.put_if_absent("k", &b"first"[..]).await.unwrap());
    assert!(!store.put_if_absent("k", &b"second"[..]).await.unwrap());
    assert_eq!(store.get("k").await.unwrap().as_deref(), Some(&b"first"[..]));
    // After a delete the key can be created again.
    store.delete("k").await.unwrap();
    assert!(store.put_if_absent("k", &b"third"[..]).await.unwrap());
}

/// Design §19 assumption 1: of many concurrent creators exactly one wins.
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
async fn put_if_absent_race_has_one_winner() {
    let Some(store) = real_store("race") else { return };
    for round in 0..5 {
        let key = format!("race/{round}");
        let mut tasks = Vec::new();
        for i in 0..24u8 {
            let (store, key) = (store.clone(), key.clone());
            tasks.push(tokio::spawn(async move { store.put_if_absent(&key, vec![i; 64]).await.unwrap().then_some(i) }));
        }
        let mut winners = Vec::new();
        for t in tasks {
            winners.extend(t.await.unwrap());
        }
        assert_eq!(winners.len(), 1, "round {round}: winners {winners:?}");
        // What is stored is the winner's value, in full.
        assert_eq!(store.get(&key).await.unwrap().unwrap().as_ref(), &[winners[0]; 64][..]);
    }
}

/// The manifest protocol on top of it: concurrent commits lose no update.
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
async fn concurrent_manifest_commits_all_land() {
    let Some(store) = real_store("manifest") else { return };
    const WRITERS: u64 = 12;
    let mut tasks = Vec::new();
    for i in 0..WRITERS {
        let store = store.clone();
        tasks.push(tokio::spawn(async move {
            manifest::commit_day(&store, 7, "2026-11-18", |m: &mut DayManifest| {
                m.plans.insert(format!("w{i}"), i + 1);
                Ok(true)
            })
            .await
            .unwrap()
        }));
    }
    let mut conflicts = 0;
    for t in tasks {
        conflicts += t.await.unwrap().conflicts;
    }
    let m = manifest::load_day(&store, 7, "2026-11-18").await.unwrap();
    assert_eq!(m.version, WRITERS, "one version per commit");
    assert_eq!(m.plans.len() as u64, WRITERS, "no lost update (conflicts retried: {conflicts})");
    let versions = store.list(&keys::data_manifest_dir(7, "2026-11-18")).await.unwrap();
    assert_eq!(versions.len() as u64, WRITERS);
}
