//! Shared test vectors (`spec/vectors/`): the Rust codec must agree with the
//! Python reference byte for byte.

use la_codec::wire::{self, Batch, Header, Prop, Record, Value};
use la_codec::{clock, ids, murmur3};
use serde_json::Value as J;
use std::path::PathBuf;

fn load(name: &str) -> J {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../spec/vectors").join(name);
    serde_json::from_slice(&std::fs::read(&p).unwrap_or_else(|e| panic!("{}: {e}", p.display()))).unwrap()
}

fn unhex(s: &str) -> Vec<u8> {
    assert!(s.len().is_multiple_of(2));
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap()).collect()
}

fn id16(j: &J) -> [u8; 16] {
    ids::parse_uuid(j.as_str().unwrap()).unwrap()
}

fn props(j: &J) -> Vec<Prop> {
    j.as_array()
        .unwrap()
        .iter()
        .map(|p| {
            let v = &p["v"];
            let value = match p["t"].as_str().unwrap() {
                "null" => Value::Null,
                "bool" => Value::Bool(v.as_bool().unwrap()),
                "int" => Value::Int(v.as_str().unwrap().parse().unwrap()),
                "float" => Value::Float(match v.as_str().unwrap() {
                    "NaN" => f64::NAN,
                    "Infinity" => f64::INFINITY,
                    "-Infinity" => f64::NEG_INFINITY,
                    s => s.parse().unwrap(),
                }),
                "string" => Value::Str(v.as_str().unwrap().to_owned()),
                "json" => Value::Json(v.as_str().unwrap().to_owned()),
                t => panic!("unknown type {t}"),
            };
            Prop { key: p["k"].as_str().unwrap().to_owned(), value }
        })
        .collect()
}

fn logical(j: &J) -> (Header, Vec<Record>) {
    let h = Header {
        batch_id: id16(&j["batch_id"]),
        sent_at: j["sent_at"].as_u64().unwrap(),
        sdk: j["sdk"].as_str().unwrap().into(),
        anon_id: j["anon_id"].as_str().unwrap().into(),
        user_id: j["user_id"].as_str().unwrap().into(),
        session_id: id16(&j["session_id"]),
        context: props(&j["context"]),
    };
    let recs = j["records"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| Record {
            kind: r["kind"].as_u64().unwrap() as u8,
            uuid: id16(&r["uuid"]),
            ts: r["ts"].as_u64().unwrap(),
            name: r["name"].as_str().unwrap().into(),
            props: props(&r["props"]),
        })
        .collect();
    (h, recs)
}

/// Bitwise float comparison so NaN and -0.0 are checked exactly.
fn same_props(a: &[Prop], b: &[Prop]) -> bool {
    a.len() == b.len()
        && a.iter().zip(b).all(|(x, y)| {
            x.key == y.key
                && match (&x.value, &y.value) {
                    (Value::Float(p), Value::Float(q)) => p.to_bits() == q.to_bits(),
                    (p, q) => p == q,
                }
        })
}

fn assert_same(name: &str, got: &Batch, h: &Header, recs: &[Record]) {
    assert_eq!(got.header.batch_id, h.batch_id, "{name}");
    assert_eq!(
        (&got.header.sent_at, &got.header.sdk, &got.header.anon_id, &got.header.user_id, &got.header.session_id),
        (&h.sent_at, &h.sdk, &h.anon_id, &h.user_id, &h.session_id),
        "{name}"
    );
    assert!(same_props(&got.header.context, &h.context), "{name}: context");
    assert_eq!(got.records.len(), recs.len(), "{name}: record count");
    for (g, e) in got.records.iter().zip(recs) {
        assert_eq!((g.kind, g.uuid, g.ts, &g.name), (e.kind, e.uuid, e.ts, &e.name), "{name}");
        assert!(same_props(&g.props, &e.props), "{name}: props of {}", e.name);
    }
}

#[test]
fn wire_valid_encode_and_decode() {
    let v = load("wire.json");
    let cases = v["valid"].as_array().unwrap();
    assert!(cases.len() >= 6);
    for c in cases {
        let name = c["name"].as_str().unwrap();
        let (h, recs) = logical(&c["batch"]);
        let bytes = unhex(c["hex"].as_str().unwrap());
        assert_eq!(wire::encode_batch(&h, &recs), bytes, "{name}: encode");
        let info = wire::check_frame(&bytes).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!((info.batch_id, info.records), (h.batch_id, recs.len()), "{name}");
        let got = wire::decode_batch(&bytes).unwrap();
        assert!(got.rejected.is_empty() && got.unknown_kinds == 0, "{name}");
        assert_same(name, &got, &h, &recs);
    }
}

#[test]
fn wire_tolerant_decode() {
    let v = load("wire.json");
    for c in v["tolerant"].as_array().unwrap() {
        let name = c["name"].as_str().unwrap();
        let bytes = unhex(c["hex"].as_str().unwrap());
        let (h, mut recs) = logical(&c["batch"]);
        let mut got = wire::decode_batch(&bytes).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert!(got.rejected.is_empty(), "{name}");
        if name == "duplicate_key_last_wins" {
            // The list decoder keeps both; collapse to "last wins" like a map would.
            for r in got.records.iter_mut().chain(recs.iter_mut()) {
                let mut seen: Vec<Prop> = Vec::new();
                for p in r.props.drain(..) {
                    seen.retain(|q| q.key != p.key);
                    seen.push(p);
                }
                r.props = seen;
            }
        }
        assert_same(name, &got, &h, &recs);
    }
}

#[test]
fn wire_invalid() {
    let v = load("wire.json");
    let cases = v["invalid"].as_array().unwrap();
    assert!(cases.len() >= 20);
    for c in cases {
        let name = c["name"].as_str().unwrap();
        let bytes = unhex(c["hex"].as_str().unwrap());
        let status = c["http_status"].as_u64().unwrap() as u16;
        match wire::check_frame(&bytes) {
            Err(e) => {
                assert!(!c["frame_ok"].as_bool().unwrap(), "{name}: frame rejected ({e}) but expected ok");
                assert_eq!(e.http_status(), status, "{name}: {e}");
                assert!(wire::decode_batch(&bytes).is_err(), "{name}");
            }
            Ok(_) => {
                assert!(c["frame_ok"].as_bool().unwrap(), "{name}: frame accepted but expected reject");
                assert_eq!(status, 200, "{name}");
                match wire::decode_batch(&bytes) {
                    Err(wire::BatchError::Header(_)) => {
                        assert!(!c["header_ok"].as_bool().unwrap(), "{name}")
                    }
                    Err(e) => panic!("{name}: unexpected {e}"),
                    Ok(b) => {
                        assert!(c["header_ok"].as_bool().unwrap(), "{name}");
                        let bad: Vec<u64> = b.rejected.iter().map(|(i, _)| *i as u64).collect();
                        let want: Vec<u64> =
                            c["bad_records"].as_array().unwrap().iter().map(|x| x.as_u64().unwrap()).collect();
                        assert_eq!(bad, want, "{name}");
                    }
                }
            }
        }
    }
}

#[test]
fn clock_vectors() {
    let v = load("clock.json");
    assert_eq!(v["flag_bits"]["clock_corrected"], clock::FLAG_CLOCK_CORRECTED);
    assert_eq!(v["flag_bits"]["late"], clock::FLAG_LATE);
    for c in v["cases"].as_array().unwrap() {
        let g = |k: &str| c[k].as_i64().unwrap();
        let (ts, flags) = clock::correct(g("recv_ts"), g("sent_at"), g("ts"));
        assert_eq!((ts, flags as i64), (g("out_ts"), g("flags")), "{}", c["name"]);
    }
}

#[test]
fn murmur3_vectors() {
    let v = load("murmur3.json");
    for c in v["cases"].as_array().unwrap() {
        let s = c["input"].as_str().unwrap();
        let h = murmur3::murmur3_32(s.as_bytes(), 0);
        assert_eq!(h as u64, c["hash"].as_u64().unwrap(), "{s:?}");
        assert_eq!((h % 10000) as u64, c["bucket"].as_u64().unwrap(), "{s:?}");
    }
}

/// Cheap in-tree stand-in for a fuzzer: every decoder must be total on
/// mutated and truncated inputs. (`engine/fuzz` holds the real fuzz targets.)
#[test]
fn decoders_never_panic_on_mutations() {
    let v = load("wire.json");
    let mut seeds: Vec<Vec<u8>> = Vec::new();
    for k in ["valid", "tolerant", "invalid"] {
        for c in v[k].as_array().unwrap() {
            let b = unhex(c["hex"].as_str().unwrap());
            if b.len() < 4096 {
                seeds.push(b);
            }
        }
    }
    let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
    let mut next = move || {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        x
    };
    let mut decoded = 0u32;
    for seed in &seeds {
        for cut in 0..seed.len().min(200) {
            let _ = wire::decode_batch(&seed[..cut]);
        }
        for _ in 0..2000 {
            let mut m = seed.clone();
            if m.is_empty() {
                continue;
            }
            for _ in 0..1 + next() % 3 {
                let i = (next() % m.len() as u64) as usize;
                match next() % 3 {
                    0 => m[i] = next() as u8,
                    1 => m[i] ^= 1 << (next() % 8),
                    _ => {
                        m.truncate(i.max(1));
                    }
                }
            }
            if wire::decode_batch(&m).is_ok() {
                decoded += 1;
            }
            let _ = la_codec::wal::ReplayIndex::decode(&m);
            if let Ok(r) = la_codec::wal::SegmentReader::new(&m) {
                let _ = r.count();
            }
        }
    }
    assert!(decoded > 100, "mutations should often stay decodable (got {decoded})");
}
