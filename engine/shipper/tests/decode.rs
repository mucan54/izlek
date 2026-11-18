//! Entry decoding and mapping rules: `spec/wal.md` §2–§3, `spec/wire.md` §3
//! and §5, `spec/parquet.md` §2.

mod common;

use common::*;
use la_codec::clock::{FLAG_CLOCK_CORRECTED, FLAG_COMPAT, FLAG_LATE, FLAG_WEB};
use la_codec::ids::{parse_uuid, posthog_event_uuid, replay_chunk_uuid};
use la_codec::wal::{ReplayIndex, ENV_FLAG_WEB, REPLAY_ENC_ZSTD};
use la_codec::wire::{Record, Value, KIND_RESET};
use la_core::schema::*;
use la_shipper::decode::{self, decode_entry, DecodeStats, ProjectBatch, RawEvent};
use la_shipper::props::{to_props_json, PropVal};
use std::collections::HashSet;

fn run(entry: &Entry) -> (ProjectBatch, DecodeStats) {
    run_with(entry, &[])
}

fn run_with(entry: &Entry, drop_keys: &[&str]) -> (ProjectBatch, DecodeStats) {
    let dk: HashSet<String> = drop_keys.iter().map(|s| s.to_string()).collect();
    let mut out = ProjectBatch::default();
    let mut st = DecodeStats::default();
    decode_entry(&entry.0, &entry.1, &dk, &mut out, &mut st);
    (out, st)
}

fn rejected(st: &DecodeStats) -> Vec<(&'static str, u64)> {
    st.rejected.iter().map(|(k, v)| (*k, *v)).collect()
}

fn props(ev: &RawEvent) -> String {
    to_props_json(&ev.props)
}

// ---------------------------------------------------------------- native

#[test]
fn native_event_row_production() {
    let mut h = header("anon-1", "", t0());
    h.session_id = [9; 16];
    h.context.extend([
        prop("locale", s("tr-TR")),
        prop("theme", s("dark")),
        prop("sdk", s("ignored")),
        prop("build", Value::Int(7)),
        prop("os_version", Value::Int(14)), // reserved but not a string: stays in props
    ]);
    let rec = event(
        1,
        t0() - 1000,
        "purchase",
        vec![
            prop("amount", Value::Float(12.5)),
            prop("theme", s("light")), // record props win over context
            prop("path", s("/checkout")),
            prop("referrer", s("https://www.google.com/search?q=x")),
            prop("screen", s("Checkout")),
            prop("items", Value::Json("[ {\"sku\": \"a\"}, 2 ]".into())),
            prop("broken", Value::Json("{nope".into())),
            prop("nan", Value::Float(f64::NAN)),
            prop("dup", Value::Int(1)),
            prop("dup", Value::Int(2)),
            prop("nothing", Value::Null),
            prop("ok", Value::Bool(true)),
        ],
    );
    let mut entry = native(7, t0(), &h, &[rec]);
    entry.0.flags = ENV_FLAG_WEB;
    let (out, st) = run(&entry);
    assert_eq!(st, DecodeStats::default());
    let ev = &out.events[0];
    assert_eq!((ev.uuid, ev.ts, ev.recv_ts, ev.name.as_str()), (uuid(1), t0() - 1000, t0(), "purchase"));
    assert_eq!((ev.anon_id.as_str(), ev.user_id.as_deref(), ev.session_id), ("anon-1", None, Some([9; 16])));
    assert_eq!(ev.flags, FLAG_WEB);
    assert_eq!(ev.dims[DIM_PLATFORM].as_deref(), Some("android"));
    assert_eq!(ev.dims[DIM_OS].as_deref(), Some("Android"));
    assert_eq!(ev.dims[DIM_OS_VERSION], None);
    assert_eq!(ev.dims[DIM_APP_VERSION].as_deref(), Some("3.1"));
    assert_eq!(ev.dims[DIM_LOCALE].as_deref(), Some("tr-TR"));
    assert_eq!(ev.dims[DIM_COUNTRY].as_deref(), Some("TR"));
    assert_eq!(ev.dims[DIM_SDK].as_deref(), Some("la-kmp/1.2.0"));
    assert_eq!(ev.dims[DIM_URL_PATH].as_deref(), Some("/checkout"));
    assert_eq!(ev.dims[DIM_REFERRER_HOST].as_deref(), Some("www.google.com"));
    assert_eq!(ev.dims[DIM_SCREEN].as_deref(), Some("Checkout"));
    assert_eq!(
        props(ev),
        r#"{"amount":12.5,"broken":"{nope","build":7,"dup":2,"items":[{"sku":"a"},2],"nan":null,"nothing":null,"ok":true,"os_version":14,"theme":"light"}"#
    );
    assert!(out.links.is_empty() && out.writes.is_empty());
}

#[test]
fn native_country_zz_is_null_and_referrer_without_url_is_kept() {
    let h = header("a", "u-1", 0);
    let mut entry = native(7, t0(), &h, &[event(1, t0(), "e", vec![prop("referrer", s("newsletter"))])]);
    entry.0.country = *b"ZZ";
    let (out, _) = run(&entry);
    let ev = &out.events[0];
    assert_eq!(ev.dims[DIM_COUNTRY], None);
    assert_eq!(ev.dims[DIM_REFERRER_HOST].as_deref(), Some("newsletter"));
    assert_eq!(ev.user_id.as_deref(), Some("u-1"));
    assert_eq!(props(ev), "{}");
}

#[test]
fn native_identify_set_person_and_reset() {
    let h = header("anon-1", "", 0);
    let recs = vec![
        identify(1, t0(), "user-42", vec![prop("plan", s("pro")), prop("age", Value::Int(30))]),
        set_person(2, t0() - 5, vec![prop("theme", s("dark")), prop("gone", Value::Null)]),
        Record { kind: KIND_RESET, uuid: uuid(3), ts: t0() as u64, ..Default::default() },
        // identify without a string user_id is malformed
        Record {
            kind: 1,
            uuid: uuid(4),
            ts: t0() as u64,
            name: String::new(),
            props: vec![prop("user_id", Value::Int(1))],
        },
    ];
    let (out, st) = run(&native(7, t0(), &h, &recs));
    assert!(out.events.is_empty(), "only kind=0 produces event rows");
    assert_eq!(out.links.len(), 1);
    assert_eq!(
        (out.links[0].anon_id.as_str(), out.links[0].user_id.as_str(), out.links[0].ts, out.links[0].uuid),
        ("anon-1", "user-42", t0(), uuid(1))
    );
    let writes: Vec<_> =
        out.writes.iter().map(|w| (w.person_key.as_str(), w.key.as_str(), w.value.as_str(), w.ts)).collect();
    assert_eq!(
        writes,
        [
            ("u:user-42", "age", "30", t0()),
            ("u:user-42", "plan", "\"pro\"", t0()),
            // the batch itself is still anonymous → written under the anonymous key
            ("a:anon-1", "gone", "null", t0() - 5),
            ("a:anon-1", "theme", "\"dark\"", t0() - 5),
        ]
    );
    assert_eq!(st.resets, 1);
    assert_eq!(rejected(&st), [("bad_record", 1)]);
}

#[test]
fn clock_vectors_apply_to_native_records() {
    let text = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../../spec/vectors/clock.json")).unwrap();
    let v: serde_json::Value = serde_json::from_str(&text).unwrap();
    let cases = v["cases"].as_array().unwrap();
    assert!(cases.len() >= 10);
    for c in cases {
        let (recv, sent, ts) =
            (c["recv_ts"].as_i64().unwrap(), c["sent_at"].as_i64().unwrap(), c["ts"].as_i64().unwrap());
        if recv < 0 || sent < 0 || ts < 0 {
            continue; // not representable on the wire (unsigned varints)
        }
        let (out, _) = run(&native(7, recv, &header("a", "", sent), &[event(1, ts, "e", vec![])]));
        let ev = &out.events[0];
        assert_eq!(ev.ts, c["out_ts"].as_i64().unwrap(), "{}", c["name"]);
        assert_eq!(ev.flags as i64, c["flags"].as_i64().unwrap(), "{}", c["name"]);
        assert_eq!(ev.recv_ts, recv);
    }
    assert_eq!((FLAG_CLOCK_CORRECTED, FLAG_LATE), (1, 2));
}

#[test]
fn limits_and_reject_reasons() {
    let ok = || header("anon", "", 0);
    let long = "x".repeat(129);
    let big = "v".repeat(8193);
    let many: Vec<_> = (0..201).map(|i| prop(&format!("k{i}"), Value::Int(i))).collect();
    let exactly: Vec<_> = (0..200).map(|i| prop(&format!("k{i}"), Value::Int(i))).collect();
    let recs = vec![
        event(1, t0(), "", vec![]),                            // empty_name
        event(2, t0(), &long, vec![]),                         // too_long (name)
        event(3, t0(), "e", vec![prop(&long, Value::Int(1))]), // too_long (key)
        event(4, t0(), "e", many),                             // too_many_props
        event(5, t0(), "e", exactly),                          // fine
        event(6, t0(), "e", vec![prop("big", s(&big)), prop("fits", s(&"v".repeat(8192)))]), // property dropped
        event(7, t0(), &"n".repeat(128), vec![prop(&"k".repeat(128), Value::Json(format!("[\"{big}\"]")))]),
    ];
    let (out, st) = run(&native(7, t0(), &ok(), &recs));
    assert_eq!(rejected(&st), [("empty_name", 1), ("too_long", 2), ("too_many_props", 1)]);
    assert_eq!(out.events.iter().map(|e| e.uuid).collect::<Vec<_>>(), [uuid(5), uuid(6), uuid(7)]);
    assert_eq!(st.props_dropped, 2, "oversized values drop the property, not the record");
    assert_eq!(out.events[0].props.len(), 200);
    assert_eq!(out.events[1].props.len(), 1);
    assert!(out.events[2].props.is_empty());

    // Identifier limits reject the whole batch.
    let two = [event(1, t0(), "e", vec![]), event(2, t0(), "e", vec![])];
    let (out, st) = run(&native(7, t0(), &header("", "", 0), &two));
    assert!(out.events.is_empty());
    assert_eq!(rejected(&st), [("empty_id", 2)]);
    let (_, st) = run(&native(7, t0(), &header(&"a".repeat(201), "", 0), &two));
    assert_eq!(rejected(&st), [("too_long", 2)]);
    let (_, st) = run(&native(7, t0(), &header("a", &"u".repeat(201), 0), &two));
    assert_eq!(rejected(&st), [("too_long", 2)]);
    let (out, st) = run(&native(7, t0(), &header(&"a".repeat(200), &"u".repeat(200), 0), &two));
    assert_eq!((out.events.len(), st.rejected_total()), (2, 0));
    // No anonymous id but a user id: the user id serves as both.
    let (out, _) = run(&native(7, t0(), &header("", "srv-user", 0), &two));
    assert_eq!((out.events[0].anon_id.as_str(), out.events[0].user_id.as_deref()), ("srv-user", Some("srv-user")));
}

#[test]
fn malformed_native_bodies_are_counted_not_fatal() {
    let h = header("anon", "", 0);
    let good = native(7, t0(), &h, &[event(1, t0(), "e", vec![]), event(2, t0(), "e", vec![])]);
    // Not an LA1 batch at all.
    let (out, st) = run(&(good.0, b"garbage".to_vec()));
    assert!(out.events.is_empty());
    assert_eq!(rejected(&st), [("bad_record", 1)]);
    // Invalid UTF-8 inside one record: only that record is lost.
    let mut bad = la_codec::wire::encode_record_body(&event(3, t0(), "name", vec![]));
    let pos = bad.len() - 2;
    bad[pos] = 0xff;
    let mut body = good.1.clone();
    body.push(bad.len() as u8);
    body.extend_from_slice(&bad);
    let (out, st) = run(&(good.0, body));
    assert_eq!(out.events.len(), 2);
    assert_eq!(rejected(&st), [("bad_utf8", 1)]);
    // Unknown record kind is skipped silently (forward compatibility).
    let mut body = good.1.clone();
    let mut future = la_codec::wire::encode_record_body(&event(4, t0(), "x", vec![]));
    future[0] = 9;
    body.push(future.len() as u8);
    body.extend_from_slice(&future);
    let (out, st) = run(&(good.0, body));
    assert_eq!((out.events.len(), st.unknown_kinds, st.rejected_total()), (2, 1, 0));
    // Unknown entry format.
    let mut e = good.clone();
    e.0.format = 99;
    let (out, st) = run(&e);
    assert!(out.events.is_empty());
    assert_eq!((st.unknown_format, st.rejected_total()), (1, 0));
}

#[test]
fn bot_filter_needs_the_web_bit() {
    let mut h = header("anon", "", 0);
    h.context.push(prop("ua", s("Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")));
    let recs = [event(1, t0(), "pageview", vec![]), set_person(2, t0(), vec![prop("k", Value::Int(1))])];
    let mut entry = native(7, t0(), &h, &recs);
    let (out, st) = run(&entry);
    assert_eq!((out.events.len(), out.writes.len(), st.rejected_total()), (1, 1, 0), "not a web request");
    entry.0.flags = ENV_FLAG_WEB;
    let (out, st) = run(&entry);
    assert!(out.events.is_empty() && out.writes.is_empty());
    assert_eq!(rejected(&st), [("bot", 2)]);
    for ua in
        ["HeadlessChrome/120", "Chrome-Lighthouse", "Pingdom.com_bot", "PhantomJS/2.1", "AhrefsSpider", "my CRAWLER"]
    {
        assert!(decode::is_bot_ua(ua), "{ua}");
    }
    assert!(!decode::is_bot_ua("Mozilla/5.0 (Linux; Android 14) Chrome/120 Mobile Safari/537.36"));
}

#[test]
fn drop_keys_remove_properties_dimensions_and_person_properties() {
    let mut h = header("anon", "", 0);
    h.context.push(prop("email", s("a@b.c")));
    let recs = [
        event(1, t0(), "e", vec![prop("email", s("x@y.z")), prop("locale", s("tr")), prop("keep", Value::Int(1))]),
        set_person(2, t0(), vec![prop("email", s("x@y.z")), prop("plan", s("pro"))]),
    ];
    let (out, _) = run_with(&native(7, t0(), &h, &recs), &["email", "locale"]);
    assert_eq!(props(&out.events[0]), r#"{"keep":1}"#);
    assert_eq!(out.events[0].dims[DIM_LOCALE], None);
    assert_eq!(out.writes.iter().map(|w| w.key.as_str()).collect::<Vec<_>>(), ["plan"]);
}

// --------------------------------------------------------------- posthog

fn ph(json: &str) -> (ProjectBatch, DecodeStats) {
    run(&posthog(7, t0(), json))
}

#[test]
fn posthog_three_payload_shapes() {
    let ev = r#"{"event":"$pageview","distinct_id":"d1","properties":{"$os":"iOS","plan":"pro"}}"#;
    let (batch, st) = ph(&format!(r#"{{"api_key":"pk","batch":[{ev},{ev}],"sent_at":"2026-11-18T10:00:00Z"}}"#));
    assert_eq!((batch.events.len(), st.rejected_total()), (2, 0));
    let (single, _) = ph(ev);
    assert_eq!(single.events.len(), 1);
    let (array, _) = ph(&format!("[{ev}, {ev} ,{ev}]"));
    assert_eq!(array.events.len(), 3);
    let e = &single.events[0];
    assert_eq!((e.name.as_str(), e.anon_id.as_str(), e.user_id.as_deref()), ("$pageview", "d1", None));
    assert_eq!(e.flags, FLAG_COMPAT);
    assert_eq!(e.dims[DIM_OS].as_deref(), Some("iOS"));
    assert_eq!(props(e), r#"{"$os":"iOS","plan":"pro"}"#, "mapped keys also stay in props");
    // All three shapes carry byte-identical event objects → identical derived uuids.
    assert_eq!(e.uuid, batch.events[0].uuid);
    assert_eq!(e.uuid, array.events[2].uuid);
    assert_eq!(e.uuid, posthog_event_uuid(7, ev.as_bytes()));
    assert_eq!(e.uuid[6] >> 4, 8);
    // Garbage payloads.
    assert_eq!(rejected(&ph("not json").1), [("bad_record", 1)]);
    assert_eq!(rejected(&ph("42").1), [("bad_record", 1)]);
    assert_eq!(rejected(&ph(r#"[1,{"event":"e","distinct_id":"d"}]"#).1), [("bad_record", 1)]);
    assert_eq!(rejected(&run(&(env(2, 7, t0()), vec![0xff, 0xfe])).1), [("bad_utf8", 1)]);
}

#[test]
fn posthog_uuid_given_or_derived_from_raw_bytes() {
    let given = r#"{"event":"e","distinct_id":"d","uuid":"0193F1A2-b3c4-7d5e-8a9b-0c1d2e3f4a5b"}"#;
    assert_eq!(ph(given).0.events[0].uuid, parse_uuid("0193f1a2b3c47d5e8a9b0c1d2e3f4a5b").unwrap());
    // An invalid uuid falls back to the derivation.
    let invalid = r#"{"event":"e","distinct_id":"d","uuid":"nope"}"#;
    assert_eq!(ph(invalid).0.events[0].uuid, posthog_event_uuid(7, invalid.as_bytes()));
    // Deterministic, sensitive to project and to every raw byte (even whitespace and key order).
    let a = r#"{"event":"e","distinct_id":"d","properties":{"a":1,"b":2}}"#;
    let b = r#"{"event":"e","distinct_id":"d","properties":{"b":2,"a":1}}"#;
    let c = r#"{"event":"e", "distinct_id":"d","properties":{"a":1,"b":2}}"#;
    let id = |json: &str| ph(json).0.events[0].uuid;
    assert_eq!(id(a), id(a));
    assert_ne!(id(a), id(b));
    assert_ne!(id(a), id(c));
    assert_ne!(id(a), run(&posthog(8, t0(), a)).0.events[0].uuid);
    // Inside a batch the raw bytes are those of the element only.
    assert_eq!(ph(&format!(r#"{{"batch":[ {a} ]}}"#)).0.events[0].uuid, id(a));
}

#[test]
fn posthog_distinct_id_fallbacks() {
    let id = |json: &str| ph(json).0.events.first().map(|e| e.anon_id.clone());
    assert_eq!(id(r#"{"event":"e","distinct_id":"top","properties":{"distinct_id":"inner"}}"#).unwrap(), "top");
    assert_eq!(id(r#"{"event":"e","properties":{"distinct_id":"inner"},"$distinct_id":"dollar"}"#).unwrap(), "inner");
    assert_eq!(id(r#"{"event":"e","$distinct_id":"dollar"}"#).unwrap(), "dollar");
    assert_eq!(id(r#"{"event":"e","distinct_id":12345}"#).unwrap(), "12345");
    assert_eq!(id(r#"{"event":"e","distinct_id":{"a":1}}"#).unwrap(), r#"{"a":1}"#);
    assert_eq!(id(r#"{"event":"e","distinct_id":"","properties":{"distinct_id":"inner"}}"#).unwrap(), "inner");
    assert_eq!(rejected(&ph(r#"{"event":"e","properties":{}}"#).1), [("empty_id", 1)]);
    assert_eq!(rejected(&ph(r#"{"event":"e","distinct_id":null}"#).1), [("empty_id", 1)]);
    assert_eq!(rejected(&ph(r#"{"distinct_id":"d"}"#).1), [("empty_name", 1)]);
    assert_eq!(rejected(&ph(&format!(r#"{{"event":"e","distinct_id":"{}"}}"#, "d".repeat(201))).1), [("too_long", 1)]);
}

#[test]
fn posthog_timestamps() {
    let ts = |json: &str| {
        let e = ph(json).0.events.remove(0);
        (e.ts - t0(), e.flags & 3)
    };
    // (a) offset wins over everything else.
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","offset":1500,"timestamp":"2020-01-01T00:00:00Z"}"#), (-1500, 0));
    // A negative offset would be in the future: clamped to recv_ts.
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","offset":-5}"#), (0, 1));
    // (b) timestamp + sent_at: device 1 h ahead → corrected by the skew.
    let b = r#"{"event":"e","distinct_id":"d","timestamp":"2026-11-18T10:59:50.000Z","sent_at":"2026-11-18T11:00:00.000Z"}"#;
    assert_eq!(ts(b), (-10_000, 1));
    // sent_at on the envelope of a batch.
    let env_sent = r#"{"batch":[{"event":"e","distinct_id":"d","timestamp":"2026-11-18T10:59:50Z"}],"sent_at":"2026-11-18T11:00:00+00:00"}"#;
    assert_eq!(ts(env_sent), (-10_000, 1));
    // The event's own sent_at wins over the envelope's.
    let both = r#"{"batch":[{"event":"e","distinct_id":"d","timestamp":"2026-11-18T09:59:58Z","sent_at":"2026-11-18T10:00:00Z"}],"sent_at":"2030-01-01T00:00:00Z"}"#;
    assert_eq!(ts(both), (-2_000, 0));
    // (c) timestamp only: taken as is, clamped when in the future.
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","timestamp":"2026-11-18T09:00:00Z"}"#), (-HOUR_MS, 0));
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","timestamp":"2026-11-18T12:00:00+01:00"}"#), (0, 1));
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","timestamp":"2026-10-01T00:00:00Z"}"#).1, 2, "late");
    // (d) nothing: recv_ts.
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d"}"#), (0, 0));
    assert_eq!(ts(r#"{"event":"e","distinct_id":"d","timestamp":"yesterday-ish"}"#), (0, 0));
    // Unix numbers in seconds and milliseconds, and other ISO spellings.
    let secs = (t0() - 60_000) / 1000;
    assert_eq!(ts(&format!(r#"{{"event":"e","distinct_id":"d","timestamp":{secs}}}"#)), (-60_000, 0));
    assert_eq!(ts(&format!(r#"{{"event":"e","distinct_id":"d","timestamp":{}}}"#, t0() - 60_000)), (-60_000, 0));
    assert_eq!(ts(&format!(r#"{{"event":"e","distinct_id":"d","timestamp":"{}"}}"#, t0() - 1)), (-1, 0));
    assert_eq!(decode::parse_timestamp("2026-11-18T10:00:00"), Some(t0()));
    assert_eq!(decode::parse_timestamp("2026-11-18 10:00:00.250"), Some(t0() + 250));
    assert_eq!(decode::parse_timestamp("2026-11-18T13:00:00+0300"), Some(t0()));
    assert_eq!(decode::parse_timestamp("2026-11-18"), Some(t0() - 10 * HOUR_MS));
    assert_eq!(decode::parse_timestamp(""), None);
    assert_eq!(decode::parse_timestamp("-5"), None);
}

#[test]
fn posthog_identify_links_the_anonymous_id_and_itself() {
    let json = r#"{"event":"$identify","distinct_id":"user-1","uuid":"0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4a5b",
        "properties":{"$anon_distinct_id":"anon-9","$set":{"plan":"pro","n":3},"$set_once":{"first":"x"},"k":1}}"#;
    let (out, st) = ph(json);
    assert_eq!(st.rejected_total(), 0);
    let links: Vec<_> = out.links.iter().map(|l| (l.anon_id.as_str(), l.user_id.as_str(), l.ts)).collect();
    assert_eq!(links, [("anon-9", "user-1", t0()), ("user-1", "user-1", t0())]);
    assert!(out.links.iter().all(|l| l.uuid == out.events[0].uuid));
    // The event itself is an ordinary row, written for the user.
    let e = &out.events[0];
    assert_eq!((e.name.as_str(), e.user_id.as_deref(), e.anon_id.as_str()), ("$identify", Some("user-1"), "anon-9"));
    assert_eq!(props(e), r#"{"k":1}"#, "$set/$set_once/$anon_distinct_id never reach props");
    // $set_once is the weakest write (ts = 0), $set carries the event time.
    let writes: Vec<_> =
        out.writes.iter().map(|w| (w.person_key.as_str(), w.key.as_str(), w.value.as_str(), w.ts)).collect();
    assert_eq!(
        writes,
        [("u:user-1", "first", "\"x\"", 0), ("u:user-1", "n", "3", t0()), ("u:user-1", "plan", "\"pro\"", t0())]
    );
    // Same ids on both sides, or a non-string anonymous id: no link.
    assert!(ph(r#"{"event":"$identify","distinct_id":"u","properties":{"$anon_distinct_id":"u"}}"#).0.links.is_empty());
    assert!(ph(r#"{"event":"$identify","distinct_id":"u","properties":{"$anon_distinct_id":7}}"#).0.links.is_empty());
    assert!(ph(r#"{"event":"$pageview","distinct_id":"u","properties":{"$anon_distinct_id":"a"}}"#).0.links.is_empty());
}

#[test]
fn posthog_alias_identified_events_and_event_level_set() {
    let (out, _) = ph(r#"{"event":"$create_alias","distinct_id":"user-1","properties":{"alias":"old-id"}}"#);
    let links: Vec<_> = out.links.iter().map(|l| (l.anon_id.as_str(), l.user_id.as_str())).collect();
    assert_eq!(links, [("old-id", "user-1"), ("user-1", "user-1")]);
    assert_eq!((out.events[0].anon_id.as_str(), out.events[0].user_id.as_deref()), ("user-1", None));

    let (out, _) = ph(r#"{"event":"x","distinct_id":"user-1","$set":{"top":true},
            "properties":{"$is_identified":true,"$device_id":"dev-7","$session_id":"0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4a5b",
                          "$unset":["old"],"token":"pk","distinct_id":"user-1"}}"#);
    let e = &out.events[0];
    assert_eq!((e.anon_id.as_str(), e.user_id.as_deref()), ("dev-7", Some("user-1")));
    assert_eq!(e.session_id, parse_uuid("0193f1a2b3c47d5e8a9b0c1d2e3f4a5b"));
    assert_eq!(
        props(e),
        r#"{"$device_id":"dev-7","$is_identified":true,"$session_id":"0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4a5b"}"#
    );
    let writes: Vec<_> = out.writes.iter().map(|w| (w.person_key.as_str(), w.key.as_str(), w.value.as_str())).collect();
    assert_eq!(writes, [("u:user-1", "top", "true"), ("u:user-1", "old", "null")]);
    // $is_identified without a device id: the distinct id serves as both.
    let (out, _) = ph(r#"{"event":"x","distinct_id":"user-1","properties":{"$is_identified":true}}"#);
    assert_eq!((out.events[0].anon_id.as_str(), out.events[0].user_id.as_deref()), ("user-1", Some("user-1")));
    // A non-uuid session id is dropped.
    let (out, _) = ph(r#"{"event":"x","distinct_id":"d","properties":{"$session_id":"abc"}}"#);
    assert_eq!(out.events[0].session_id, None);
}

#[test]
fn posthog_dimension_mapping_and_value_kinds() {
    let json = r#"{"event":"$screen","distinct_id":"d","properties":{
        "$os":"Android","$os_version":"14","$device_model":"Pixel 8","$device_type":"Mobile","$app_version":"3.1",
        "$locale":"tr-TR","$lib":"posthog-android","$lib_version":"3.4.0","$pathname":"/home",
        "$referring_domain":"t.co","$screen_name":"Home",
        "n":1,"big":18446744073709551615,"f":1.50,"b":false,"z":null,"arr":[1,{"k":"v"}],"obj":{"y":1,"x":[]}}}"#;
    let mut entry = posthog(7, t0(), json);
    entry.0.flags = ENV_FLAG_WEB;
    let (out, _) = run(&entry);
    let e = &out.events[0];
    let dim = |i: usize| e.dims[i].as_deref();
    assert_eq!(
        (dim(DIM_OS), dim(DIM_OS_VERSION), dim(DIM_DEVICE_MODEL)),
        (Some("Android"), Some("14"), Some("Pixel 8"))
    );
    assert_eq!(
        (dim(DIM_DEVICE_TYPE), dim(DIM_APP_VERSION), dim(DIM_LOCALE)),
        (Some("Mobile"), Some("3.1"), Some("tr-TR"))
    );
    assert_eq!((dim(DIM_PLATFORM), dim(DIM_SDK)), (Some("android"), Some("posthog-android/3.4.0")));
    assert_eq!(
        (dim(DIM_URL_PATH), dim(DIM_REFERRER_HOST), dim(DIM_SCREEN)),
        (Some("/home"), Some("t.co"), Some("Home"))
    );
    assert_eq!(dim(DIM_COUNTRY), Some("TR"));
    assert_eq!(e.flags, FLAG_COMPAT | FLAG_WEB);
    let get = |k: &str| e.props.iter().find(|(key, _)| key == k).map(|(_, v)| v.clone()).unwrap();
    assert_eq!(get("n"), PropVal::Int(1));
    assert_eq!(get("big").type_name(), "float");
    assert_eq!(get("f"), PropVal::Float(1.5));
    assert_eq!(get("b"), PropVal::Bool(false));
    assert_eq!(get("z"), PropVal::Null);
    assert_eq!(get("arr"), PropVal::Json(r#"[1,{"k":"v"}]"#.into()));
    assert_eq!(get("obj"), PropVal::Json(r#"{"x":[],"y":1}"#.into()));
    assert_eq!(get("$lib"), PropVal::Str("posthog-android".into()));
    for (lib, platform) in [("web", "web"), ("posthog-ios", "ios"), ("posthog-node", "posthog-node")] {
        let (out, _) = ph(&format!(r#"{{"event":"e","distinct_id":"d","properties":{{"$lib":"{lib}"}}}}"#));
        assert_eq!(out.events[0].dims[DIM_PLATFORM].as_deref(), Some(platform));
        assert_eq!(out.events[0].dims[DIM_SDK].as_deref(), Some(lib));
    }
}

#[test]
fn posthog_limits() {
    let many: String = (0..201).map(|i| format!(r#""k{i}":{i}"#)).collect::<Vec<_>>().join(",");
    assert_eq!(
        rejected(&ph(&format!(r#"{{"event":"e","distinct_id":"d","properties":{{{many}}}}}"#)).1),
        [("too_many_props", 1)]
    );
    let long_key = "k".repeat(129);
    assert_eq!(
        rejected(&ph(&format!(r#"{{"event":"e","distinct_id":"d","properties":{{"{long_key}":1}}}}"#)).1),
        [("too_long", 1)]
    );
    let big = "v".repeat(8193);
    let (out, st) = ph(&format!(r#"{{"event":"e","distinct_id":"d","properties":{{"big":"{big}","ok":1}}}}"#));
    assert_eq!((props(&out.events[0]).as_str(), st.props_dropped), (r#"{"ok":1}"#, 1));
    assert_eq!(rejected(&ph(&format!(r#"{{"event":"{}","distinct_id":"d"}}"#, "n".repeat(129))).1), [("too_long", 1)]);
    // A rejected $identify leaves no link behind.
    let (out, st) = ph(&format!(
        r#"{{"event":"$identify","distinct_id":"u","properties":{{"$anon_distinct_id":"a","{long_key}":1}}}}"#
    ));
    assert_eq!((out.links.len(), out.events.len(), st.rejected_total()), (0, 0, 1));
}

// ---------------------------------------------------------------- replay

#[test]
fn replay_index_becomes_a_chunk_event() {
    let idx = ReplayIndex {
        session_id: [0xab; 16],
        seq: 3,
        start_ts: (t0() - 4000) as u64,
        duration_ms: 5000,
        bytes: 12_345,
        encoding: REPLAY_ENC_ZSTD,
        anon_id: "anon-1".into(),
        user_id: "user-1".into(),
        key: "replay/p=7/d=2026-11-18/abababababababababababababababab/000003.bin.zst".into(),
    };
    let (out, st) = run(&replay(7, t0(), &idx));
    assert_eq!(st, DecodeStats::default());
    let e = &out.events[0];
    assert_eq!(e.name, "session_replay_chunk");
    assert_eq!(e.uuid, replay_chunk_uuid(&[0xab; 16], 3));
    assert_eq!((e.ts, e.recv_ts, e.flags), (t0() - 4000, t0(), 0));
    assert_eq!((e.anon_id.as_str(), e.user_id.as_deref(), e.session_id), ("anon-1", Some("user-1"), Some([0xab; 16])));
    assert_eq!(
        props(e),
        r#"{"bytes":12345,"duration_ms":5000,"encoding":1,"key":"replay/p=7/d=2026-11-18/abababababababababababababababab/000003.bin.zst","seq":3}"#
    );
    assert!(e.synthetic);
    // Device clock ahead: clamped to recv_ts (no skew correction for replay entries).
    let future = ReplayIndex { start_ts: (t0() + 60_000) as u64, user_id: String::new(), ..idx.clone() };
    let e = run(&replay(7, t0(), &future)).0.events.remove(0);
    assert_eq!((e.ts, e.flags, e.user_id), (t0(), FLAG_CLOCK_CORRECTED, None));
    // Re-sent chunk: same uuid.
    assert_eq!(run(&replay(7, t0() + 99, &idx)).0.events[0].uuid, out.events[0].uuid);
    // Truncated body.
    let (_, st) = run(&(env(3, 7, t0()), idx.encode()[..20].to_vec()));
    assert_eq!(rejected(&st), [("bad_record", 1)]);
}
