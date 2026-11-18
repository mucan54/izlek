//! Pure, allocation-light codecs for the platform's binary contracts.
//!
//! * [`wire`]   — LA1 batches (`spec/wire.md`)
//! * [`wal`]    — LAW1 segments (`spec/wal.md`)
//! * [`clock`]  — clock-skew correction (`spec/wire.md` §5)
//! * [`ids`]    — deterministic UUIDv8 derivations and hex helpers
//! * [`murmur3`]— murmur3_32, used for shard assignment and flag buckets
//!
//! Every decoder here is total: arbitrary input yields `Ok` or `Err`, never a
//! panic, and never allocates more than a small multiple of the input size.
#![forbid(unsafe_code)]

pub mod clock;
pub mod ids;
pub mod murmur3;
pub mod reader;
pub mod wal;
pub mod wire;

pub use reader::{DecodeError, Reader};
