//! carbuyer scorer: the local price baseline and underpriced-listing scoring,
//! ported from the JavaScript `src/price-model.js`, `src/discount.js` and
//! `src/trim.js`.
//!
//! Module map (one job each):
//! - [`baseline`]    traits the scoring depends on (`Baseline`, `SellerDiscount`)
//! - [`price_model`] the dealer / private price baseline (`PriceModel`)
//! - [`discount`]    the measured private-party discount (`Discount`)
//! - [`trim`]        trim vocabulary induction and matching
//! - [`score`]       deal scores and the appraiser that wires it all together
//! - [`stats`], [`js`] medians and JavaScript-compatible number helpers

pub mod baseline;
pub mod discount;
pub mod js;
pub mod listing;
pub mod price_model;
pub mod score;
pub mod stats;
pub mod trim;
