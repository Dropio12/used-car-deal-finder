//! carbuyer Cloudflare Worker (workers-rs).
//!
//! Module map (one job each):
//! - [`sql`]       database-neutral statements (`Stmt`, `Param`)
//! - [`alerts`]    snipe alerts: the rule, statements, the `Notifier` port
//! - [`ingest`]    the upsert rules of the Go store, planned as statements
//! - [`deals`]     scoring stored listings with the `scorer` crate, ranking, filters
//! - [`snapshot`]  the daily deals snapshot (statements + read shape)
//! - [`auth`]      bearer-token check
//! - [`service`]   use cases over the `ListingStore` / `SnapshotStore` / `AlertStore` ports
//! - `d1`          D1 implementation of the ports (wasm only)
//! - `entry`       fetch + scheduled handlers, the composition root (wasm only)
//!
//! Everything except `d1` and `entry` is plain Rust, so `cargo test` runs it natively.

pub mod alerts;
pub mod auth;
pub mod deals;
pub mod ingest;
pub mod service;
pub mod snapshot;
pub mod sql;

#[cfg(target_arch = "wasm32")]
mod d1;
#[cfg(target_arch = "wasm32")]
mod entry;
