//! Compaction (L0→L1→L2), garbage collection, retention, WAL lifecycle and
//! person deletion for the events store (`docs/design.md` §10,
//! `spec/manifest.md` §9–§10). Driven by `la-shipper compact`.
#![forbid(unsafe_code)]

pub mod config;
pub mod describe;
pub mod gc;
pub mod inspect;
pub mod job;
pub mod json;
pub mod merge;
pub mod normalize;
pub mod run;
pub mod tombstones;
pub mod triggers;
pub mod wal;

pub use config::{CompactorConfig, Ctx};
