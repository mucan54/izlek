//! `/q/schema` and `/q/replay` (`spec/query.md` §8, `spec/replay.md` §5).

use crate::error::{ApiError, ApiResult};
use crate::state::{Engine, Snapshot};
use bytes::Bytes;
use la_core::schema::ProjectSchema;
use serde_json::{json, Value};
use std::collections::{BTreeMap, BTreeSet};

const MAX_NAMES: usize = 1_000;
const MAX_PROPS: usize = 2_000;

/// Autocomplete data from the last days of shipper schema statistics.
pub fn schema_json(schema: &ProjectSchema, snap: &Snapshot) -> Value {
    let mut names: BTreeMap<&str, u64> = BTreeMap::new();
    let mut keys: BTreeMap<&str, (u64, BTreeMap<&str, u64>)> = BTreeMap::new();
    for day in schema.stats.days.values() {
        for (name, n) in &day.names {
            *names.entry(name).or_default() += n;
        }
        for (key, st) in &day.keys {
            let e = keys.entry(key).or_default();
            e.0 += st.n;
            for (t, n) in &st.types {
                *e.1.entry(t).or_default() += n;
            }
        }
    }
    let mut events: Vec<(&str, u64)> = names.into_iter().collect();
    events.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(b.0)));
    events.truncate(MAX_NAMES);

    // Promoted columns are always listed, even if the stats window forgot them.
    for c in &schema.columns {
        keys.entry(c.key.as_str()).or_default();
    }
    let mut props: Vec<(u64, &str, String, bool)> = keys
        .into_iter()
        .map(|(key, (n, types))| match schema.column(key) {
            Some(c) => (n, key, c.ty.as_str().to_owned(), true),
            None => {
                let dominant = types.iter().filter(|(t, _)| **t != "null").max_by_key(|(t, c)| (**c, std::cmp::Reverse(**t)));
                (n, key, dominant.map_or("string", |(t, _)| *t).to_owned(), false)
            }
        })
        .collect();
    props.sort_by(|a, b| b.0.cmp(&a.0).then(a.1.cmp(b.1)));
    props.truncate(MAX_PROPS);

    let mut person_props: BTreeSet<&str> = BTreeSet::new();
    if let Some(table) = &snap.persons {
        let hidden = snap.tombstone_set(0).person_keys;
        for (_, p) in table.iter().filter(|(k, _)| !hidden.contains(*k)) {
            for k in p.keys() {
                if person_props.len() < MAX_NAMES || person_props.contains(k.as_str()) {
                    person_props.insert(k);
                }
            }
        }
    }
    json!({
        "events": events.iter().map(|(n, _)| *n).collect::<Vec<_>>(),
        "props": props.iter().map(|(_, k, t, p)| json!({ "key": k, "type": t, "promoted": p })).collect::<Vec<_>>(),
        "person_props": person_props,
    })
}

/// `Content-Encoding` of a replay chunk, from its key.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReplayEncoding {
    Zstd,
    Gzip,
}

impl ReplayEncoding {
    pub fn header(self) -> &'static str {
        match self {
            ReplayEncoding::Zstd => "zstd",
            ReplayEncoding::Gzip => "gzip",
        }
    }
}

fn is_hex32(s: &str) -> bool {
    s.len() == 32 && s.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// Validates a replay object key for `project`. A key outside the project's
/// prefix is `403`; anything else that is not exactly
/// `replay/p=<pid>/d=<day>/<session_hex32>/<seq:06>.bin.{zst|gz}` is `400`.
pub fn check_replay_key(project: u32, key: &str) -> ApiResult<ReplayEncoding> {
    let prefix = format!("replay/p={project}/");
    let Some(rest) = key.strip_prefix(&prefix) else {
        return Err(ApiError::forbidden("replay key does not belong to this project"));
    };
    let bad = || ApiError::bad_request("malformed replay key");
    let parts: Vec<&str> = rest.split('/').collect();
    let [day, session, file] = parts.as_slice() else { return Err(bad()) };
    let day = day.strip_prefix("d=").ok_or_else(bad)?;
    if la_core::time::parse_day(day).is_none() || !is_hex32(session) {
        return Err(bad());
    }
    let (seq, encoding) = if let Some(s) = file.strip_suffix(".bin.zst") {
        (s, ReplayEncoding::Zstd)
    } else if let Some(s) = file.strip_suffix(".bin.gz") {
        (s, ReplayEncoding::Gzip)
    } else {
        return Err(bad());
    };
    if seq.len() != 6 || !seq.bytes().all(|b| b.is_ascii_digit()) {
        return Err(bad());
    }
    Ok(encoding)
}

/// The raw (still compressed) chunk.
pub async fn replay_chunk(engine: &Engine, project: u32, key: &str) -> ApiResult<(Bytes, ReplayEncoding)> {
    let encoding = check_replay_key(project, key)?;
    match engine.store.get(key).await? {
        Some(bytes) => Ok((bytes, encoding)),
        None => Err(ApiError::not_found("no such replay chunk")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn replay_keys() {
        let s = "0123456789abcdef0123456789abcdef";
        let ok = format!("replay/p=7/d=2026-11-18/{s}/000003.bin.zst");
        assert_eq!(check_replay_key(7, &ok).unwrap(), ReplayEncoding::Zstd);
        assert_eq!(check_replay_key(7, &ok.replace(".zst", ".gz")).unwrap(), ReplayEncoding::Gzip);
        // Someone else's prefix, or no prefix at all.
        for key in [
            ok.replace("p=7", "p=8"),
            ok.replace("p=7", "p=70"),
            format!("/{ok}"),
            "data/p=7/d=2026-11-18/L2-00000001-000.parquet".to_owned(),
            "control/keys.json".to_owned(),
            String::new(),
            format!("replay/p=07/d=2026-11-18/{s}/000003.bin.zst"),
        ] {
            assert_eq!(check_replay_key(7, &key).unwrap_err().status, 403, "{key}");
        }
        // Inside the prefix but not a chunk key (traversal, extra segments…).
        for key in [
            format!("replay/p=7/../p=8/d=2026-11-18/{s}/000003.bin.zst"),
            format!("replay/p=7/d=2026-11-18/../../p=8/d=2026-11-18/{s}/000003.bin.zst"),
            format!("replay/p=7/d=2026-11-18/{s}/../../../../control/keys.json"),
            format!("replay/p=7/d=2026-11-18/{s}/000003.bin"),
            format!("replay/p=7/d=2026-11-18/{s}/3.bin.zst"),
            format!("replay/p=7/d=2026-11-18/{s}/000003.bin.zst/x"),
            format!("replay/p=7/d=2026-11-18/{}/000003.bin.zst", s.to_uppercase()),
            format!("replay/p=7/d=2026-13-40/{s}/000003.bin.zst"),
            format!("replay/p=7/d=2026-11-18/{s}/00000%33.bin.zst"),
            format!("replay/p=7//d=2026-11-18/{s}/000003.bin.zst"),
            "replay/p=7/".to_owned(),
        ] {
            assert_eq!(check_replay_key(7, &key).unwrap_err().status, 400, "{key}");
        }
    }
}
