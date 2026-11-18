//! Bounds-checked byte cursor shared by all decoders.

use thiserror::Error;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Error)]
pub enum DecodeError {
    #[error("unexpected end of input")]
    Eof,
    #[error("varint longer than 10 bytes or overflowing u64")]
    VarintOverflow,
    #[error("length exceeds remaining input")]
    LengthOverrun,
    #[error("invalid utf-8")]
    Utf8,
    #[error("unknown property tag {0}")]
    UnknownTag(u8),
    #[error("bad magic")]
    BadMagic,
    #[error("value out of range")]
    OutOfRange,
}

/// Forward-only cursor over a byte slice.
#[derive(Debug, Clone)]
pub struct Reader<'a> {
    buf: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    pub fn new(buf: &'a [u8]) -> Self {
        Self { buf, pos: 0 }
    }

    pub fn remaining(&self) -> usize {
        self.buf.len() - self.pos
    }

    pub fn position(&self) -> usize {
        self.pos
    }

    pub fn is_empty(&self) -> bool {
        self.pos == self.buf.len()
    }

    pub fn u8(&mut self) -> Result<u8, DecodeError> {
        let b = *self.buf.get(self.pos).ok_or(DecodeError::Eof)?;
        self.pos += 1;
        Ok(b)
    }

    pub fn take(&mut self, n: usize) -> Result<&'a [u8], DecodeError> {
        if n > self.remaining() {
            return Err(DecodeError::LengthOverrun);
        }
        let s = &self.buf[self.pos..self.pos + n];
        self.pos += n;
        Ok(s)
    }

    pub fn bytes16(&mut self) -> Result<[u8; 16], DecodeError> {
        let s = self.take(16).map_err(|_| DecodeError::Eof)?;
        let mut out = [0u8; 16];
        out.copy_from_slice(s);
        Ok(out)
    }

    /// Unsigned LEB128. Non-minimal encodings are accepted; anything longer
    /// than 10 bytes or overflowing 64 bits is rejected.
    pub fn varint(&mut self) -> Result<u64, DecodeError> {
        let mut out: u64 = 0;
        for i in 0..10 {
            let b = self.u8()?;
            let low = (b & 0x7f) as u64;
            if i == 9 && low > 1 {
                return Err(DecodeError::VarintOverflow);
            }
            out |= low << (7 * i);
            if b & 0x80 == 0 {
                return Ok(out);
            }
        }
        Err(DecodeError::VarintOverflow)
    }

    pub fn zigzag(&mut self) -> Result<i64, DecodeError> {
        let n = self.varint()?;
        Ok(((n >> 1) as i64) ^ -((n & 1) as i64))
    }

    /// A varint that must describe a length within the remaining input.
    pub fn len(&mut self) -> Result<usize, DecodeError> {
        let n = self.varint()?;
        if n > self.remaining() as u64 {
            return Err(DecodeError::LengthOverrun);
        }
        Ok(n as usize)
    }

    pub fn str(&mut self) -> Result<&'a str, DecodeError> {
        let n = self.len()?;
        let raw = self.take(n)?;
        std::str::from_utf8(raw).map_err(|_| DecodeError::Utf8)
    }

    pub fn f64_le(&mut self) -> Result<f64, DecodeError> {
        let s = self.take(8).map_err(|_| DecodeError::Eof)?;
        let mut b = [0u8; 8];
        b.copy_from_slice(s);
        Ok(f64::from_le_bytes(b))
    }

    pub fn u32_le(&mut self) -> Result<u32, DecodeError> {
        let s = self.take(4).map_err(|_| DecodeError::Eof)?;
        Ok(u32::from_le_bytes([s[0], s[1], s[2], s[3]]))
    }

    pub fn u64_le(&mut self) -> Result<u64, DecodeError> {
        let s = self.take(8).map_err(|_| DecodeError::Eof)?;
        let mut b = [0u8; 8];
        b.copy_from_slice(s);
        Ok(u64::from_le_bytes(b))
    }
}

pub fn put_varint(out: &mut Vec<u8>, mut n: u64) {
    loop {
        let b = (n & 0x7f) as u8;
        n >>= 7;
        if n != 0 {
            out.push(b | 0x80);
        } else {
            out.push(b);
            return;
        }
    }
}

pub fn put_zigzag(out: &mut Vec<u8>, n: i64) {
    put_varint(out, ((n << 1) ^ (n >> 63)) as u64);
}

pub fn put_str(out: &mut Vec<u8>, s: &str) {
    put_varint(out, s.len() as u64);
    out.extend_from_slice(s.as_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn varint_roundtrip_edges() {
        for n in [0u64, 1, 127, 128, 300, 16383, 16384, u32::MAX as u64, u64::MAX - 1, u64::MAX] {
            let mut b = Vec::new();
            put_varint(&mut b, n);
            let mut r = Reader::new(&b);
            assert_eq!(r.varint().unwrap(), n);
            assert!(r.is_empty());
        }
    }

    #[test]
    fn zigzag_roundtrip_edges() {
        for n in [0i64, 1, -1, 63, 64, -64, -65, i64::MAX, i64::MIN] {
            let mut b = Vec::new();
            put_zigzag(&mut b, n);
            assert_eq!(Reader::new(&b).zigzag().unwrap(), n);
        }
    }

    #[test]
    fn varint_rejects_overflow() {
        // 11 continuation bytes
        assert_eq!(Reader::new(&[0x80; 11]).varint(), Err(DecodeError::VarintOverflow));
        // 10th byte carries more than the single remaining bit
        let mut b = vec![0xff; 9];
        b.push(0x02);
        assert_eq!(Reader::new(&b).varint(), Err(DecodeError::VarintOverflow));
        // unterminated
        assert_eq!(Reader::new(&[0x80]).varint(), Err(DecodeError::Eof));
    }

    #[test]
    fn non_minimal_varint_accepted() {
        assert_eq!(Reader::new(&[0x85, 0x00]).varint().unwrap(), 5);
    }
}
