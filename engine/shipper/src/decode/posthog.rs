//! `format = 2`: PostHog capture payloads (`spec/wal.md` §2.2 and §3).

use super::*;
use crate::props::{sort_last_wins, PropVal};
use chrono::{DateTime, NaiveDate, NaiveDateTime};
use la_codec::clock::{self, FLAG_COMPAT, FLAG_WEB};
use la_codec::ids::{parse_uuid, posthog_event_uuid};
use la_codec::wire::{MAX_ID_BYTES, MAX_NAME_BYTES};
use la_core::schema::{
    DIM_APP_VERSION, DIM_COUNTRY, DIM_DEVICE_MODEL, DIM_DEVICE_TYPE, DIM_LOCALE, DIM_OS, DIM_OS_VERSION, DIM_PLATFORM,
    DIM_REFERRER_HOST, DIM_SCREEN, DIM_SDK, DIM_URL_PATH,
};
use serde_json::value::RawValue;
use serde_json::{Map, Value};

/// Properties that never reach `props`.
const EXCLUDED: [&str; 6] = ["token", "distinct_id", "$set", "$set_once", "$unset", "$anon_distinct_id"];

/// PostHog property → dimension column (the key also stays in `props`).
const DIMS: [(&str, usize); 9] = [
    ("$os", DIM_OS),
    ("$os_version", DIM_OS_VERSION),
    ("$device_model", DIM_DEVICE_MODEL),
    ("$device_type", DIM_DEVICE_TYPE),
    ("$app_version", DIM_APP_VERSION),
    ("$locale", DIM_LOCALE),
    ("$pathname", DIM_URL_PATH),
    ("$referring_domain", DIM_REFERRER_HOST),
    ("$screen_name", DIM_SCREEN),
];

/// Unix seconds below this, milliseconds from here on (1e11 s is the year
/// 5138, 1e11 ms is 1973).
const SECONDS_BELOW: f64 = 1e11;

fn number_to_ms(n: f64) -> Option<i64> {
    if !n.is_finite() || n < 0.0 {
        return None;
    }
    let ms = if n < SECONDS_BELOW { n * 1000.0 } else { n };
    (ms < 9.0e15).then_some(ms as i64)
}

/// ISO-8601 / RFC 3339 text or a unix number (seconds or milliseconds) → unix ms.
pub fn parse_timestamp(s: &str) -> Option<i64> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    if let Ok(d) = DateTime::parse_from_rfc3339(s) {
        return Some(d.timestamp_millis()).filter(|t| *t >= 0);
    }
    for fmt in ["%Y-%m-%dT%H:%M:%S%.f%z", "%Y-%m-%d %H:%M:%S%.f%z"] {
        if let Ok(d) = DateTime::parse_from_str(s, fmt) {
            return Some(d.timestamp_millis()).filter(|t| *t >= 0);
        }
    }
    // No offset: UTC.
    for fmt in ["%Y-%m-%dT%H:%M:%S%.f", "%Y-%m-%d %H:%M:%S%.f"] {
        if let Ok(d) = NaiveDateTime::parse_from_str(s, fmt) {
            return Some(d.and_utc().timestamp_millis()).filter(|t| *t >= 0);
        }
    }
    if let Ok(d) = NaiveDate::parse_from_str(s, "%Y-%m-%d") {
        return d.and_hms_opt(0, 0, 0).map(|d| d.and_utc().timestamp_millis()).filter(|t| *t >= 0);
    }
    s.parse::<f64>().ok().and_then(number_to_ms)
}

fn ts_value(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => n.as_f64().and_then(number_to_ms),
        Value::String(s) => parse_timestamp(s),
        _ => None,
    }
}

fn str_of<'a>(obj: Option<&'a Map<String, Value>>, key: &str) -> Option<&'a str> {
    obj?.get(key)?.as_str().filter(|s| !s.is_empty())
}

fn to_list(obj: &Map<String, Value>) -> PropList {
    // serde_json maps iterate in key order already, but do not rely on the
    // crate's feature flags for that.
    sort_last_wins(obj.iter().map(|(k, v)| (k.clone(), PropVal::from_json(v))).collect())
}

pub(super) fn decode(
    env: &Envelope,
    body: &[u8],
    drop_keys: &HashSet<String>,
    out: &mut ProjectBatch,
    st: &mut DecodeStats,
) {
    let Ok(text) = std::str::from_utf8(body) else {
        st.reject(BAD_UTF8, 1);
        return;
    };
    let text = text.trim();
    let events: Vec<&RawValue>;
    let mut envelope_sent_at = None;
    match text.as_bytes().first() {
        Some(b'[') => match serde_json::from_str::<Vec<&RawValue>>(text) {
            Ok(v) => events = v,
            Err(_) => return st.reject(BAD_RECORD, 1),
        },
        Some(b'{') => {
            let Ok(top) = serde_json::from_str::<BTreeMap<String, &RawValue>>(text) else {
                return st.reject(BAD_RECORD, 1);
            };
            match top.get("batch").filter(|b| b.get().starts_with('[')) {
                Some(batch) => {
                    let Ok(v) = serde_json::from_str::<Vec<&RawValue>>(batch.get()) else {
                        return st.reject(BAD_RECORD, 1);
                    };
                    events = v;
                    envelope_sent_at = top
                        .get("sent_at")
                        .and_then(|r| serde_json::from_str::<Value>(r.get()).ok())
                        .as_ref()
                        .and_then(ts_value);
                }
                None => {
                    let Ok(raw) = serde_json::from_str::<&RawValue>(text) else {
                        return st.reject(BAD_RECORD, 1);
                    };
                    events = vec![raw];
                }
            }
        }
        _ => return st.reject(BAD_RECORD, 1),
    }
    for raw in events {
        if let Err(reason) = decode_event(env, raw, envelope_sent_at, drop_keys, out, st) {
            st.reject(reason, 1);
        }
    }
}

fn decode_event(
    env: &Envelope,
    raw: &RawValue,
    envelope_sent_at: Option<i64>,
    drop_keys: &HashSet<String>,
    out: &mut ProjectBatch,
    st: &mut DecodeStats,
) -> Result<(), &'static str> {
    let Ok(Value::Object(ev)) = serde_json::from_str::<Value>(raw.get()) else {
        return Err(BAD_RECORD);
    };
    let props = ev.get("properties").and_then(Value::as_object);

    // distinct_id: first non-empty of the three places; non-strings as JSON text.
    let distinct_id = [ev.get("distinct_id"), props.and_then(|p| p.get("distinct_id")), ev.get("$distinct_id")]
        .into_iter()
        .flatten()
        .map(|v| match v {
            Value::String(s) => s.clone(),
            Value::Null => String::new(),
            other => other.to_string(),
        })
        .find(|s| !s.is_empty())
        .ok_or(EMPTY_ID)?;
    if distinct_id.len() > MAX_ID_BYTES {
        return Err(TOO_LONG);
    }
    let name = ev.get("event").and_then(Value::as_str).filter(|n| !n.is_empty()).ok_or(EMPTY_NAME)?;
    if name.len() > MAX_NAME_BYTES {
        return Err(TOO_LONG);
    }
    let web = env.is_web();
    if web && str_of(props, "$raw_user_agent").is_some_and(is_bot_ua) {
        return Err(BOT);
    }
    let uuid = ev
        .get("uuid")
        .and_then(Value::as_str)
        .and_then(parse_uuid)
        .unwrap_or_else(|| posthog_event_uuid(env.project_id, raw.get().as_bytes()));

    // Timestamp, in the order of `spec/wal.md` §3.
    let recv = recv_ts(env);
    let offset = ev.get("offset").and_then(Value::as_f64).filter(|o| o.is_finite());
    let timestamp = ev.get("timestamp").and_then(ts_value);
    let sent_at = ev.get("sent_at").and_then(ts_value).or(envelope_sent_at).filter(|s| *s > 0);
    let (ts, clock_flags) = match (offset, timestamp, sent_at) {
        (Some(off), _, _) => clock::clamp(recv, recv.saturating_sub(off as i64)),
        (None, Some(t), Some(s)) => clock::correct(recv, s, t),
        (None, Some(t), None) => clock::clamp(recv, t),
        (None, None, _) => (recv, 0),
    };

    // Identity.
    let identified = props.and_then(|p| p.get("$is_identified")).and_then(Value::as_bool) == Some(true);
    let (mut anon_id, mut user_id) = if identified {
        (str_of(props, "$device_id").unwrap_or(&distinct_id).to_owned(), Some(distinct_id.clone()))
    } else {
        (distinct_id.clone(), None)
    };
    let link_from = match name {
        "$identify" => str_of(props, "$anon_distinct_id"),
        "$create_alias" => str_of(props, "alias"),
        _ => None,
    }
    .filter(|a| *a != distinct_id && a.len() <= MAX_ID_BYTES);
    let mut links: Vec<Candidate> = Vec::new();
    if let Some(from) = link_from {
        links.push(Candidate { anon_id: from.to_owned(), user_id: distinct_id.clone(), ts, uuid });
        // Self link: later events that carry the user id as a plain
        // distinct_id resolve to the same person.
        links.push(Candidate { anon_id: distinct_id.clone(), user_id: distinct_id.clone(), ts, uuid });
        if name == "$identify" {
            if !identified {
                anon_id = from.to_owned();
            }
            user_id = Some(distinct_id.clone());
        }
    }
    if anon_id.len() > MAX_ID_BYTES {
        return Err(TOO_LONG);
    }

    // Event properties.
    let mut list: PropList = match props {
        Some(p) => sort_last_wins(
            p.iter()
                .filter(|(k, _)| !EXCLUDED.contains(&k.as_str()))
                .map(|(k, v)| (k.clone(), PropVal::from_json(v)))
                .collect(),
        ),
        None => Vec::new(),
    };
    if too_many(list.len()) {
        return Err(TOO_MANY_PROPS);
    }
    enforce_limits(&mut list, drop_keys, st)?;

    let mut dims: [Option<String>; DIM_COLUMNS.len()] = Default::default();
    for (key, dim) in DIMS {
        dims[dim] = str_of(props, key).map(str::to_owned);
    }
    if let Some(lib) = str_of(props, "$lib") {
        dims[DIM_PLATFORM] = Some(
            match lib {
                "posthog-android" => "android",
                "posthog-ios" => "ios",
                other => other,
            }
            .to_owned(),
        );
        dims[DIM_SDK] = Some(match str_of(props, "$lib_version") {
            Some(v) => format!("{lib}/{v}"),
            None => lib.to_owned(),
        });
    }
    dims[DIM_COUNTRY] = env.country_str().map(str::to_owned);
    // Dimensions follow the block list too.
    for (key, dim) in DIMS {
        if drop_keys.contains(key) {
            dims[dim] = None;
        }
    }

    // The record is accepted from here on.
    out.links.extend(links);

    // Person properties: `$set_once` (weakest, ts = 0), `$set`, `$unset`.
    let person = la_core::events::person_key(user_id.as_deref(), &anon_id);
    let mut write = |obj: &Map<String, Value>, ts: i64, st: &mut DecodeStats| {
        let mut l = to_list(obj);
        if too_many(l.len()) || enforce_limits(&mut l, drop_keys, st).is_err() {
            st.props_dropped += l.len() as u64;
            return;
        }
        person_writes(&mut out.writes, &person, l, ts);
    };
    for holder in [props, Some(&ev)].into_iter().flatten() {
        if let Some(Value::Object(o)) = holder.get("$set_once") {
            write(o, 0, st);
        }
    }
    for holder in [props, Some(&ev)].into_iter().flatten() {
        if let Some(Value::Object(o)) = holder.get("$set") {
            write(o, ts, st);
        }
    }
    if let Some(Value::Array(keys)) = props.and_then(|p| p.get("$unset")) {
        let o: Map<String, Value> =
            keys.iter().filter_map(Value::as_str).map(|k| (k.to_owned(), Value::Null)).collect();
        write(&o, ts, st);
    }

    out.events.push(RawEvent {
        uuid,
        ts,
        recv_ts: recv,
        name: name.to_owned(),
        anon_id,
        user_id,
        session_id: str_of(props, "$session_id").and_then(parse_uuid),
        dims,
        flags: clock_flags | FLAG_COMPAT | if web { FLAG_WEB } else { 0 },
        props: list,
        synthetic: false,
    });
    Ok(())
}
