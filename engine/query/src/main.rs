//! `la-query`: the query service binary. Configuration is read from the
//! environment (see `README.md`).
#![forbid(unsafe_code)]

use la_core::Store;
use la_query::config::Config;
use la_query::http::router;
use la_query::service::Service;
use la_query::state::Engine;

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut s) => {
                s.recv().await;
            }
            Err(_) => std::future::pending::<()>().await,
        }
    };
    tokio::select! {
        _ = ctrl_c => {}
        _ = terminate => {}
    }
}

fn fail(message: String) -> ! {
    tracing::error!(%message, "startup failed");
    eprintln!("la-query: {message}");
    std::process::exit(2);
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_env("LA_LOG").unwrap_or_else(|_| "info".into()),
        )
        .init();

    let cfg = Config::from_env().unwrap_or_else(|e| fail(e));
    let store = Store::from_env().unwrap_or_else(|e| fail(e.to_string()));
    let listen = cfg.listen.clone();
    tracing::info!(store = store.describe(), %listen, cache_dir = ?cfg.cache_dir, "starting la-query");
    let engine = Engine::new(store, cfg).unwrap_or_else(|e| fail(e));
    if let Err(e) = engine.refresh_control().await {
        // Not fatal: /readyz stays red and the refresher keeps trying.
        tracing::warn!(error = %e, "control files not readable yet");
    }
    let refresher = engine.spawn_refresher();
    let service = Service::new(engine);
    let listener =
        tokio::net::TcpListener::bind(&listen).await.unwrap_or_else(|e| fail(format!("bind {listen}: {e}")));
    let served = axum::serve(listener, router(service)).with_graceful_shutdown(shutdown_signal()).await;
    refresher.abort();
    if let Err(e) = served {
        fail(format!("server error: {e}"));
    }
    tracing::info!("la-query stopped");
}
