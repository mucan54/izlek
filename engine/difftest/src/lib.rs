//! Differential testing of the query service (design §17.3): a seeded data
//! generator, reference implementations in plain DataFusion SQL, a runner
//! that fails loudly on any mismatch, and the benchmarks for the design's
//! measured assumptions (§19).
#![forbid(unsafe_code)]

pub mod bench;
pub mod gen;
pub mod model;
pub mod reference;
pub mod runner;
