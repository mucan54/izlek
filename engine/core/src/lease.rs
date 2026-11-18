//! Epoch-chained leases built on create-if-absent (`spec/manifest.md` §7).
//! A lease only avoids duplicated work; correctness never depends on it.

use crate::error::Result;
use crate::{keys, Store};
use serde::{Deserialize, Serialize};

pub const DEFAULT_TTL_MS: i64 = 90_000;
pub const RENEW_EVERY_MS: u64 = 30_000;
const KEEP_EPOCHS: usize = 5;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LeaseRecord {
    pub epoch: u64,
    pub holder: String,
    pub acquired_at: i64,
    pub ttl_ms: i64,
}

#[derive(Debug, Clone)]
pub struct Lease {
    store: Store,
    name: String,
    holder: String,
    ttl_ms: i64,
}

fn epoch_of(key: &str) -> Option<u64> {
    let name = key.rsplit('/').next()?.strip_suffix(".json")?;
    (name.len() == 12).then(|| name.parse().ok()).flatten()
}

impl Lease {
    pub fn new(store: Store, name: &str, holder: &str) -> Self {
        Self { store, name: name.to_owned(), holder: holder.to_owned(), ttl_ms: DEFAULT_TTL_MS }
    }

    pub fn with_ttl_ms(mut self, ttl_ms: i64) -> Self {
        self.ttl_ms = ttl_ms;
        self
    }

    pub fn holder(&self) -> &str {
        &self.holder
    }

    /// The newest epoch record, if any.
    pub async fn current(&self) -> Result<Option<LeaseRecord>> {
        let objs = self.store.list(&keys::lease_dir(&self.name)).await?;
        let Some(max) = objs.iter().filter_map(|o| epoch_of(&o.key)).max() else {
            return Ok(None);
        };
        self.store.get_json(&keys::lease_epoch(&self.name, max)).await
    }

    /// Acquires or renews at time `now_ms`. Returns `true` while this holder
    /// owns the lease. Call every [`RENEW_EVERY_MS`].
    pub async fn try_hold(&self, now_ms: i64) -> Result<bool> {
        let cur = self.current().await?;
        let next = match &cur {
            None => 1,
            Some(r) if r.holder == self.holder => r.epoch + 1,
            Some(r) if r.acquired_at + r.ttl_ms < now_ms => r.epoch + 1,
            Some(_) => return Ok(false),
        };
        let rec = LeaseRecord { epoch: next, holder: self.holder.clone(), acquired_at: now_ms, ttl_ms: self.ttl_ms };
        if !self.store.put_json_if_absent(&keys::lease_epoch(&self.name, next), &rec).await? {
            return Ok(false);
        }
        // Best-effort cleanup of old epochs.
        let objs = self.store.list(&keys::lease_dir(&self.name)).await.unwrap_or_default();
        let mut epochs: Vec<u64> = objs.iter().filter_map(|o| epoch_of(&o.key)).collect();
        epochs.sort_unstable();
        if epochs.len() > KEEP_EPOCHS {
            for e in &epochs[..epochs.len() - KEEP_EPOCHS] {
                let _ = self.store.delete(&keys::lease_epoch(&self.name, *e)).await;
            }
        }
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn takeover_only_after_expiry() {
        let s = Store::memory();
        let a = Lease::new(s.clone(), "shipper-1-0", "a").with_ttl_ms(1000);
        let b = Lease::new(s.clone(), "shipper-1-0", "b").with_ttl_ms(1000);
        assert!(a.try_hold(10_000).await.unwrap());
        assert!(!b.try_hold(10_500).await.unwrap(), "still held");
        assert!(a.try_hold(10_600).await.unwrap(), "renewal");
        assert!(!b.try_hold(11_600).await.unwrap(), "expires strictly after acquired_at + ttl");
        assert!(b.try_hold(11_601).await.unwrap(), "takeover after expiry");
        assert!(!a.try_hold(11_700).await.unwrap(), "old holder notices it lost the lease");
        assert_eq!(b.current().await.unwrap().unwrap().holder, "b");
        // Other lease names are independent.
        assert!(Lease::new(s, "compactor-1-0", "a").try_hold(0).await.unwrap());
    }

    #[tokio::test]
    async fn only_one_of_many_contenders_wins_an_epoch() {
        let s = Store::memory();
        let mut tasks = Vec::new();
        for i in 0..16 {
            let l = Lease::new(s.clone(), "x", &format!("h{i}"));
            tasks.push(tokio::spawn(async move { l.try_hold(1).await.unwrap() }));
        }
        let mut winners = 0;
        for t in tasks {
            winners += t.await.unwrap() as u32;
        }
        assert_eq!(winners, 1);
    }

    #[tokio::test]
    async fn old_epochs_are_pruned() {
        let s = Store::memory();
        let l = Lease::new(s.clone(), "x", "h");
        for i in 0..20 {
            assert!(l.try_hold(i).await.unwrap());
        }
        assert_eq!(s.list(&keys::lease_dir("x")).await.unwrap().len(), KEEP_EPOCHS);
        assert_eq!(l.current().await.unwrap().unwrap().epoch, 20);
    }
}
