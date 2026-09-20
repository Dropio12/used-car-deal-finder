//! Turning a baseline into a deal score. Port of `scoreListing` /
//! `scoreAgainst` from `src/price-model.js` and `buildAppraiser` from
//! `src/watch.js` (AutoHebdo-only inputs, same rules).
//!
//! Depends only on the [`Baseline`] and [`SellerDiscount`] traits, never on a
//! concrete model type.

use serde_json::{Map, Value};

use crate::baseline::{Baseline, Estimate, Market, SellerAdjustment, SellerDiscount};
use crate::discount::{measure_discount, query_for, Discount, DiscountConfig};
use crate::js::{json_num, round, truthy};
use crate::listing::Listing;
use crate::price_model::{ModelConfig, PriceModel};

/// A scored listing: the estimate plus how far under it the asking price sits.
#[derive(Debug, Clone, PartialEq)]
pub struct Score {
    pub estimate: Estimate,
    pub price: f64,
    pub delta: f64,
    /// Positive means cheaper than comparable cars.
    pub discount_pct: f64,
    /// Below the 25th percentile of its own bucket, not just below the median.
    pub below_p25: bool,
    /// Measured against undamaged comps, so part of the gap is the damage.
    pub damaged: bool,
    /// Priced on fewer comps than the floor normally allows.
    pub thin: bool,
    pub market: Option<Market>,
}

impl Score {
    pub fn to_json(&self) -> Value {
        let mut m = self.estimate.to_json();
        m.insert("price".into(), json_num(self.price));
        m.insert("delta".into(), json_num(self.delta));
        m.insert("discountPct".into(), json_num(self.discount_pct));
        m.insert("belowP25".into(), Value::from(self.below_p25));
        m.insert("damaged".into(), Value::from(self.damaged));
        m.insert("thin".into(), Value::from(self.thin));
        if let Some(market) = self.market {
            m.insert("market".into(), Value::from(market.as_str()));
        }
        Value::Object(m)
    }
}

/// How far below one baseline a listing is priced, or `None` if it cannot be scored.
pub fn score_listing(model: &dyn Baseline, listing: &Listing, discount: Option<&dyn SellerDiscount>) -> Option<Score> {
    let limits = model.limits();
    let price = listing.price?;
    // "1 $" is a dealer placeholder for "call us", not a bargain.
    if price < limits.min_plausible_price {
        return None;
    }
    if truthy(&listing.is_conditional_price) {
        return None;
    }
    // A donor car is worth its parts, not its comps.
    if truthy(&listing.is_parts) {
        return None;
    }

    let mut comps = model.estimate(&query_for(listing))?;
    // An unreliable baseline must never become a deal score.
    if !comps.reliable {
        return None;
    }

    // A private asking price belongs to a different market than a dealer's.
    if listing.is_seller("PrivateSeller") && model.market() != Market::Private {
        let seller = discount.and_then(|d| d.ratio_for(listing.make.as_deref(), listing.model.as_deref()))?;
        let dealer_baseline = comps.baseline;
        comps.baseline = round(comps.baseline * seller.ratio);
        comps.low = comps.low.map(|v| round(v * seller.ratio));
        comps.high = comps.high.map(|v| round(v * seller.ratio));
        comps.dealer_baseline = Some(dealer_baseline);
        comps.seller_adjustment = Some(SellerAdjustment { ratio: seller.ratio, discount_pct: seller.discount_pct, n: seller.n });
    }

    let delta = comps.baseline - price;
    let under = delta / comps.baseline;
    // Beyond this the price is a payment amount, a typo, or a placeholder.
    if under > limits.max_plausible_discount {
        return None;
    }

    let below_p25 = comps.low.is_some_and(|low| price < low);
    let thin = comps.basis.ends_with('*');
    Some(Score {
        estimate: comps,
        price,
        delta,
        discount_pct: round(under * 1000.0) / 10.0,
        below_p25,
        damaged: listing.is_damaged == Value::Bool(true),
        thin,
        market: None,
    })
}

/// Score against the first baseline that can price this car.
pub fn score_against(models: &[&dyn Baseline], listing: &Listing, discount: Option<&dyn SellerDiscount>) -> Option<Score> {
    models.iter().find_map(|m| {
        score_listing(*m, listing, discount).map(|mut s| {
            s.market = Some(m.market());
            s
        })
    })
}

/// Options the binary accepts, applied like the JS `options` objects.
#[derive(Debug, Clone)]
pub struct AppraiserOptions {
    pub model: Value,
    pub discount: Value,
    pub private_sources: Vec<String>,
}

impl Default for AppraiserOptions {
    fn default() -> Self {
        AppraiserOptions {
            model: Value::Object(Map::new()),
            discount: Value::Object(Map::new()),
            private_sources: ["autohebdo", "kijiji", "lespac", "marketplace"].iter().map(|s| s.to_string()).collect(),
        }
    }
}

impl AppraiserOptions {
    pub fn from_json(v: &Value) -> Self {
        let mut o = AppraiserOptions::default();
        if let Some(m) = v.get("model").filter(|m| m.is_object()) {
            o.model = m.clone();
        }
        if let Some(d) = v.get("discount").filter(|d| d.is_object()) {
            o.discount = d.clone();
        }
        if let Some(s) = v.get("privateSources").and_then(Value::as_array) {
            o.private_sources = s.iter().filter_map(Value::as_str).map(str::to_string).collect();
        }
        o
    }
}

/// Dealer baseline + per-source private discount + pooled private baseline,
/// exactly as `buildAppraiser` wires them.
pub struct Appraiser {
    pub dealer: PriceModel,
    pub private: PriceModel,
    pub discounts: Vec<(String, Discount)>,
}

impl Appraiser {
    pub fn build(listings: &[Listing], options: &AppraiserOptions) -> Appraiser {
        let dealer_rows: Vec<Listing> = listings
            .iter()
            .filter(|l| l.is_seller("Dealer") && l.source.as_deref() == Some("autohebdo"))
            .cloned()
            .collect();
        let dealer = PriceModel::build(&dealer_rows, ModelConfig::default().with_overrides(&options.model));

        let mut discounts = Vec::new();
        let mut all_private: Vec<Listing> = Vec::new();
        for source in &options.private_sources {
            let rows: Vec<Listing> = listings
                .iter()
                .filter(|l| l.is_seller("PrivateSeller") && l.source.as_deref() == Some(source.as_str()))
                .cloned()
                .collect();
            if rows.is_empty() {
                continue;
            }
            // The discount stays per source: each site's sellers ask differently.
            let d = measure_discount(&dealer, &rows, DiscountConfig::default().with_overrides(&options.discount));
            discounts.push((source.clone(), d));
            all_private.extend(rows);
        }

        let mut private_options = options.model.clone();
        if let Some(obj) = private_options.as_object_mut() {
            obj.insert("market".into(), Value::from("private"));
        }
        let private = PriceModel::build(&all_private, ModelConfig::default().with_overrides(&private_options));
        Appraiser { dealer, private, discounts }
    }

    /// `None` when there is no trustworthy baseline for this car.
    pub fn score(&self, listing: &Listing) -> Option<Score> {
        let discount = listing
            .source
            .as_deref()
            .and_then(|s| self.discounts.iter().find(|(name, _)| name == s))
            .map(|(_, d)| d);
        match discount {
            None => score_against(&[&self.dealer], listing, None),
            Some(d) => score_against(&[&self.dealer, &self.private], listing, Some(d)),
        }
    }

    pub fn summary_json(&self) -> Value {
        let mut m = Map::new();
        m.insert("sources".into(), Value::from(self.discounts.iter().map(|(s, _)| s.clone()).collect::<Vec<_>>()));
        m.insert("dealerListings".into(), Value::from(self.dealer.coverage.usable));
        m.insert("privateListings".into(), Value::from(self.private.coverage.usable));
        m.insert("dealerCoverage".into(), self.dealer.coverage_json());
        m.insert("privateCoverage".into(), self.private.coverage_json());
        let mut d = Map::new();
        for (source, discount) in &self.discounts {
            d.insert(source.clone(), discount.to_json());
        }
        m.insert("discounts".into(), Value::Object(d));
        Value::Object(m)
    }
}

/// Score a batch: the whole job of the binary, minus I/O.
pub fn run(input: &Value) -> Result<Value, String> {
    let (rows, options) = match input {
        Value::Array(rows) => (rows.clone(), AppraiserOptions::default()),
        Value::Object(obj) => {
            let rows = obj
                .get("listings")
                .and_then(Value::as_array)
                .cloned()
                .ok_or("input object needs a \"listings\" array")?;
            let options = obj.get("options").map(AppraiserOptions::from_json).unwrap_or_default();
            (rows, options)
        }
        _ => return Err("input must be a JSON array of listings or {\"listings\": [...], \"options\": {...}}".into()),
    };

    let listings: Vec<Listing> = rows.iter().map(Listing::from_value).collect();
    let appraiser = Appraiser::build(&listings, &options);

    let scores: Vec<Value> = listings
        .iter()
        .map(|l| {
            let mut m = Map::new();
            m.insert("id".into(), l.id.clone());
            m.insert("score".into(), appraiser.score(l).map_or(Value::Null, |s| s.to_json()));
            Value::Object(m)
        })
        .collect();

    let mut out = Map::new();
    out.insert("appraiser".into(), appraiser.summary_json());
    out.insert("scores".into(), Value::Array(scores));
    Ok(Value::Object(out))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn cars(n: usize, year: i64, base: f64, seller: &str, prefix: &str) -> Vec<Listing> {
        (0..n)
            .map(|i| {
                let km = 60_000.0 + i as f64 * 5_000.0;
                Listing::from_value(&json!({
                    "id": format!("{prefix}{i}"), "source": "autohebdo", "make": "Toyota", "model": "RAV4",
                    "year": year, "km": km, "price": round(base - 0.1 * (km - 60_000.0)),
                    "trimText": "XLE", "province": "QC", "sellerType": seller, "isDamaged": false
                }))
            })
            .collect()
    }

    fn car(price: f64) -> Listing {
        Listing::from_value(&json!({
            "id": "x", "source": "autohebdo", "make": "Toyota", "model": "RAV4", "year": 2019, "km": 60000,
            "price": price, "trimText": "XLE", "province": "QC", "sellerType": "Dealer"
        }))
    }

    fn model() -> PriceModel {
        PriceModel::build(&cars(12, 2019, 30_000.0, "Dealer", "d"), ModelConfig::default())
    }

    #[test]
    fn reports_how_far_under_the_baseline() {
        let s = score_listing(&model(), &car(24_000.0), None).unwrap();
        assert!(s.delta > 0.0);
        assert!(s.discount_pct > 15.0 && s.discount_pct < 25.0);
        assert!(s.below_p25);
        let over = score_listing(&model(), &car(40_000.0), None).unwrap();
        assert!(over.discount_pct < 0.0);
        assert!(!over.below_p25);
    }

    #[test]
    fn refuses_placeholders_payments_parts_and_conditional_prices() {
        let m = model();
        assert!(score_listing(&m, &car(999.0), None).is_none());
        assert!(score_listing(&m, &car(1_224.0), None).is_none());
        let mut parts = car(12_000.0);
        parts.is_parts = Value::Bool(true);
        assert!(score_listing(&m, &parts, None).is_none());
        let mut cond = car(24_000.0);
        cond.is_conditional_price = Value::Bool(true);
        assert!(score_listing(&m, &cond, None).is_none());
        assert!(score_listing(&m, &car(21_000.0), None).is_some());
    }

    #[test]
    fn damaged_cars_score_but_are_labelled() {
        let mut c = car(20_000.0);
        c.is_damaged = Value::Bool(true);
        assert!(score_listing(&model(), &c, None).unwrap().damaged);
        assert!(!score_listing(&model(), &car(20_000.0), None).unwrap().damaged);
    }

    #[test]
    fn private_cars_need_a_discount_against_dealer_comps() {
        let mut c = car(24_000.0);
        c.seller_type = Some("PrivateSeller".into());
        assert!(score_listing(&model(), &c, None).is_none());
    }

    #[test]
    fn appraiser_prefers_dealer_and_falls_back_to_private() {
        let mut rows = cars(20, 2019, 30_000.0, "Dealer", "d");
        rows.extend(cars(20, 2019, 25_500.0, "PrivateSeller", "p"));
        rows.extend(cars(20, 2019, 25_500.0, "PrivateSeller", "q"));
        rows.extend(cars(12, 2009, 12_000.0, "PrivateSeller", "old"));
        let a = Appraiser::build(&rows, &AppraiserOptions::default());
        let mut newer = rows[25].clone();
        newer.price = Some(20_000.0);
        let s = a.score(&newer).unwrap();
        assert_eq!(s.market, Some(Market::Dealer));
        assert!(s.estimate.seller_adjustment.is_some());
        let mut old = rows[rows.len() - 6].clone();
        old.price = Some(9_000.0);
        let s = a.score(&old).unwrap();
        assert_eq!(s.market, Some(Market::Private));
        assert!(s.estimate.seller_adjustment.is_none());
    }

    #[test]
    fn run_accepts_array_or_object_and_rejects_junk() {
        let rows: Vec<Value> = (0..3).map(|i| json!({"id": i})).collect();
        let out = run(&Value::Array(rows.clone())).unwrap();
        assert_eq!(out["scores"].as_array().unwrap().len(), 3);
        let out = run(&json!({"listings": rows, "options": {"model": {"minComps": 3}}})).unwrap();
        assert!(out["scores"][0]["score"].is_null());
        assert!(run(&json!("nope")).is_err());
    }
}
