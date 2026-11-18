//! UUID helpers and the deterministic UUIDv8 derivations from `spec/wal.md`.

use crate::reader::put_varint;
use sha2::{Digest, Sha256};

pub const ZERO16: [u8; 16] = [0; 16];

/// Forces version 8 / RFC variant bits onto 16 arbitrary bytes.
pub fn uuid8(mut b: [u8; 16]) -> [u8; 16] {
    b[6] = (b[6] & 0x0f) | 0x80;
    b[8] = (b[8] & 0x3f) | 0x80;
    b
}

fn sha16(parts: &[&[u8]]) -> [u8; 16] {
    let mut h = Sha256::new();
    for p in parts {
        h.update(p);
    }
    let d = h.finalize();
    let mut out = [0u8; 16];
    out.copy_from_slice(&d[..16]);
    out
}

/// `uuid8(sha256("la-replay" ‖ session_id ‖ varint(seq))[0..16])`
pub fn replay_chunk_uuid(session_id: &[u8; 16], seq: u64) -> [u8; 16] {
    let mut v = Vec::with_capacity(10);
    put_varint(&mut v, seq);
    uuid8(sha16(&[b"la-replay", session_id, &v]))
}

/// `uuid8(sha256("la-ph" ‖ u32le(project_id) ‖ raw_event_bytes)[0..16])`
pub fn posthog_event_uuid(project_id: u32, raw_event: &[u8]) -> [u8; 16] {
    uuid8(sha16(&[b"la-ph", &project_id.to_le_bytes(), raw_event]))
}

pub fn hex32(b: &[u8; 16]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(32);
    for x in b {
        s.push(HEX[(x >> 4) as usize] as char);
        s.push(HEX[(x & 15) as usize] as char);
    }
    s
}

/// Parses 32 hex digits, with or without the canonical dashes.
pub fn parse_uuid(s: &str) -> Option<[u8; 16]> {
    let mut out = [0u8; 16];
    let mut n = 0usize;
    for c in s.bytes() {
        let v = match c {
            b'0'..=b'9' => c - b'0',
            b'a'..=b'f' => c - b'a' + 10,
            b'A'..=b'F' => c - b'A' + 10,
            b'-' => continue,
            _ => return None,
        };
        if n >= 32 {
            return None;
        }
        if n.is_multiple_of(2) {
            out[n / 2] = v << 4;
        } else {
            out[n / 2] |= v;
        }
        n += 1;
    }
    (n == 32).then_some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hex_roundtrip() {
        let u = parse_uuid("0193f1a2-b3c4-7d5e-8a9b-0c1d2e3f4a5b").unwrap();
        assert_eq!(hex32(&u), "0193f1a2b3c47d5e8a9b0c1d2e3f4a5b");
        assert_eq!(parse_uuid(&hex32(&u)), Some(u));
        assert_eq!(parse_uuid("xyz"), None);
        assert_eq!(parse_uuid("0193f1a2b3c47d5e8a9b0c1d2e3f4a5b00"), None);
        assert_eq!(parse_uuid("0193f1a2b3c47d5e8a9b0c1d2e3f4a"), None);
    }

    #[test]
    fn derived_ids_are_stable_v8() {
        let a = replay_chunk_uuid(&[1; 16], 0);
        assert_eq!(a, replay_chunk_uuid(&[1; 16], 0));
        assert_ne!(a, replay_chunk_uuid(&[1; 16], 1));
        assert_eq!(a[6] >> 4, 8);
        assert_eq!(a[8] >> 6, 0b10);
        let p = posthog_event_uuid(7, br#"{"event":"x"}"#);
        assert_ne!(p, posthog_event_uuid(8, br#"{"event":"x"}"#));
        assert_eq!(p[6] >> 4, 8);
    }
}
