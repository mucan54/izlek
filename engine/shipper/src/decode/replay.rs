//! `format = 3`: replay index entries → `session_replay_chunk` rows
//! (`spec/wal.md` §2.3).

use super::*;
use crate::props::PropVal;
use la_codec::clock::{self, FLAG_WEB};
use la_codec::ids::replay_chunk_uuid;
use la_codec::wal::ReplayIndex;
use la_codec::wire::MAX_ID_BYTES;
use la_codec::DecodeError;
use la_core::schema::DIM_COUNTRY;

pub const REPLAY_CHUNK_EVENT: &str = la_compactor::merge::REPLAY_CHUNK_EVENT;

pub(super) fn decode(env: &Envelope, body: &[u8], out: &mut ProjectBatch, st: &mut DecodeStats) {
    let idx = match ReplayIndex::decode(body) {
        Ok(i) => i,
        Err(e) => {
            st.reject(if e == DecodeError::Utf8 { BAD_UTF8 } else { BAD_RECORD }, 1);
            return;
        }
    };
    let anon_id = if idx.anon_id.is_empty() { idx.user_id.clone() } else { idx.anon_id.clone() };
    if anon_id.is_empty() {
        st.reject(EMPTY_ID, 1);
        return;
    }
    if anon_id.len() > MAX_ID_BYTES || idx.user_id.len() > MAX_ID_BYTES {
        st.reject(TOO_LONG, 1);
        return;
    }
    let recv = recv_ts(env);
    // Only the "clamp future" and "late" steps apply: there is no sent_at.
    let (ts, flags) = clock::clamp(recv, i64::try_from(idx.start_ts).unwrap_or(i64::MAX));
    let int = |v: u64| PropVal::Int(i64::try_from(v).unwrap_or(i64::MAX));
    // Already in key order.
    let props = vec![
        ("bytes".to_owned(), int(idx.bytes)),
        ("duration_ms".to_owned(), int(idx.duration_ms)),
        ("encoding".to_owned(), PropVal::Int(idx.encoding as i64)),
        ("key".to_owned(), PropVal::Str(idx.key.clone())),
        ("seq".to_owned(), int(idx.seq)),
    ];
    let mut dims: [Option<String>; DIM_COLUMNS.len()] = Default::default();
    dims[DIM_COUNTRY] = env.country_str().map(str::to_owned);
    out.events.push(RawEvent {
        uuid: replay_chunk_uuid(&idx.session_id, idx.seq),
        ts,
        recv_ts: recv,
        name: REPLAY_CHUNK_EVENT.to_owned(),
        anon_id,
        user_id: non_empty(&idx.user_id),
        session_id: Some(idx.session_id),
        dims,
        flags: flags | if env.is_web() { FLAG_WEB } else { 0 },
        props,
        synthetic: true,
    });
}
