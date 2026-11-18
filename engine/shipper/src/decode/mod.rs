//! WAL entry → decoded records (`spec/wal.md` §2–§3, `spec/parquet.md` §2).
//!
//! Decoding is a pure function of the entry and the project's `drop_keys`:
//! it never looks at the identity map or the promoted schema, so re-executing
//! a plan decodes to exactly the same records.

mod native;
mod posthog;
mod replay;

use crate::props::PropList;
use la_codec::wal::{Envelope, FORMAT_NATIVE, FORMAT_POSTHOG, FORMAT_REPLAY_INDEX};
use la_codec::wire::{MAX_KEY_BYTES, MAX_PROPS, MAX_VALUE_BYTES};
use la_core::identity::Candidate;
use la_core::persons::PropWrite;
use la_core::schema::DIM_COLUMNS;
use std::collections::{BTreeMap, HashSet};

pub use posthog::parse_timestamp;

// `records_rejected_total{reason}` values (`spec/parquet.md` §2).
pub const EMPTY_NAME: &str = "empty_name";
pub const EMPTY_ID: &str = "empty_id";
pub const TOO_LONG: &str = "too_long";
pub const TOO_MANY_PROPS: &str = "too_many_props";
pub const BAD_UTF8: &str = "bad_utf8";
pub const BAD_RECORD: &str = "bad_record";
pub const BOT: &str = "bot";
pub const UNKNOWN_PROJECT: &str = "unknown_project";

/// Case-insensitive substrings of a web user agent that mark a bot.
pub const BOT_PATTERNS: [&str; 7] =
    ["bot", "crawler", "spider", "headlesschrome", "lighthouse", "pingdom", "phantomjs"];

pub fn is_bot_ua(ua: &str) -> bool {
    let ua = ua.to_ascii_lowercase();
    BOT_PATTERNS.iter().any(|p| ua.contains(p))
}

/// An event before identity resolution and promoted-column splitting.
#[derive(Debug, Clone, PartialEq)]
pub struct RawEvent {
    pub uuid: [u8; 16],
    pub ts: i64,
    pub recv_ts: i64,
    pub name: String,
    pub anon_id: String,
    pub user_id: Option<String>,
    pub session_id: Option<[u8; 16]>,
    pub dims: [Option<String>; DIM_COLUMNS.len()],
    pub flags: i32,
    /// Sorted by key, unique.
    pub props: PropList,
    /// Platform-generated rows (replay chunk index): kept out of the schema
    /// statistics and never split into promoted columns.
    pub synthetic: bool,
}

/// Everything one project contributes to a plan, in WAL order.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct ProjectBatch {
    pub events: Vec<RawEvent>,
    pub links: Vec<Candidate>,
    pub writes: Vec<PropWrite>,
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct DecodeStats {
    pub rejected: BTreeMap<&'static str, u64>,
    /// Properties dropped for exceeding the value size limit.
    pub props_dropped: u64,
    pub unknown_format: u64,
    pub unknown_kinds: u64,
    pub resets: u64,
}

impl DecodeStats {
    pub fn reject(&mut self, reason: &'static str, n: u64) {
        if n > 0 {
            *self.rejected.entry(reason).or_default() += n;
        }
    }

    pub fn rejected_total(&self) -> u64 {
        self.rejected.values().sum()
    }

    pub fn merge(&mut self, other: &DecodeStats) {
        for (k, v) in &other.rejected {
            self.reject(k, *v);
        }
        self.props_dropped += other.props_dropped;
        self.unknown_format += other.unknown_format;
        self.unknown_kinds += other.unknown_kinds;
        self.resets += other.resets;
    }
}

/// Applies the per-record limits to an already key-sorted property list:
/// `Err(reason)` rejects the record, oversized values and `drop_keys` are
/// removed from the list.
pub(crate) fn enforce_limits(
    props: &mut PropList,
    drop_keys: &HashSet<String>,
    st: &mut DecodeStats,
) -> Result<(), &'static str> {
    if props.iter().any(|(k, _)| k.len() > MAX_KEY_BYTES) {
        return Err(TOO_LONG);
    }
    let before = props.len();
    props.retain(|(_, v)| v.limited_len() <= MAX_VALUE_BYTES);
    st.props_dropped += (before - props.len()) as u64;
    if !drop_keys.is_empty() {
        props.retain(|(k, _)| !drop_keys.contains(k));
    }
    Ok(())
}

pub(crate) fn too_many(n: usize) -> bool {
    n > MAX_PROPS
}

pub(crate) fn person_writes(out: &mut Vec<PropWrite>, person_key: &str, props: PropList, ts: i64) {
    for (key, value) in props {
        out.push(PropWrite { person_key: person_key.to_owned(), key, value: value.to_json(), ts, seq: 0 });
    }
}

/// `referrer` → host part if it parses as a URL, otherwise the value itself
/// (cut to 255 bytes on a character boundary).
pub(crate) fn referrer_host(value: &str) -> String {
    let host = url::Url::parse(value).ok().and_then(|u| u.host_str().map(str::to_owned));
    let mut s = host.unwrap_or_else(|| value.to_owned());
    if s.len() > 255 {
        let mut cut = 255;
        while !s.is_char_boundary(cut) {
            cut -= 1;
        }
        s.truncate(cut);
    }
    s
}

pub(crate) fn recv_ts(env: &Envelope) -> i64 {
    i64::try_from(env.recv_ts_ms).unwrap_or(i64::MAX)
}

pub(crate) fn non_empty(s: &str) -> Option<String> {
    (!s.is_empty()).then(|| s.to_owned())
}

/// Decodes one WAL entry of a project into `out`.
pub fn decode_entry(
    env: &Envelope,
    body: &[u8],
    drop_keys: &HashSet<String>,
    out: &mut ProjectBatch,
    st: &mut DecodeStats,
) {
    match env.format {
        FORMAT_NATIVE => native::decode(env, body, drop_keys, out, st),
        FORMAT_POSTHOG => posthog::decode(env, body, drop_keys, out, st),
        FORMAT_REPLAY_INDEX => replay::decode(env, body, out, st),
        _ => st.unknown_format += 1,
    }
}
