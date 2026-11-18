//! WAL → Parquet shipper (`docs/design.md` §8, `spec/manifest.md` §6).
//!
//! * [`decode`] — the three WAL entry formats → records
//! * [`rows`] — records → `events` rows, schema statistics
//! * [`plan`] / [`exec`] / [`ship`] — plan protocol, execution, the loop
//! * [`rebuild`] — re-deriving `data/` from `wal/`
#![forbid(unsafe_code)]

pub mod config;
pub mod decode;
pub mod error;
pub mod exec;
pub mod http;
pub mod plan;
pub mod props;
pub mod rebuild;
pub mod rows;
pub mod ship;

pub use config::{ShipConfig, ShipCtx};
pub use error::{Result, ShipError};
