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
        Some(Command::Bench { events, days, persons, dir, runs, partitions, identify, keep }) => {
            let identify_ratios: Vec<f64> = identify.split(',').filter(|s| !s.is_empty()).map(|s| s.trim().parse().expect("ratio")).collect();
            let cfg = la_difftest::bench::BenchConfig { events, days, persons, dir: dir.clone(), runs, partitions, identify_ratios };
            let report = la_difftest::bench::run(&cfg).await;
            println!("{report}");
            if !keep {
                let _ = std::fs::remove_dir_all(&dir);
            }
        }
    }
}
