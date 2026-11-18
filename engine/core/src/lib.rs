//! Storage contracts shared by the shipper, compactor and query service
//! (`spec/manifest.md`, `spec/parquet.md`). Components talk to each other only
//! through the objects described here.
#![forbid(unsafe_code)]

pub mod control;
pub mod error;
pub mod events;
pub mod identity;
pub mod keys;
pub mod lease;
pub mod manifest;
pub mod metrics;
pub mod parquet_io;
pub mod persons;
pub mod schema;
pub mod store;
pub mod time;
pub mod tombstone;

pub use error::{Error, Result};
pub use store::Store;
