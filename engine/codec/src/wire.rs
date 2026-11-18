//! LA1 batch codec (`spec/wire.md`).

use crate::reader::{put_str, put_varint, put_zigzag, DecodeError, Reader};
use thiserror::Error;

pub const MAGIC: [u8; 4] = *b"LA1\0";

pub const MAX_BATCH_BYTES: usize = 1 << 20;
pub const MAX_RECORDS: usize = 500;
pub const MAX_HEADER_BYTES: usize = 65_536;
pub const MAX_RECORD_BYTES: usize = 65_536;
pub const MIN_HEADER_BYTES: usize = 16;

// Content limits enforced by the shipper (`spec/wire.md` §3).
pub const MAX_PROPS: usize = 200;
pub const MAX_KEY_BYTES: usize = 128;
pub const MAX_VALUE_BYTES: usize = 8_192;
pub const MAX_NAME_BYTES: usize = 128;
pub const MAX_ID_BYTES: usize = 200;

pub const KIND_EVENT: u8 = 0;
pub const KIND_IDENTIFY: u8 = 1;
pub const KIND_SET_PERSON: u8 = 2;
pub const KIND_RESET: u8 = 3;

#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Int(i64),
    Float(f64),
    Str(String),
    /// Raw JSON text (array/object). Not validated by the codec.
    Json(String),
}

#[derive(Debug, Clone, PartialEq)]
pub struct Prop {
    pub key: String,
    pub value: Value,
}

impl Prop {
    pub fn new(key: impl Into<String>, value: Value) -> Self {
        Self { key: key.into(), value }
    }
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Header {
    pub batch_id: [u8; 16],
    /// Unix ms on the device clock; 0 = unknown.
    pub sent_at: u64,
    pub sdk: String,
    pub anon_id: String,
    /// Empty = not identified.
    pub user_id: String,
    /// All zero = none.
    pub session_id: [u8; 16],
    pub context: Vec<Prop>,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Record {
    pub kind: u8,
    pub uuid: [u8; 16],
    pub ts: u64,
    pub name: String,
    pub props: Vec<Prop>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Error)]
pub enum FrameError {
    #[error("bad magic")]
    BadMagic,
    #[error("malformed length prefix")]
    BadLength,
    #[error("header length out of bounds")]
    HeaderSize,
    #[error("record length out of bounds")]
    RecordSize,
    #[error("frame truncated")]
    Truncated,
    #[error("too many records")]
    TooManyRecords,
}

impl FrameError {
    /// HTTP status the ingestion hot path answers with.
    pub fn http_status(self) -> u16 {
        match self {
            FrameError::TooManyRecords => 413,
            _ => 400,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FrameInfo {
    pub batch_id: [u8; 16],
    pub records: usize,
}

fn frame_len(r: &mut Reader<'_>, min: usize, max: usize, size_err: FrameError) -> Result<usize, FrameError> {
    let n = match r.varint() {
        Ok(n) => n,
        Err(DecodeError::Eof) => return Err(FrameError::Truncated),
        Err(_) => return Err(FrameError::BadLength),
    };
    if n < min as u64 || n > max as u64 {
        return Err(size_err);
    }
    if n > r.remaining() as u64 {
        return Err(FrameError::Truncated);
    }
    Ok(n as usize)
}

/// Hot-path validation: walks length prefixes only, never looks inside
/// record bodies, performs no allocation.
pub fn check_frame(buf: &[u8]) -> Result<FrameInfo, FrameError> {
    if buf.len() < 4 || buf[..4] != MAGIC {
        return Err(FrameError::BadMagic);
    }
    let mut r = Reader::new(&buf[4..]);
    if r.is_empty() {
        return Err(FrameError::Truncated);
    }
    let hlen = frame_len(&mut r, MIN_HEADER_BYTES, MAX_HEADER_BYTES, FrameError::HeaderSize)?;
    let header = r.take(hlen).map_err(|_| FrameError::Truncated)?;
    let mut batch_id = [0u8; 16];
    batch_id.copy_from_slice(&header[..16]);
    let mut records = 0usize;
    while !r.is_empty() {
        let n = frame_len(&mut r, 1, MAX_RECORD_BYTES, FrameError::RecordSize)?;
        r.take(n).map_err(|_| FrameError::Truncated)?;
        records += 1;
        if records > MAX_RECORDS {
            return Err(FrameError::TooManyRecords);
        }
    }
    Ok(FrameInfo { batch_id, records })
}

fn decode_props(r: &mut Reader<'_>) -> Result<Vec<Prop>, DecodeError> {
    let count = r.varint()?;
    // Every property takes at least 2 bytes (empty key + tag).
    if count > (r.remaining() as u64) / 2 + 1 {
        return Err(DecodeError::LengthOverrun);
    }
    let mut out = Vec::with_capacity(count as usize);
    for _ in 0..count {
        let key = r.str()?.to_owned();
        let tag = r.u8()?;
        let value = match tag {
            0 => Value::Null,
            1 => Value::Bool(false),
            2 => Value::Bool(true),
            3 => Value::Int(r.zigzag()?),
            4 => Value::Float(r.f64_le()?),
            5 => Value::Str(r.str()?.to_owned()),
            6 => Value::Json(r.str()?.to_owned()),
            t => return Err(DecodeError::UnknownTag(t)),
        };
        out.push(Prop { key, value });
    }
    Ok(out)
}

pub fn decode_header(body: &[u8]) -> Result<Header, DecodeError> {
    let mut r = Reader::new(body);
    Ok(Header {
        batch_id: r.bytes16()?,
        sent_at: r.varint()?,
        sdk: r.str()?.to_owned(),
        anon_id: r.str()?.to_owned(),
        user_id: r.str()?.to_owned(),
        session_id: r.bytes16()?,
        context: decode_props(&mut r)?,
        // trailing bytes are ignored on purpose (forward compatibility)
    })
}

pub fn decode_record(body: &[u8]) -> Result<Record, DecodeError> {
    let mut r = Reader::new(body);
    Ok(Record {
        kind: r.u8()?,
        uuid: r.bytes16()?,
        ts: r.varint()?,
        name: r.str()?.to_owned(),
        props: decode_props(&mut r)?,
    })
}

#[derive(Debug, Clone, PartialEq)]
pub struct Batch {
    pub header: Header,
    /// Successfully decoded records of a known kind, in wire order.
    pub records: Vec<Record>,
    /// `(wire index, error)` for records whose body failed to decode.
    pub rejected: Vec<(usize, DecodeError)>,
    /// Records with a kind this decoder does not know (skipped, not an error).
    pub unknown_kinds: usize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Error)]
pub enum BatchError {
    #[error("frame: {0}")]
    Frame(#[from] FrameError),
    #[error("header: {0}")]
    Header(DecodeError),
}

/// Full decode. A bad header rejects the batch; a bad record only rejects
/// that record.
pub fn decode_batch(buf: &[u8]) -> Result<Batch, BatchError> {
    check_frame(buf)?;
    let mut r = Reader::new(&buf[4..]);
    // check_frame proved every length prefix, so these cannot fail.
    let hlen = r.len().map_err(|_| FrameError::Truncated)?;
    let header = decode_header(r.take(hlen).map_err(|_| FrameError::Truncated)?).map_err(BatchError::Header)?;
    let mut batch = Batch { header, records: Vec::new(), rejected: Vec::new(), unknown_kinds: 0 };
    let mut idx = 0usize;
    while !r.is_empty() {
        let n = r.len().map_err(|_| FrameError::Truncated)?;
        let body = r.take(n).map_err(|_| FrameError::Truncated)?;
        match decode_record(body) {
            Ok(rec) if rec.kind <= KIND_RESET => batch.records.push(rec),
            Ok(_) => batch.unknown_kinds += 1,
            // An unknown kind may carry a body we cannot parse; it is still "skipped".
            Err(_) if body[0] > KIND_RESET => batch.unknown_kinds += 1,
            Err(e) => batch.rejected.push((idx, e)),
        }
        idx += 1;
    }
    Ok(batch)
}

fn put_props(out: &mut Vec<u8>, props: &[Prop]) {
    put_varint(out, props.len() as u64);
    for p in props {
        put_str(out, &p.key);
        match &p.value {
            Value::Null => out.push(0),
            Value::Bool(false) => out.push(1),
            Value::Bool(true) => out.push(2),
            Value::Int(n) => {
                out.push(3);
                put_zigzag(out, *n);
            }
            Value::Float(f) => {
                out.push(4);
                out.extend_from_slice(&f.to_le_bytes());
            }
            Value::Str(s) => {
                out.push(5);
                put_str(out, s);
            }
            Value::Json(s) => {
                out.push(6);
                put_str(out, s);
            }
        }
    }
}

pub fn encode_header_body(h: &Header) -> Vec<u8> {
    let mut out = Vec::with_capacity(96);
    out.extend_from_slice(&h.batch_id);
    put_varint(&mut out, h.sent_at);
    put_str(&mut out, &h.sdk);
    put_str(&mut out, &h.anon_id);
    put_str(&mut out, &h.user_id);
    out.extend_from_slice(&h.session_id);
    put_props(&mut out, &h.context);
    out
}

pub fn encode_record_body(r: &Record) -> Vec<u8> {
    let mut out = Vec::with_capacity(64);
    out.push(r.kind);
    out.extend_from_slice(&r.uuid);
    put_varint(&mut out, r.ts);
    put_str(&mut out, &r.name);
    put_props(&mut out, &r.props);
    out
}

/// Encodes a batch. The caller is responsible for staying within the frame
/// limits; `check_frame` on the result tells whether a server would accept it.
pub fn encode_batch(header: &Header, records: &[Record]) -> Vec<u8> {
    let mut out = Vec::with_capacity(256);
    out.extend_from_slice(&MAGIC);
    let hb = encode_header_body(header);
    put_varint(&mut out, hb.len() as u64);
    out.extend_from_slice(&hb);
    for r in records {
        let rb = encode_record_body(r);
        put_varint(&mut out, rb.len() as u64);
        out.extend_from_slice(&rb);
    }
    out
}
