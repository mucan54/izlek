//! LAW1 segment codec (`spec/wal.md`).

use crate::reader::{put_str, put_varint, DecodeError, Reader};

pub const SEG_MAGIC: [u8; 4] = *b"LAW1";
pub const ENVELOPE_LEN: usize = 16;
/// Upper bound for `len` (envelope + body) accepted by readers.
pub const MAX_ENTRY_LEN: usize = ENVELOPE_LEN + (1 << 20) + 4096;

pub const FORMAT_NATIVE: u8 = 1;
pub const FORMAT_POSTHOG: u8 = 2;
pub const FORMAT_REPLAY_INDEX: u8 = 3;

pub const ENV_FLAG_WEB: u8 = 1;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Envelope {
    pub format: u8,
    pub project_id: u32,
    pub recv_ts_ms: u64,
    /// ISO-3166-1 alpha-2, `ZZ` when unknown.
    pub country: [u8; 2],
    pub flags: u8,
}

impl Envelope {
    pub fn encode(&self) -> [u8; ENVELOPE_LEN] {
        let mut b = [0u8; ENVELOPE_LEN];
        b[0] = self.format;
        b[1..5].copy_from_slice(&self.project_id.to_le_bytes());
        b[5..13].copy_from_slice(&self.recv_ts_ms.to_le_bytes());
        b[13..15].copy_from_slice(&self.country);
        b[15] = self.flags;
        b
    }

    pub fn decode(b: &[u8]) -> Option<Self> {
        if b.len() < ENVELOPE_LEN {
            return None;
        }
        Some(Self {
            format: b[0],
            project_id: u32::from_le_bytes([b[1], b[2], b[3], b[4]]),
            recv_ts_ms: u64::from_le_bytes([b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12]]),
            country: [b[13], b[14]],
            flags: b[15],
        })
    }

    /// Country as a string, `None` for `ZZ` or non-ASCII garbage.
    pub fn country_str(&self) -> Option<&str> {
        let c = &self.country;
        if c == b"ZZ" || !c.iter().all(|b| b.is_ascii_uppercase()) {
            return None;
        }
        std::str::from_utf8(c).ok()
    }

    pub fn is_web(&self) -> bool {
        self.flags & ENV_FLAG_WEB != 0
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Entry<'a> {
    pub envelope: Envelope,
    pub body: &'a [u8],
}

/// Why iteration over a segment stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Tail {
    /// Every byte belonged to a valid entry.
    Clean,
    /// A torn or corrupt entry starts at this byte offset; everything before
    /// it is valid.
    CorruptAt(usize),
}

/// Iterates the valid prefix of a (decompressed) segment.
pub struct SegmentReader<'a> {
    buf: &'a [u8],
    pos: usize,
    tail: Option<Tail>,
}

impl<'a> SegmentReader<'a> {
    pub fn new(buf: &'a [u8]) -> Result<Self, DecodeError> {
        if buf.len() < 4 || buf[..4] != SEG_MAGIC {
            return Err(DecodeError::BadMagic);
        }
        Ok(Self { buf, pos: 4, tail: None })
    }

    /// `None` until the iterator is exhausted.
    pub fn tail(&self) -> Option<Tail> {
        self.tail
    }

    /// Length of the valid prefix seen so far (use to truncate a local file).
    pub fn valid_len(&self) -> usize {
        self.pos
    }
}

impl<'a> Iterator for SegmentReader<'a> {
    type Item = Entry<'a>;

    fn next(&mut self) -> Option<Entry<'a>> {
        if self.tail.is_some() {
            return None;
        }
        let rest = &self.buf[self.pos..];
        if rest.is_empty() {
            self.tail = Some(Tail::Clean);
            return None;
        }
        let bad = Some(Tail::CorruptAt(self.pos));
        if rest.len() < 8 {
            self.tail = bad;
            return None;
        }
        let len = u32::from_le_bytes([rest[0], rest[1], rest[2], rest[3]]) as usize;
        let crc = u32::from_le_bytes([rest[4], rest[5], rest[6], rest[7]]);
        if !(ENVELOPE_LEN..=MAX_ENTRY_LEN).contains(&len) || rest.len() - 8 < len {
            self.tail = bad;
            return None;
        }
        let payload = &rest[8..8 + len];
        if crc32c::crc32c(payload) != crc {
            self.tail = bad;
            return None;
        }
        // len >= ENVELOPE_LEN was checked above.
        let envelope = Envelope::decode(payload)?;
        self.pos += 8 + len;
        Some(Entry { envelope, body: &payload[ENVELOPE_LEN..] })
    }
}

/// In-memory segment builder (tests, tools, rebuild fixtures).
#[derive(Debug, Clone)]
pub struct SegmentWriter {
    buf: Vec<u8>,
}

impl Default for SegmentWriter {
    fn default() -> Self {
        Self::new()
    }
}

impl SegmentWriter {
    pub fn new() -> Self {
        Self { buf: SEG_MAGIC.to_vec() }
    }

    pub fn append(&mut self, env: &Envelope, body: &[u8]) {
        let e = env.encode();
        let len = (ENVELOPE_LEN + body.len()) as u32;
        let crc = crc32c::crc32c_append(crc32c::crc32c(&e), body);
        self.buf.extend_from_slice(&len.to_le_bytes());
        self.buf.extend_from_slice(&crc.to_le_bytes());
        self.buf.extend_from_slice(&e);
        self.buf.extend_from_slice(body);
    }

    pub fn len(&self) -> usize {
        self.buf.len()
    }

    pub fn is_empty(&self) -> bool {
        self.buf.len() == SEG_MAGIC.len()
    }

    pub fn finish(self) -> Vec<u8> {
        self.buf
    }
}

pub const REPLAY_ENC_ZSTD: u8 = 1;
pub const REPLAY_ENC_GZIP: u8 = 2;

/// Body of a `format = 3` entry (`spec/wal.md` §2.3).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReplayIndex {
    pub session_id: [u8; 16],
    pub seq: u64,
    pub start_ts: u64,
    pub duration_ms: u64,
    pub bytes: u64,
    pub encoding: u8,
    pub anon_id: String,
    pub user_id: String,
    pub key: String,
}

impl ReplayIndex {
    pub fn decode(body: &[u8]) -> Result<Self, DecodeError> {
        let mut r = Reader::new(body);
        Ok(Self {
            session_id: r.bytes16()?,
            seq: r.varint()?,
            start_ts: r.varint()?,
            duration_ms: r.varint()?,
            bytes: r.varint()?,
            encoding: r.u8()?,
            anon_id: r.str()?.to_owned(),
            user_id: r.str()?.to_owned(),
            key: r.str()?.to_owned(),
        })
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(64 + self.key.len());
        out.extend_from_slice(&self.session_id);
        put_varint(&mut out, self.seq);
        put_varint(&mut out, self.start_ts);
        put_varint(&mut out, self.duration_ms);
        put_varint(&mut out, self.bytes);
        out.push(self.encoding);
        put_str(&mut out, &self.anon_id);
        put_str(&mut out, &self.user_id);
        put_str(&mut out, &self.key);
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(p: u32) -> Envelope {
        Envelope { format: FORMAT_NATIVE, project_id: p, recv_ts_ms: 1_795_046_400_000, country: *b"TR", flags: 1 }
    }

    #[test]
    fn roundtrip_and_clean_tail() {
        let mut w = SegmentWriter::new();
        w.append(&env(1), b"hello");
        w.append(&env(2), b"");
        let seg = w.finish();
        let mut r = SegmentReader::new(&seg).unwrap();
        let a = r.next().unwrap();
        assert_eq!((a.envelope, a.body), (env(1), &b"hello"[..]));
        let b = r.next().unwrap();
        assert_eq!((b.envelope.project_id, b.body.len()), (2, 0));
        assert!(r.next().is_none());
        assert_eq!(r.tail(), Some(Tail::Clean));
        assert_eq!(r.valid_len(), seg.len());
        assert_eq!(a.envelope.country_str(), Some("TR"));
        assert!(a.envelope.is_web());
    }

    #[test]
    fn torn_tail_stops_at_last_good_entry() {
        let mut w = SegmentWriter::new();
        w.append(&env(1), b"first");
        let good = w.len();
        w.append(&env(1), b"second-entry");
        let seg = w.finish();
        for cut in good + 1..seg.len() {
            let mut r = SegmentReader::new(&seg[..cut]).unwrap();
            assert_eq!(r.by_ref().count(), 1, "cut={cut}");
            assert_eq!(r.tail(), Some(Tail::CorruptAt(good)));
            assert_eq!(r.valid_len(), good);
        }
    }

    #[test]
    fn bit_flip_is_detected() {
        let mut w = SegmentWriter::new();
        w.append(&env(1), b"payload");
        let mut seg = w.finish();
        let last = seg.len() - 1;
        seg[last] ^= 1;
        let mut r = SegmentReader::new(&seg).unwrap();
        assert!(r.next().is_none());
        assert_eq!(r.tail(), Some(Tail::CorruptAt(4)));
    }

    #[test]
    fn rejects_bad_magic_and_absurd_len() {
        assert!(SegmentReader::new(b"LAW2").is_err());
        let mut seg = SEG_MAGIC.to_vec();
        seg.extend_from_slice(&u32::MAX.to_le_bytes());
        seg.extend_from_slice(&[0; 64]);
        let mut r = SegmentReader::new(&seg).unwrap();
        assert!(r.next().is_none());
        assert_eq!(r.tail(), Some(Tail::CorruptAt(4)));
    }

    #[test]
    fn zz_country_is_none() {
        let mut e = env(1);
        e.country = *b"ZZ";
        assert_eq!(e.country_str(), None);
        e.country = [0xff, 0x00];
        assert_eq!(e.country_str(), None);
    }

    #[test]
    fn replay_index_roundtrip() {
        let idx = ReplayIndex {
            session_id: [7; 16],
            seq: 300,
            start_ts: 1_795_046_400_000,
            duration_ms: 5000,
            bytes: 12_345,
            encoding: REPLAY_ENC_GZIP,
            anon_id: "anon".into(),
            user_id: String::new(),
            key: "replay/p=1/d=2026-11-18/07070707070707070707070707070707/000300.bin.gz".into(),
        };
        assert_eq!(ReplayIndex::decode(&idx.encode()).unwrap(), idx);
        assert!(ReplayIndex::decode(&idx.encode()[..20]).is_err());
    }
}
