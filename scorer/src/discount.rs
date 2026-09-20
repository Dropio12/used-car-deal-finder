//! The private-party discount. Port of `src/discount.js`.
//!
//! Each private listing is priced against the *dealer* baseline and
//! contributes one ratio: private asking price / what a dealer would ask.
//! Per-model medians are used when a model has enough samples, else the
//! global median, else nothing (a private car then cannot be scored).

use std::collections::HashMap;

use serde_json::{Map, Value};

use crate::baseline::{Baseline, Query, RatioSummary, SellerDiscount};
use crate::js::{json_num, json_opt_num, opt_str, round, truthy};
use crate::listing::Listing;
use crate::stats::{median, percentile};

#[derive(Debug, Clone, PartialEq)]
pub struct DiscountConfig {
    /// Samples required before a per-model figure replaces the global one.
    pub min_model_samples: f64,
    /// Samples required before any discount is reported at all.
    pub min_global_samples: f64,
    /// Ratios outside this band are data errors, not market signal.
    pub plausible_ratio: (f64, f64),
}

impl Default for DiscountConfig {
    fn default() -> Self {
        DiscountConfig { min_model_samples: 15.0, min_global_samples: 30.0, plausible_ratio: (0.3, 1.5) }
    }
}

impl DiscountConfig {
    pub fn with_overrides(mut self, options: &Value) -> Self {
        if let Some(v) = options.get("minModelSamples").and_then(Value::as_f64) {
            self.min_model_samples = v;
        }
        if let Some(v) = options.get("minGlobalSamples").and_then(Value::as_f64) {
            self.min_global_samples = v;
        }
        if let Some(b) = options.get("plausibleRatio").and_then(Value::as_array) {
            if let (Some(lo), Some(hi)) = (b.first().and_then(Value::as_f64), b.get(1).and_then(Value::as_f64)) {
                self.plausible_ratio = (lo, hi);
            }
        }
        self
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct Sample {
    pub id: Value,
    pub make: Option<String>,
    pub model: Option<String>,
    pub ratio: f64,
    pub basis: String,
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct Rejected {
    pub unpriceable: usize,
    pub implausible_price: usize,
    pub outlier_ratio: usize,
    pub damaged: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct GroupSummary {
    pub n: usize,
    pub median_year: Option<f64>,
    pub median_km: Option<f64>,
    pub median_price: Option<f64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Discount {
    pub config: DiscountConfig,
    pub global: Option<RatioSummary>,
    pub by_model: HashMap<String, RatioSummary>,
    by_model_order: Vec<String>,
    pub samples: Vec<Sample>,
    pub rejected: Rejected,
    pub basis_mix: Vec<(String, usize)>,
    pub matched: GroupSummary,
    pub unmatched: GroupSummary,
    pub considered: usize,
}

fn summarize_ratios(ratios: &[f64]) -> RatioSummary {
    let value = median(ratios).unwrap_or(f64::NAN);
    RatioSummary {
        ratio: value,
        discount_pct: if value.is_nan() { None } else { Some(round((1.0 - value) * 1000.0) / 10.0) },
        n: ratios.len(),
        p25: percentile(ratios, 25.0),
        p75: percentile(ratios, 75.0),
    }
}

fn describe(rows: &[&Listing]) -> GroupSummary {
    let col = |f: fn(&Listing) -> Option<f64>| -> Vec<f64> { rows.iter().filter_map(|r| f(r)).collect() };
    GroupSummary {
        n: rows.len(),
        median_year: median(&col(|l| l.year)),
        median_km: median(&col(|l| l.km)),
        median_price: median(&col(|l| l.price)),
    }
}

/// Measure how far private asking prices sit below dealer asking prices.
pub fn measure_discount(dealer: &dyn Baseline, private_listings: &[Listing], config: DiscountConfig) -> Discount {
    let (min_ratio, max_ratio) = config.plausible_ratio;
    let limits = dealer.limits();
    let mut samples = Vec::new();
    let mut rejected = Rejected::default();

    for l in private_listings {
        let price = match l.price {
            Some(p) if p >= limits.min_plausible_price && !truthy(&l.is_conditional_price) => p,
            _ => {
                rejected.implausible_price += 1;
                continue;
            }
        };
        // A wreck is cheap because it is a wreck; it must not calibrate the discount.
        if truthy(&l.is_damaged) || truthy(&l.is_parts) {
            rejected.damaged += 1;
            continue;
        }
        let comps = match dealer.estimate(&query_for(l)) {
            Some(c) if c.reliable => c,
            _ => {
                rejected.unpriceable += 1;
                continue;
            }
        };
        let ratio = price / comps.baseline;
        if ratio < min_ratio || ratio > max_ratio {
            rejected.outlier_ratio += 1;
            continue;
        }
        samples.push(Sample { id: l.id.clone(), make: l.make.clone(), model: l.model.clone(), ratio, basis: comps.basis });
    }

    let mut order: Vec<String> = Vec::new();
    let mut grouped: HashMap<String, Vec<f64>> = HashMap::new();
    for s in &samples {
        let key = format!("{}|{}", opt_str(s.make.as_deref()), opt_str(s.model.as_deref()));
        if !grouped.contains_key(&key) {
            order.push(key.clone());
        }
        grouped.entry(key).or_default().push(s.ratio);
    }
    let mut by_model = HashMap::new();
    let mut by_model_order = Vec::new();
    for key in order {
        let ratios = &grouped[&key];
        if ratios.len() as f64 >= config.min_model_samples {
            by_model.insert(key.clone(), summarize_ratios(ratios));
            by_model_order.push(key);
        }
    }

    let global = if samples.len() as f64 >= config.min_global_samples {
        Some(summarize_ratios(&samples.iter().map(|s| s.ratio).collect::<Vec<_>>()))
    } else {
        None
    };

    let mut basis_mix: Vec<(String, usize)> = Vec::new();
    for s in &samples {
        match basis_mix.iter_mut().find(|(b, _)| *b == s.basis) {
            Some(entry) => entry.1 += 1,
            None => basis_mix.push((s.basis.clone(), 1)),
        }
    }

    let matched_ids: Vec<&Value> = samples.iter().map(|s| &s.id).collect();
    let (matched, unmatched): (Vec<&Listing>, Vec<&Listing>) =
        private_listings.iter().partition(|l| matched_ids.contains(&&l.id));

    Discount {
        config,
        global,
        by_model,
        by_model_order,
        samples,
        rejected,
        basis_mix,
        matched: describe(&matched),
        unmatched: describe(&unmatched),
        considered: private_listings.len(),
    }
}

pub fn query_for(l: &Listing) -> Query<'_> {
    Query {
        make: l.make.as_deref(),
        model: l.model.as_deref(),
        year: l.year,
        km: l.km,
        trim: l.trim.as_deref(),
        trim_text: l.trim_text.as_deref(),
        province: l.province.as_deref(),
    }
}

impl SellerDiscount for Discount {
    fn ratio_for(&self, make: Option<&str>, model: Option<&str>) -> Option<&RatioSummary> {
        self.by_model
            .get(&format!("{}|{}", opt_str(make), opt_str(model)))
            .or(self.global.as_ref())
    }
}

fn ratio_json(r: &RatioSummary) -> Value {
    let mut m = Map::new();
    m.insert("ratio".into(), json_num(r.ratio));
    m.insert("discountPct".into(), json_opt_num(r.discount_pct));
    m.insert("n".into(), Value::from(r.n));
    m.insert("p25".into(), json_opt_num(r.p25));
    m.insert("p75".into(), json_opt_num(r.p75));
    Value::Object(m)
}

fn group_json(g: &GroupSummary) -> Value {
    let mut m = Map::new();
    m.insert("n".into(), Value::from(g.n));
    m.insert("medianYear".into(), json_opt_num(g.median_year));
    m.insert("medianKm".into(), json_opt_num(g.median_km));
    m.insert("medianPrice".into(), json_opt_num(g.median_price));
    Value::Object(m)
}

impl Discount {
    /// Summary JSON (everything except the per-sample list), same keys as JS.
    pub fn to_json(&self) -> Value {
        let mut m = Map::new();
        m.insert("global".into(), self.global.as_ref().map_or(Value::Null, ratio_json));
        let mut by_model = Map::new();
        for k in &self.by_model_order {
            by_model.insert(k.clone(), ratio_json(&self.by_model[k]));
        }
        m.insert("byModel".into(), Value::Object(by_model));
        m.insert("samples".into(), Value::from(self.samples.len()));
        let mut rej = Map::new();
        rej.insert("unpriceable".into(), Value::from(self.rejected.unpriceable));
        rej.insert("implausiblePrice".into(), Value::from(self.rejected.implausible_price));
        rej.insert("outlierRatio".into(), Value::from(self.rejected.outlier_ratio));
        rej.insert("damaged".into(), Value::from(self.rejected.damaged));
        m.insert("rejected".into(), Value::Object(rej));
        let mut mix = Map::new();
        for (b, n) in &self.basis_mix {
            mix.insert(b.clone(), Value::from(*n));
        }
        m.insert("basisMix".into(), Value::Object(mix));
        let mut cov = Map::new();
        cov.insert("matched".into(), group_json(&self.matched));
        cov.insert("unmatched".into(), group_json(&self.unmatched));
        m.insert("coverage".into(), Value::Object(cov));
        m.insert("considered".into(), Value::from(self.considered));
        Value::Object(m)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::price_model::{ModelConfig, PriceModel};
    use serde_json::json;

    fn dealer(n: usize, make: &str, model: &str, trim: &str, base: f64) -> Vec<Listing> {
        (0..n)
            .map(|i| {
                let km = 60_000.0 + i as f64 * 5_000.0;
                Listing::from_value(&json!({
                    "id": format!("d-{make}-{model}-{i}"), "make": make, "model": model, "year": 2019,
                    "km": km, "trimText": trim, "province": "QC",
                    "price": round(base - 0.1 * (km - 60_000.0)), "sellerType": "Dealer", "isDamaged": false
                }))
            })
            .collect()
    }

    fn private(n: usize, ratio: f64, make: &str, model: &str, trim: &str, base: f64) -> Vec<Listing> {
        (0..n)
            .map(|i| {
                let km = 60_000.0 + (i % 20) as f64 * 5_000.0;
                Listing::from_value(&json!({
                    "id": format!("p-{make}-{model}-{ratio}-{i}"), "make": make, "model": model, "year": 2019,
                    "km": km, "trimText": trim, "province": "QC",
                    "price": round((base - 0.1 * (km - 60_000.0)) * ratio), "sellerType": "PrivateSeller", "isDamaged": false
                }))
            })
            .collect()
    }

    fn dealer_model() -> PriceModel {
        PriceModel::build(&dealer(20, "Toyota", "RAV4", "XLE", 30_000.0), ModelConfig::default())
    }

    #[test]
    fn recovers_a_known_discount() {
        let d = measure_discount(&dealer_model(), &private(40, 0.85, "Toyota", "RAV4", "XLE", 30_000.0), DiscountConfig::default());
        let g = d.global.unwrap();
        assert!((g.ratio - 0.85).abs() < 0.02);
        assert!((g.discount_pct.unwrap() - 15.0).abs() < 2.0);
        assert_eq!(g.n, 40);
    }

    #[test]
    fn refuses_a_global_figure_from_too_few_samples() {
        let d = measure_discount(&dealer_model(), &private(5, 0.85, "Toyota", "RAV4", "XLE", 30_000.0), DiscountConfig::default());
        assert!(d.global.is_none());
        assert_eq!(d.samples.len(), 5);
    }

    #[test]
    fn per_model_ratio_needs_enough_samples() {
        let mut p = private(30, 0.8, "Toyota", "RAV4", "XLE", 30_000.0);
        p.extend(private(5, 0.9, "Honda", "Civic", "XLE", 30_000.0));
        let d = measure_discount(&dealer_model(), &p, DiscountConfig::default());
        assert!(d.by_model.contains_key("Toyota|RAV4"));
        assert!(!d.by_model.contains_key("Honda|Civic"));
        assert_eq!(d.ratio_for(Some("Honda"), Some("Civic")).unwrap().n, d.global.as_ref().unwrap().n);
    }

    #[test]
    fn rejects_placeholders_outliers_wrecks_and_unpriceable() {
        let mut p = private(30, 0.85, "Toyota", "RAV4", "XLE", 30_000.0);
        p.push(Listing::from_value(&json!({"id":"junk","make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":1,"province":"QC"})));
        p.push(Listing::from_value(&json!({"id":"wreck","make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":20000,"province":"QC","isDamaged":true})));
        p.push(Listing::from_value(&json!({"id":"cheap","make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":2500,"province":"QC","trimText":"XLE"})));
        p.push(Listing::from_value(&json!({"id":"orphan","make":"Suzuki","model":"Swift","year":2004,"km":200000,"price":3000,"province":"QC"})));
        let d = measure_discount(&dealer_model(), &p, DiscountConfig::default());
        assert_eq!(d.rejected, Rejected { unpriceable: 1, implausible_price: 1, outlier_ratio: 1, damaged: 1 });
        assert_eq!(d.samples.len(), 30);
        assert_eq!(d.matched.n, 30);
        assert_eq!(d.unmatched.n, 4);
        assert_eq!(d.considered, 34);
    }
}
