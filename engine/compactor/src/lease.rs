//! Keeps a lease renewed in the background while long work runs.

use la_core::lease::Lease;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

pub struct LeaseKeeper {
    held: Arc<AtomicBool>,
    task: tokio::task::JoinHandle<()>,
}

impl LeaseKeeper {
    /// Tries to take the lease once, then keeps renewing (or re-trying) every
    /// `every` until dropped.
    pub async fn start(lease: Lease, every: Duration) -> Self {
        let held = Arc::new(AtomicBool::new(false));
        let attempt = |lease: Lease, held: Arc<AtomicBool>| async move {
            match lease.try_hold(la_core::time::now_ms()).await {
                Ok(h) => held.store(h, Ordering::Relaxed),
                Err(e) => {
                    // Keep the previous state: a transient store error must not
                    // flap ownership, the TTL bounds how long that can last.
                    tracing::warn!(error = %e, "lease renewal failed");
                }
            }
        };
        attempt(lease.clone(), held.clone()).await;
        let h = held.clone();
        let task = tokio::spawn(async move {
            loop {
                tokio::time::sleep(every).await;
                attempt(lease.clone(), h.clone()).await;
            }
        });
        Self { held, task }
    }

    pub fn held(&self) -> bool {
        self.held.load(Ordering::Relaxed)
    }
}

impl Drop for LeaseKeeper {
    fn drop(&mut self) {
        self.task.abort();
    }
}
