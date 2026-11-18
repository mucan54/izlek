//! Query service of the analytics platform (`docs/design.md` §11,
//! `spec/query.md`): scan layer over the day manifests, streaming funnel /
//! retention / trends operators, restricted SQL and the HTTP API.
#![forbid(unsafe_code)]

pub mod config;
pub mod error;
pub mod filecache;
pub mod filter;
pub mod funnel;
pub mod http;
pub mod intern;
pub mod json;
pub mod q_events;
pub mod q_funnel;
pub mod q_misc;
pub mod q_retention;
pub mod q_sql;
pub mod q_trends;
pub mod request;
pub mod scan;
pub mod service;
pub mod session;
pub mod state;
pub mod udf;
