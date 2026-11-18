//! `la-difftest`: long differential runs and benchmarks.
#![forbid(unsafe_code)]

use clap::{Parser, Subcommand};
use la_difftest::runner::{run_seed, Battery, Tally};

#[derive(Parser)]
#[command(name = "la-difftest", about = "Differential tests and benchmarks for la-query")]
struct Cli {
    /// Number of seeds to run (differential mode).
    #[arg(long, default_value_t = 200)]
    seeds: u64,
    /// First seed.
    #[arg(long, default_value_t = 1)]
    start: u64,
    /// Fewer queries per seed.
    #[arg(long)]
    quick: bool,
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// Run differential tests (the default).
    Diff,
    /// Write one small generated project into a directory store (for manual
    /// testing of `la-query`) and register a query key for it.
    Gen {
        #[arg(long)]
        dir: std::path::PathBuf,
        #[arg(long, default_value_t = 1)]
        seed: u64,
        #[arg(long, default_value = "sk_demo")]
        key: String,
    },
    /// Measure the design's §19 assumptions on synthetic data.
    Bench {
        /// Events per generated project.
        #[arg(long, default_value_t = 6_000_000)]
        events: u64,
        #[arg(long, default_value_t = 20)]
        days: usize,
        #[arg(long, default_value_t = 400_000)]
        persons: u64,
        /// Directory for the generated store (created; removed afterwards unless --keep).
        #[arg(long)]
        dir: std::path::PathBuf,
        #[arg(long, default_value_t = 5)]
        runs: usize,
        #[arg(long, default_value_t = 4)]
        partitions: usize,
        /// Fractions of persons that identify late (comma separated).
        #[arg(long, default_value = "0.05,0.2")]
        identify: String,
        #[arg(long)]
        keep: bool,
    },
}

#[tokio::main]
async fn main() {
    let cli = Cli::parse();
    match cli.command {
        None | Some(Command::Diff) => {
            let battery = if cli.quick { Battery::QUICK } else { Battery::FULL };
            let started = std::time::Instant::now();
            let mut tally = Tally::default();
            for seed in cli.start..cli.start + cli.seeds {
                tally.add(&run_seed(seed, battery).await);
                if tally.seeds % 25 == 0 {
                    eprintln!("{} seeds ok ({:.1}s)", tally.seeds, started.elapsed().as_secs_f64());
                }
            }
            println!("differential run passed: {tally:?} in {:.1}s", started.elapsed().as_secs_f64());
        }
        Some(Command::Gen { dir, seed, key }) => {
            let store = la_core::Store::local(&dir).expect("store directory");
            let ds = la_difftest::gen::generate(&store, &la_difftest::gen::GenConfig::small(seed)).await;
            la_difftest::runner::register_key(&store, &key, ds.project).await;
            println!(
                "project {} in file://{}: {} logical events ({} alive), days {}..{}, key {key}\n{:?}",
                ds.project,
                dir.display(),
                ds.stats.logical_events,
                ds.rows.len(),
                ds.days[0],
                ds.days[ds.days.len() - 1],
                ds.stats
            );
        }
        Some(Command::Bench { events, days, persons, dir, runs, partitions, identify, keep }) => {
            let identify_ratios: Vec<f64> =
                identify.split(',').filter(|s| !s.is_empty()).map(|s| s.trim().parse().expect("ratio")).collect();
            let cfg = la_difftest::bench::BenchConfig {
                events,
                days,
                persons,
                dir: dir.clone(),
                runs,
                partitions,
                identify_ratios,
            };
            let report = la_difftest::bench::run(&cfg).await;
            println!("{report}");
            if !keep {
                let _ = std::fs::remove_dir_all(&dir);
            }
        }
    }
}
