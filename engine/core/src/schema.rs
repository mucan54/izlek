//! `events` table schema and promoted-column bookkeeping
//! (`spec/parquet.md`, `spec/manifest.md` §5).

use crate::error::Result;
use crate::{keys, Store};
use arrow::datatypes::{DataType, Field, Schema, SchemaRef, TimeUnit};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::sync::Arc;

/// Nullable string dimension columns, in schema order (columns 9..=20).
pub const DIM_COLUMNS: [&str; 12] = [
    "platform",
    "os",
    "os_version",
    "device_model",
    "device_type",
    "app_version",
    "locale",
    "country",
    "sdk",
    "url_path",
    "referrer_host",
    "screen",
];

pub const DIM_PLATFORM: usize = 0;
pub const DIM_OS: usize = 1;
pub const DIM_OS_VERSION: usize = 2;
pub const DIM_DEVICE_MODEL: usize = 3;
pub const DIM_DEVICE_TYPE: usize = 4;
pub const DIM_APP_VERSION: usize = 5;
pub const DIM_LOCALE: usize = 6;
pub const DIM_COUNTRY: usize = 7;
pub const DIM_SDK: usize = 8;
pub const DIM_URL_PATH: usize = 9;
pub const DIM_REFERRER_HOST: usize = 10;
pub const DIM_SCREEN: usize = 11;

/// Number of columns before the first promoted column.
pub const FIXED_PREFIX: usize = 8 + DIM_COLUMNS.len() + 1;

pub const MAX_PROMOTED: usize = 200;
pub const PROMOTE_MIN_SHARE: f64 = 0.05;
pub const PROMOTE_MIN_EVENTS: u64 = 1_000;
pub const STATS_WINDOW_DAYS: usize = 7;
pub const STATS_MAX_KEYS_PER_DAY: usize = 2_000;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PropType {
    String,
    Int,
    Float,
    Bool,
}

impl PropType {
    pub fn arrow(self) -> DataType {
        match self {
            PropType::String => DataType::Utf8,
            PropType::Int => DataType::Int64,
            PropType::Float => DataType::Float64,
            PropType::Bool => DataType::Boolean,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            PropType::String => "string",
            PropType::Int => "int",
            PropType::Float => "float",
            PropType::Bool => "bool",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PromotedColumn {
    pub key: String,
    #[serde(rename = "type")]
    pub ty: PropType,
    /// Schema version that introduced the column.
    pub since: u64,
}

impl PromotedColumn {
    pub fn column_name(&self) -> String {
        promoted_column_name(&self.key)
    }
}

pub fn promoted_column_name(key: &str) -> String {
    format!("p_{key}")
}

/// Only keys matching `^[A-Za-z0-9_$]{1,64}$` may become columns.
pub fn is_promotable_key(key: &str) -> bool {
    (1..=64).contains(&key.len()) && key.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'$')
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct KeyStat {
    /// Events carrying the key.
    #[serde(default)]
    pub n: u64,
    /// Occurrences per observed type name (`string`/`int`/`float`/`bool`/`json`/`null`).
    #[serde(default)]
    pub types: BTreeMap<String, u64>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DayStats {
    #[serde(default)]
    pub events: u64,
    #[serde(default)]
    pub keys: BTreeMap<String, KeyStat>,
    /// Event name → count (drives UI autocompletion; capped like `keys`).
    #[serde(default)]
    pub names: BTreeMap<String, u64>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SchemaStats {
    #[serde(default)]
    pub days: BTreeMap<String, DayStats>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ProjectSchema {
    #[serde(default)]
    pub version: u64,
    #[serde(default)]
    pub project: u32,
    #[serde(default)]
    pub columns: Vec<PromotedColumn>,
    /// `shardkey → highest plan whose stats are already included`.
    #[serde(default)]
    pub applied: BTreeMap<String, u64>,
    #[serde(default)]
    pub stats: SchemaStats,
}

impl ProjectSchema {
    pub async fn load(store: &Store, project: u32) -> Result<Self> {
        let mut s: ProjectSchema = store.get_json(&keys::project_schema(project)).await?.unwrap_or_default();
        s.project = project;
        Ok(s)
    }

    /// Unconditional write: the project's shipper shard is the only writer.
    pub async fn save(&self, store: &Store) -> Result<()> {
        store.put_json(&keys::project_schema(self.project), self).await
    }

    pub fn column(&self, key: &str) -> Option<&PromotedColumn> {
        self.columns.iter().find(|c| c.key == key)
    }

    /// Columns as they were at `schema_version` (for reading older files).
    pub fn columns_at(&self, schema_version: u64) -> Vec<PromotedColumn> {
        self.columns.iter().filter(|c| c.since <= schema_version).cloned().collect()
    }

    /// Drops stats days outside the window ending at `today` and trims each
    /// day to the most frequent keys/names.
    pub fn trim_stats(&mut self, today: &str) {
        let keep: Vec<String> =
            self.stats.days.keys().filter(|d| d.as_str() <= today).rev().take(STATS_WINDOW_DAYS).cloned().collect();
        self.stats.days.retain(|d, _| keep.contains(d));
        for day in self.stats.days.values_mut() {
            if day.keys.len() > STATS_MAX_KEYS_PER_DAY {
                let mut by_n: Vec<(u64, String)> = day.keys.iter().map(|(k, s)| (s.n, k.clone())).collect();
                by_n.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
                let keep: std::collections::BTreeSet<String> =
                    by_n.into_iter().take(STATS_MAX_KEYS_PER_DAY).map(|(_, k)| k).collect();
                day.keys.retain(|k, _| keep.contains(k));
            }
            if day.names.len() > STATS_MAX_KEYS_PER_DAY {
                let mut by_n: Vec<(u64, String)> = day.names.iter().map(|(k, n)| (*n, k.clone())).collect();
                by_n.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
                let keep: std::collections::BTreeSet<String> =
                    by_n.into_iter().take(STATS_MAX_KEYS_PER_DAY).map(|(_, k)| k).collect();
                day.names.retain(|k, _| keep.contains(k));
            }
        }
    }

    /// Applies the promotion rule (`spec/manifest.md` §5) to the current stats
    /// window. Returns the keys that were promoted; bumps `version` if any.
    pub fn promote(&mut self) -> Vec<String> {
        let total: u64 = self.stats.days.values().map(|d| d.events).sum();
        if total < PROMOTE_MIN_EVENTS {
            return Vec::new();
        }
        type TypeCounts<'a> = BTreeMap<&'a str, u64>;
        let mut agg: BTreeMap<&str, (u64, TypeCounts<'_>)> = BTreeMap::new();
        for day in self.stats.days.values() {
            for (k, st) in &day.keys {
                let e = agg.entry(k.as_str()).or_default();
                e.0 += st.n;
                for (t, n) in &st.types {
                    *e.1.entry(t.as_str()).or_default() += n;
                }
            }
        }
        let mut candidates: Vec<(u64, String, PropType)> = Vec::new();
        for (key, (n, types)) in agg {
            if !is_promotable_key(key) || self.column(key).is_some() || n < PROMOTE_MIN_EVENTS {
                continue;
            }
            if (n as f64) < PROMOTE_MIN_SHARE * total as f64 {
                continue;
            }
            // `null` never breaks consistency; int+float counts as float.
            let seen: Vec<&str> = types.iter().filter(|(t, n)| **t != "null" && **n > 0).map(|(t, _)| *t).collect();
            let ty = match seen.as_slice() {
                ["string"] => PropType::String,
                ["int"] => PropType::Int,
                ["float"] | ["float", "int"] => PropType::Float,
                ["bool"] => PropType::Bool,
                _ => continue,
            };
            candidates.push((n, key.to_owned(), ty));
        }
        candidates.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
        let room = MAX_PROMOTED.saturating_sub(self.columns.len());
        candidates.truncate(room);
        if candidates.is_empty() {
            return Vec::new();
        }
        self.version += 1;
        let mut promoted = Vec::new();
        for (_, key, ty) in candidates {
            self.columns.push(PromotedColumn { key: key.clone(), ty, since: self.version });
            promoted.push(key);
        }
        promoted
    }
}

fn ts_type() -> DataType {
    DataType::Timestamp(TimeUnit::Millisecond, Some("UTC".into()))
}

/// Arrow schema of an events file written with the given promoted columns.
pub fn events_schema(promoted: &[PromotedColumn]) -> SchemaRef {
    let mut f = vec![
        Field::new("uuid", DataType::FixedSizeBinary(16), false),
        Field::new("ts", ts_type(), false),
        Field::new("recv_ts", ts_type(), false),
        Field::new("event", DataType::Utf8, false),
        Field::new("person_key", DataType::Utf8, false),
        Field::new("anon_id", DataType::Utf8, false),
        Field::new("user_id", DataType::Utf8, true),
        Field::new("session_id", DataType::FixedSizeBinary(16), true),
    ];
    for name in DIM_COLUMNS {
        f.push(Field::new(name, DataType::Utf8, true));
    }
    f.push(Field::new("flags", DataType::Int32, false));
    for c in promoted {
        f.push(Field::new(c.column_name(), c.ty.arrow(), true));
    }
    f.push(Field::new("props", DataType::Utf8, false));
    Arc::new(Schema::new(f))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[allow(clippy::type_complexity)]
    fn stats(events: u64, keys: &[(&str, u64, &[(&str, u64)])]) -> DayStats {
        DayStats {
            events,
            keys: keys
                .iter()
                .map(|(k, n, t)| {
                    (k.to_string(), KeyStat { n: *n, types: t.iter().map(|(a, b)| (a.to_string(), *b)).collect() })
                })
                .collect(),
            names: BTreeMap::new(),
        }
    }

    #[test]
    fn schema_layout() {
        let s = events_schema(&[PromotedColumn { key: "plan".into(), ty: PropType::String, since: 1 }]);
        assert_eq!(s.fields().len(), FIXED_PREFIX + 2);
        assert_eq!(s.field(0).name(), "uuid");
        assert_eq!(s.field(4).name(), "person_key");
        assert_eq!(s.field(8).name(), "platform");
        assert_eq!(s.field(FIXED_PREFIX - 1).name(), "flags");
        assert_eq!(s.field(FIXED_PREFIX).name(), "p_plan");
        assert_eq!(s.field(FIXED_PREFIX + 1).name(), "props");
        assert_eq!(DIM_COLUMNS[DIM_COUNTRY], "country");
        assert_eq!(DIM_COLUMNS[DIM_SCREEN], "screen");
    }

    #[test]
    fn promotion_rule() {
        let mut s = ProjectSchema { project: 1, ..Default::default() };
        s.stats.days.insert(
            "2026-11-18".into(),
            stats(
                20_000,
                &[
                    ("plan", 15_000, &[("string", 15_000)]),
                    ("amount", 4_000, &[("int", 1_000), ("float", 3_000)]),
                    ("mixed", 9_000, &[("string", 8_999), ("int", 1)]),
                    ("rare", 999, &[("string", 999)]),
                    ("below_share", 1_000, &[("string", 1_000)]),
                    ("bad key!", 15_000, &[("string", 15_000)]),
                    ("nullable", 5_000, &[("bool", 4_000), ("null", 1_000)]),
                    ("blob", 5_000, &[("json", 5_000)]),
                ],
            ),
        );
        // below_share: 1000 == 5% of 20000 → promoted (>=).
        let promoted = s.promote();
        assert_eq!(promoted, ["plan", "nullable", "amount", "below_share"]);
        assert_eq!(s.version, 1);
        assert_eq!(s.column("amount").unwrap().ty, PropType::Float);
        assert_eq!(s.column("nullable").unwrap().ty, PropType::Bool);
        assert!(s.promote().is_empty(), "idempotent");
        assert_eq!(s.version, 1);
        assert_eq!(s.columns_at(0).len(), 0);
        assert_eq!(s.columns_at(1).len(), 4);
    }

    #[test]
    fn promotion_needs_volume() {
        let mut s = ProjectSchema::default();
        s.stats.days.insert("2026-11-18".into(), stats(999, &[("plan", 999, &[("string", 999)])]));
        assert!(s.promote().is_empty());
    }

    #[test]
    fn stats_window_is_trimmed() {
        let mut s = ProjectSchema::default();
        for d in 1..=12 {
            s.stats.days.insert(format!("2026-11-{d:02}"), stats(1, &[]));
        }
        s.trim_stats("2026-11-10");
        let days: Vec<_> = s.stats.days.keys().cloned().collect();
        assert_eq!(days.len(), STATS_WINDOW_DAYS);
        assert_eq!(days.first().unwrap(), "2026-11-04");
        assert_eq!(days.last().unwrap(), "2026-11-10");
    }

    #[test]
    fn promotable_keys() {
        assert!(is_promotable_key("$lib_version"));
        assert!(!is_promotable_key(""));
        assert!(!is_promotable_key("a.b"));
        assert!(!is_promotable_key(&"x".repeat(65)));
    }
}
