//! `format = 1`: LA1 batches (`spec/wire.md`, `spec/parquet.md` §2).

use super::*;
use crate::props::{find, sort_last_wins, PropVal};
use la_codec::clock::{self, FLAG_WEB};
use la_codec::ids::ZERO16;
use la_codec::wire::{
    self, BatchError, KIND_EVENT, KIND_IDENTIFY, KIND_RESET, KIND_SET_PERSON, MAX_ID_BYTES, MAX_NAME_BYTES,
};
use la_codec::DecodeError;
use la_core::schema::{
    DIM_APP_VERSION, DIM_COUNTRY, DIM_DEVICE_MODEL, DIM_DEVICE_TYPE, DIM_LOCALE, DIM_OS, DIM_OS_VERSION, DIM_PLATFORM,
    DIM_REFERRER_HOST, DIM_SCREEN, DIM_SDK, DIM_URL_PATH,
};

/// Reserved context/props keys and the dimension column each one feeds
/// (`spec/wire.md` "Ayrılmış anahtarlar").
const RESERVED: [(&str, usize); 10] = [
    ("platform", DIM_PLATFORM),
    ("os", DIM_OS),
    ("os_version", DIM_OS_VERSION),
    ("device_model", DIM_DEVICE_MODEL),
    ("device_type", DIM_DEVICE_TYPE),
    ("app_version", DIM_APP_VERSION),
    ("locale", DIM_LOCALE),
    ("path", DIM_URL_PATH),
    ("referrer", DIM_REFERRER_HOST),
    ("screen", DIM_SCREEN),
];

fn convert(props: &[wire::Prop]) -> PropList {
    props.iter().map(|p| (p.key.clone(), PropVal::from_wire(&p.value))).collect()
}

pub(super) fn decode(
    env: &Envelope,
    body: &[u8],
    drop_keys: &HashSet<String>,
    out: &mut ProjectBatch,
    st: &mut DecodeStats,
) {
    let batch = match wire::decode_batch(body) {
        Ok(b) => b,
        Err(e) => {
            // The whole batch is unusable; count the records the frame claims.
            let n = wire::check_frame(body).map(|f| f.records as u64).unwrap_or(1).max(1);
            let reason = if matches!(e, BatchError::Header(DecodeError::Utf8)) { BAD_UTF8 } else { BAD_RECORD };
            st.reject(reason, n);
            return;
        }
    };
    for (_, e) in &batch.rejected {
        st.reject(if *e == DecodeError::Utf8 { BAD_UTF8 } else { BAD_RECORD }, 1);
    }
    st.unknown_kinds += batch.unknown_kinds as u64;
    let h = &batch.header;
    let resets = batch.records.iter().filter(|r| r.kind == KIND_RESET).count() as u64;
    st.resets += resets;
    let live = batch.records.len() as u64 - resets;

    // Batch-level identity checks. A batch without an anonymous id but with a
    // user id (server-side senders) uses the user id for both.
    let user_id = non_empty(&h.user_id);
    let anon_id = if h.anon_id.is_empty() { h.user_id.clone() } else { h.anon_id.clone() };
    if anon_id.is_empty() {
        st.reject(EMPTY_ID, live);
        return;
    }
    if anon_id.len() > MAX_ID_BYTES || h.user_id.len() > MAX_ID_BYTES {
        st.reject(TOO_LONG, live);
        return;
    }
    let web = env.is_web();
    let ua_is_bot = web
        && h.context.iter().rev().find(|p| p.key == "ua").is_some_and(|p| match &p.value {
            wire::Value::Str(ua) => is_bot_ua(ua),
            _ => false,
        });
    if ua_is_bot {
        st.reject(BOT, live);
        return;
    }
    let context_reject = if too_many(h.context.len()) {
        Some(TOO_MANY_PROPS)
    } else if h.context.iter().any(|p| p.key.len() > wire::MAX_KEY_BYTES) {
        Some(TOO_LONG)
    } else {
        None
    };
    let recv = recv_ts(env);
    let sent_at = i64::try_from(h.sent_at).unwrap_or(i64::MAX);
    let session_id = (h.session_id != ZERO16).then_some(h.session_id);
    let person = la_core::events::person_key(user_id.as_deref(), &anon_id);

    for rec in &batch.records {
        if rec.kind == KIND_RESET {
            continue;
        }
        if too_many(rec.props.len()) {
            st.reject(TOO_MANY_PROPS, 1);
            continue;
        }
        let (ts, clock_flags) = clock::correct(recv, sent_at, i64::try_from(rec.ts).unwrap_or(i64::MAX));
        match rec.kind {
            KIND_EVENT => {
                if rec.name.is_empty() {
                    st.reject(EMPTY_NAME, 1);
                    continue;
                }
                if rec.name.len() > MAX_NAME_BYTES {
                    st.reject(TOO_LONG, 1);
                    continue;
                }
                if let Some(reason) = context_reject {
                    st.reject(reason, 1);
                    continue;
                }
                // context ∪ props, props win; within a list the last value wins.
                let mut all = convert(&h.context);
                all.extend(convert(&rec.props));
                let mut props = sort_last_wins(all);
                if let Err(reason) = enforce_limits(&mut props, drop_keys, st) {
                    st.reject(reason, 1);
                    continue;
                }
                let mut dims: [Option<String>; DIM_COLUMNS.len()] = Default::default();
                for (key, dim) in RESERVED {
                    let Some(i) = find(&props, key) else { continue };
                    // A non-string value stays in props and the column stays null.
                    if let PropVal::Str(s) = &props[i].1 {
                        let v = if key == "referrer" { referrer_host(s) } else { s.clone() };
                        dims[dim] = non_empty(&v);
                        props.remove(i);
                    }
                }
                // `sdk` comes from the header; a property of that name is ignored.
                if let Some(i) = find(&props, "sdk") {
                    props.remove(i);
                }
                dims[DIM_SDK] = non_empty(&h.sdk);
                dims[DIM_COUNTRY] = env.country_str().map(str::to_owned);
                out.events.push(RawEvent {
                    uuid: rec.uuid,
                    ts,
                    recv_ts: recv,
                    name: rec.name.clone(),
                    anon_id: anon_id.clone(),
                    user_id: user_id.clone(),
                    session_id,
                    dims,
                    flags: clock_flags | if web { FLAG_WEB } else { 0 },
                    props,
                    synthetic: false,
                });
            }
            KIND_IDENTIFY => {
                let mut props = sort_last_wins(convert(&rec.props));
                let user = match find(&props, "user_id").map(|i| props.remove(i).1) {
                    Some(PropVal::Str(u)) => u,
                    _ => {
                        st.reject(BAD_RECORD, 1);
                        continue;
                    }
                };
                if user.is_empty() {
                    st.reject(EMPTY_ID, 1);
                    continue;
                }
                if user.len() > MAX_ID_BYTES {
                    st.reject(TOO_LONG, 1);
                    continue;
                }
                if let Err(reason) = enforce_limits(&mut props, drop_keys, st) {
                    st.reject(reason, 1);
                    continue;
                }
                if !h.anon_id.is_empty() {
                    out.links.push(Candidate { anon_id: h.anon_id.clone(), user_id: user.clone(), ts, uuid: rec.uuid });
                }
                person_writes(&mut out.writes, &format!("u:{user}"), props, ts);
            }
            KIND_SET_PERSON => {
                let mut props = sort_last_wins(convert(&rec.props));
                if let Err(reason) = enforce_limits(&mut props, drop_keys, st) {
                    st.reject(reason, 1);
                    continue;
                }
                person_writes(&mut out.writes, &person, props, ts);
            }
            _ => st.unknown_kinds += 1,
        }
    }
}
