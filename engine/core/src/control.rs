//! Control-plane snapshots read by the engine (`spec/manifest.md` §8).

use crate::error::Result;
use crate::{keys, Store};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;

fn d_retention() -> u32 {
    365
}
fn d_replay_retention() -> u32 {
    30
}
fn d_concurrency() -> u32 {
    4
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ProjectConfig {
    #[serde(default)]
    pub name: String,
    #[serde(default = "d_retention")]
    pub retention_days: u32,
    #[serde(default = "d_replay_retention")]
    pub replay_retention_days: u32,
    /// Property keys dropped at write time (PII block list).
    #[serde(default)]
    pub drop_keys: Vec<String>,
    /// 0 = unlimited.
    #[serde(default)]
    pub scan_limit_bytes: u64,
    #[serde(default = "d_concurrency")]
    pub max_concurrent_queries: u32,
}

impl Default for ProjectConfig {
    fn default() -> Self {
        Self {
            name: String::new(),
            retention_days: d_retention(),
            replay_retention_days: d_replay_retention(),
            drop_keys: Vec::new(),
            scan_limit_bytes: 0,
            max_concurrent_queries: d_concurrency(),
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Projects {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub projects: BTreeMap<String, ProjectConfig>,
}

impl Projects {
    pub async fn load(store: &Store) -> Result<Self> {
        Ok(store.get_json(keys::PROJECTS).await?.unwrap_or_default())
    }

    pub fn get(&self, project: u32) -> Option<&ProjectConfig> {
        self.projects.get(&project.to_string())
    }

    /// Config of a project, or defaults when the control plane has not
    /// published it (data-plane components must keep working without it).
    pub fn get_or_default(&self, project: u32) -> ProjectConfig {
        self.get(project).cloned().unwrap_or_default()
    }

    pub fn ids(&self) -> Vec<u32> {
        let mut v: Vec<u32> = self.projects.keys().filter_map(|k| k.parse().ok()).collect();
        v.sort_unstable();
        v
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct QueryKey {
    pub project_id: u32,
    #[serde(default)]
    pub scopes: Vec<String>,
}

/// `control/keys.json`: secret keys are stored only as SHA-256 hex.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Keys {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub keys: BTreeMap<String, QueryKey>,
}

pub fn sha256_hex(s: &str) -> String {
    Sha256::digest(s.as_bytes()).iter().map(|b| format!("{b:02x}")).collect()
}

impl Keys {
    pub async fn load(store: &Store) -> Result<Self> {
        Ok(store.get_json(keys::KEYS).await?.unwrap_or_default())
    }

    /// Looks up a presented `sk_…` secret.
    pub fn lookup(&self, secret: &str) -> Option<&QueryKey> {
        if !secret.starts_with("sk_") {
            return None;
        }
        self.keys.get(&sha256_hex(secret))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct ShardMap {
    pub version: u32,
    #[serde(default)]
    pub from_ms: i64,
    pub shards: u32,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Shards {
    #[serde(default)]
    pub maps: Vec<ShardMap>,
}

impl Default for Shards {
    fn default() -> Self {
        Self { maps: vec![ShardMap { version: 1, from_ms: 0, shards: 1 }] }
    }
}

impl Shards {
    pub async fn load(store: &Store) -> Result<Self> {
        let s: Option<Shards> = store.get_json(keys::SHARDS).await?;
        Ok(match s {
            Some(s) if !s.maps.is_empty() => s,
            _ => Shards::default(),
        })
    }

    /// The highest-version map with `from_ms <= t_ms` (the lowest version if
    /// `t_ms` predates all of them).
    pub fn map_at(&self, t_ms: i64) -> ShardMap {
        self.maps
            .iter()
            .filter(|m| m.from_ms <= t_ms)
            .max_by_key(|m| m.version)
            .or_else(|| self.maps.iter().min_by_key(|m| m.version))
            .copied()
            .unwrap_or(ShardMap { version: 1, from_ms: 0, shards: 1 })
    }

    /// `"<map version>-<shard>"` of `project` for a segment closed at `t_ms`.
    pub fn shardkey(&self, project: u32, t_ms: i64) -> String {
        let m = self.map_at(t_ms);
        format!("{}-{}", m.version, la_codec::murmur3::shard_of(project, m.shards))
    }

    /// Every shardkey a worker for `shard` of map `version` may own.
    pub fn all_shardkeys(&self) -> Vec<String> {
        let mut out = Vec::new();
        for m in &self.maps {
            for s in 0..m.shards.max(1) {
                out.push(format!("{}-{}", m.version, s));
            }
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn missing_files_mean_defaults() {
        let s = Store::memory();
        assert_eq!(Shards::load(&s).await.unwrap().shardkey(1017, 0), "1-0");
        let p = Projects::load(&s).await.unwrap();
        assert_eq!(p.get_or_default(5).retention_days, 365);
        assert!(Keys::load(&s).await.unwrap().lookup("sk_x").is_none());
    }

    #[tokio::test]
    async fn spec_examples_parse() {
        let s = Store::memory();
        s.put(keys::PROJECTS, &br#"{ "version": 5, "projects": { "1017": { "name": "Acme", "retention_days": 90,
            "replay_retention_days": 30, "drop_keys": ["email"], "scan_limit_bytes": 0, "max_concurrent_queries": 4 } } }"#[..]).await.unwrap();
        let p = Projects::load(&s).await.unwrap();
        assert_eq!(p.get(1017).unwrap().drop_keys, ["email"]);
        assert_eq!(p.get(1017).unwrap().retention_days, 90);
        assert_eq!(p.ids(), [1017]);

        let hash = sha256_hex("sk_secret");
        s.put(
            keys::KEYS,
            format!(r#"{{"version":3,"keys":{{"{hash}":{{"project_id":1017,"scopes":["query"]}}}}}}"#).into_bytes(),
        )
        .await
        .unwrap();
        let k = Keys::load(&s).await.unwrap();
        assert_eq!(k.lookup("sk_secret").unwrap().project_id, 1017);
        assert!(k.lookup("sk_other").is_none());
        assert!(k.lookup(&hash).is_none(), "the hash itself is not a credential");
    }

    #[test]
    fn shard_maps_switch_by_time() {
        let s = Shards {
            maps: vec![
                ShardMap { version: 1, from_ms: 0, shards: 1 },
                ShardMap { version: 2, from_ms: 1000, shards: 4 },
            ],
        };
        assert_eq!(s.shardkey(1017, 999), "1-0");
        let k = s.shardkey(1017, 1000);
        assert!(k.starts_with("2-"), "{k}");
        assert_eq!(k, format!("2-{}", la_codec::murmur3::murmur3_32(b"1017", 0) % 4));
        assert_eq!(s.all_shardkeys(), ["1-0", "2-0", "2-1", "2-2", "2-3"]);
        // Before the first map: fall back to the oldest.
        let late = Shards { maps: vec![ShardMap { version: 3, from_ms: 500, shards: 2 }] };
        assert!(late.shardkey(1, 0).starts_with("3-"));
    }
}
