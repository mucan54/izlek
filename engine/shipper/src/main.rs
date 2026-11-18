//! `la-shipper`: ship loop, compactor loop and rebuild tool in one binary.
#![forbid(unsafe_code)]

use la_compactor::run::RunOptions;
use la_core::metrics::{Registry, MANIFEST_CONFLICTS};
use la_core::Store;
use la_shipper::rebuild::{rebuild, RebuildOptions};
use la_shipper::{ship, ShipConfig, ShipCtx};
use std::sync::Arc;

const USAGE: &str = "\
la-shipper — WAL → Parquet shipper, compactor and rebuild tool

USAGE:
    la-shipper [ship] [--once]
    la-shipper compact [--once] [--no-lease]
    la-shipper rebuild --from <YYYY-MM-DD> --to <YYYY-MM-DD> [--project <id>] [--with-side-tables] [--force]
    la-shipper rebuild --abort

MODES:
    ship      (default) turn WAL segments into L0 Parquet files; --once ships what is pending and exits
    compact   L0→L1→L2 compaction, GC, retention, WAL lifecycle, person deletion; --once runs one full cycle
    rebuild   re-derive the event files of a range of event days from the WAL

Configuration is read from the environment (LA_STORE_URL, LA_S3_*, LA_SHIPPER_*, LA_COMPACTOR_*,
LA_METRICS_ADDR, LA_LOG); see engine/shipper/README.md.";

fn init_logging() {
    let filter = std::env::var("LA_LOG").ok().filter(|v| !v.is_empty()).unwrap_or_else(|| "info".to_owned());
    let filter = tracing_subscriber::EnvFilter::try_new(&filter).unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    // Structured JSON, one object per line. Event content is never logged:
    // log fields are identifiers, counts and object keys only.
    let _ = tracing_subscriber::fmt().json().with_env_filter(filter).with_writer(std::io::stdout).try_init();
}

fn holder(prefix: &str) -> String {
    if let Some(id) = std::env::var("LA_NODE_ID").ok().filter(|v| !v.is_empty()) {
        return id;
    }
    let host = std::env::var("HOSTNAME")
        .ok()
        .or_else(|| std::fs::read_to_string("/etc/hostname").ok())
        .map(|h| h.trim().to_owned())
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "node".to_owned());
    format!("{prefix}-{host}-{}", std::process::id())
}

async fn start_metrics(registry: Arc<Registry>, default_addr: &str) {
    let addr = std::env::var("LA_METRICS_ADDR").ok().filter(|v| !v.is_empty()).unwrap_or_else(|| default_addr.to_owned());
    if addr == "off" {
        return;
    }
    registry.register_counter("manifest_conflicts_total", &MANIFEST_CONFLICTS);
    match la_shipper::http::serve(&addr, registry).await {
        Ok(local) => tracing::info!(addr = %local, "metrics listener up"),
        Err(e) => tracing::error!(addr = %addr, error = %e, "metrics listener could not start"),
    }
}

fn shutdown_signal() -> tokio::sync::watch::Receiver<bool> {
    let (tx, rx) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        let ctrl_c = tokio::signal::ctrl_c();
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            match signal(SignalKind::terminate()) {
                Ok(mut term) => {
                    tokio::select! { _ = ctrl_c => {}, _ = term.recv() => {} }
                }
                Err(_) => {
                    let _ = ctrl_c.await;
                }
            }
        }
        #[cfg(not(unix))]
        {
            let _ = ctrl_c.await;
        }
        let _ = tx.send(true);
    });
    rx
}

struct Args {
    mode: String,
    flags: Vec<String>,
    values: std::collections::HashMap<String, String>,
}

fn parse_args() -> Result<Args, String> {
    let mut it = std::env::args().skip(1).peekable();
    let mode = match it.peek() {
        Some(a) if !a.starts_with('-') => it.next().unwrap_or_default(),
        _ => "ship".to_owned(),
    };
    let mut args = Args { mode, flags: Vec::new(), values: Default::default() };
    while let Some(a) = it.next() {
        match a.as_str() {
            "--once" | "--no-lease" | "--with-side-tables" | "--force" | "--abort" | "--help" | "-h" => args.flags.push(a),
            "--from" | "--to" | "--project" => {
                let v = it.next().ok_or_else(|| format!("{a} needs a value"))?;
                args.values.insert(a, v);
            }
            other => return Err(format!("unknown argument {other:?}")),
        }
    }
    Ok(args)
}

async fn run(args: Args) -> Result<(), String> {
    let has = |f: &str| args.flags.iter().any(|a| a == f);
    let store = Store::from_env().map_err(|e| e.to_string())?;
    match args.mode.as_str() {
        "ship" => {
            let mut ctx = ShipCtx::new(store, ShipConfig::from_env());
            ctx.holder = holder("ship");
            if has("--once") {
                let plans = ship::ship_once(&ctx).await.map_err(|e| e.to_string())?;
                tracing::info!(plans = plans.len(), records = plans.iter().map(|p| p.records_out).sum::<u64>(), "ship --once done");
                return Ok(());
            }
            start_metrics(ctx.metrics.clone(), "0.0.0.0:9102").await;
            ship::run(ctx, shutdown_signal()).await.map_err(|e| e.to_string())
        }
        "compact" => {
            let mut ctx = la_compactor::Ctx::new(store, la_compactor::CompactorConfig::from_env());
            ctx.holder = holder("compact");
            let shards = std::env::var("LA_COMPACTOR_SHARDS").ok().filter(|v| !v.is_empty() && v != "*").map(|v| {
                v.split(',').filter_map(|s| s.trim().parse().ok()).collect::<Vec<u32>>()
            });
            let opts = RunOptions { once: has("--once"), shards, no_lease: has("--no-lease") };
            if !opts.once {
                start_metrics(ctx.metrics.clone(), "0.0.0.0:9103").await;
            }
            la_compactor::run::run(ctx, opts, shutdown_signal()).await.map_err(|e| e.to_string())
        }
        "rebuild" => {
            let mut ctx = ShipCtx::new(store.clone(), ShipConfig::from_env());
            ctx.holder = holder("rebuild");
            let cctx = la_compactor::Ctx::new(store, la_compactor::CompactorConfig::from_env());
            let project = match args.values.get("--project") {
                Some(p) => Some(p.parse::<u32>().map_err(|_| "--project needs a numeric project id".to_owned())?),
                None => None,
            };
            let opts = RebuildOptions {
                from: args.values.get("--from").cloned().unwrap_or_default(),
                to: args.values.get("--to").cloned().unwrap_or_default(),
                project,
                with_side_tables: has("--with-side-tables"),
                abort: has("--abort"),
                force: has("--force"),
            };
            let report = rebuild(&ctx, &cctx, &opts).await.map_err(|e| e.to_string())?;
            tracing::info!(
                days = report.days_rebuilt.len(),
                untouched = report.days_untouched.len(),
                rows = report.rows,
                segments = report.segments,
                aborted = report.aborted,
                "rebuild finished"
            );
            Ok(())
        }
        other => Err(format!("unknown mode {other:?}\n\n{USAGE}")),
    }
}

fn main() {
    let args = match parse_args() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("{e}\n\n{USAGE}");
            std::process::exit(2);
        }
    };
    if args.mode == "help" || args.flags.iter().any(|f| f == "--help" || f == "-h") {
        println!("{USAGE}");
        return;
    }
    init_logging();
    let runtime = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("cannot start the runtime: {e}");
            std::process::exit(1);
        }
    };
    if let Err(e) = runtime.block_on(run(args)) {
        tracing::error!(error = %e, "fatal");
        eprintln!("la-shipper: {e}");
        std::process::exit(1);
    }
}
