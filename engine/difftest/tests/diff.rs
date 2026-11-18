//! Fast differential subset for `cargo test`. Long runs: `la-difftest --seeds N`.

use la_difftest::runner::{run_seed, Battery, Tally};

async fn seeds(range: std::ops::Range<u64>) -> Tally {
    let mut tally = Tally::default();
    for seed in range {
        tally.add(&run_seed(seed, Battery::QUICK).await);
    }
    tally
}

// Four shards so the seeds run on all cores.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn differential_seeds_a() {
    let t = seeds(1..11).await;
    assert_eq!(t.seeds, 10);
    assert!(t.funnels >= 120 && t.retentions >= 30 && t.trends >= 40 && t.event_walks >= 30, "{t:?}");
    // The generator really produced the physical situations the suite is about.
    assert!(t.physical_rows > t.logical_events, "duplicates across files: {t:?}");
    assert!(t.duplicate_copies > 0 && t.late_events > 0 && t.identity_links > 0, "{t:?}");
    assert!(t.anonymous_rows_at_write > 0 && t.tombstoned_rows > 0 && t.compacted_days > 0, "{t:?}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn differential_seeds_b() {
    seeds(11..21).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn differential_seeds_c() {
    seeds(21..31).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn differential_seeds_d() {
    seeds(31..41).await;
}
