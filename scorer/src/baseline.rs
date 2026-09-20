//! The abstractions the scoring code depends on.
//!
//! `score.rs` never names a concrete model type: it asks a [`Baseline`] for an
//! estimate and a [`SellerDiscount`] for the private-party ratio. A different
//! baseline (another region's market, a model trained some other way) can be
//! dropped in without touching the scoring rules.

use serde_json::{Map, Value};

use crate::js::{json_num, json_opt_num};

/// Which market a baseline's prices describe.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Market {
    /// Dealer asking prices: a private car needs the private-party discount applied.
    Dealer,
    /// Private asking prices already: no discount applies, or it would be counted twice.
    Private,
}

impl Market {
    pub fn as_str(self) -> &'static str {
        match self {
            Market::Dealer => "dealer",
            Market::Private => "private",
        }
    }
}

/// What the scorer needs to know about one car to price it.
#[derive(Debug, Clone, Default)]
pub struct Query<'a> {
    pub make: Option<&'a str>,
    pub model: Option<&'a str>,
    pub year: Option<f64>,
    pub km: Option<f64>,
    pub trim: Option<&'a str>,
    pub trim_text: Option<&'a str>,
    pub province: Option<&'a str>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct TrimFactor {
    pub factor: f64,
    pub years: usize,
    pub n: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SellerAdjustment {
    pub ratio: f64,
    pub discount_pct: Option<f64>,
    pub n: usize,
}

/// One priced answer from a baseline (the object `estimate()` returns in JS).
#[derive(Debug, Clone, PartialEq)]
pub struct Estimate {
    pub baseline: f64,
    pub low: Option<f64>,
    pub high: Option<f64>,
    pub n: usize,
    pub basis: String,
    pub trim_factor: Option<TrimFactor>,
    pub trim: Option<String>,
    pub km_adjustment: f64,
    pub km_slope: Option<f64>,
    pub median_km: Option<f64>,
    pub spread: f64,
    pub warnings: Vec<String>,
    /// False when the number exists but should not be acted on.
    pub reliable: bool,
    pub years_pooled: Option<usize>,
    pub dealer_baseline: Option<f64>,
    pub seller_adjustment: Option<SellerAdjustment>,
}

impl Estimate {
    /// JSON with the same keys (and key omissions) as the JS object.
    pub fn to_json(&self) -> Map<String, Value> {
        let mut m = Map::new();
        m.insert("baseline".into(), json_num(self.baseline));
        m.insert("low".into(), json_opt_num(self.low));
        m.insert("high".into(), json_opt_num(self.high));
        m.insert("n".into(), Value::from(self.n));
        m.insert("basis".into(), Value::from(self.basis.clone()));
        m.insert(
            "trimFactor".into(),
            self.trim_factor.as_ref().map_or(Value::Null, |t| {
                let mut f = Map::new();
                f.insert("factor".into(), json_num(t.factor));
                f.insert("years".into(), Value::from(t.years));
                f.insert("n".into(), Value::from(t.n));
                Value::Object(f)
            }),
        );
        m.insert("trim".into(), self.trim.clone().map_or(Value::Null, Value::from));
        m.insert("kmAdjustment".into(), json_num(self.km_adjustment));
        m.insert("kmSlope".into(), json_opt_num(self.km_slope));
        m.insert("medianKm".into(), json_opt_num(self.median_km));
        m.insert("spread".into(), json_num(self.spread));
        m.insert("warnings".into(), Value::from(self.warnings.clone()));
        m.insert("reliable".into(), Value::from(self.reliable));
        if let Some(y) = self.years_pooled {
            m.insert("yearsPooled".into(), Value::from(y));
        }
        if let Some(d) = self.dealer_baseline {
            m.insert("dealerBaseline".into(), json_num(d));
        }
        if let Some(s) = &self.seller_adjustment {
            let mut a = Map::new();
            a.insert("ratio".into(), json_num(s.ratio));
            a.insert("discountPct".into(), json_opt_num(s.discount_pct));
            a.insert("n".into(), Value::from(s.n));
            m.insert("sellerAdjustment".into(), Value::Object(a));
        }
        m
    }
}

/// The limits a baseline enforces; the scorer reads them too.
#[derive(Debug, Clone, PartialEq)]
pub struct Limits {
    /// Below this, a price is a placeholder ("1 $" = call us), not an asking price.
    pub min_plausible_price: f64,
    /// A "discount" beyond this is a broken price (a monthly payment), not a bargain.
    pub max_plausible_discount: f64,
}

/// Something that can say what a car should cost.
pub trait Baseline {
    fn market(&self) -> Market;
    fn limits(&self) -> Limits;
    /// `None` means there is nothing defensible to say — never a guess.
    fn estimate(&self, query: &Query) -> Option<Estimate>;
}

/// A measured private-party ratio for one model (or the global one).
#[derive(Debug, Clone, PartialEq)]
pub struct RatioSummary {
    pub ratio: f64,
    pub discount_pct: Option<f64>,
    pub n: usize,
    pub p25: Option<f64>,
    pub p75: Option<f64>,
}

/// Where the scorer looks up how far private sellers ask below dealers.
pub trait SellerDiscount {
    /// Per-model ratio first, then the global one, else `None`.
    fn ratio_for(&self, make: Option<&str>, model: Option<&str>) -> Option<&RatioSummary>;
}
