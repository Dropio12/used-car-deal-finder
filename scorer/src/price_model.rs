//! The Québec price baseline. Port of `src/price-model.js` (the model half;
//! scoring lives in `score.rs`).
//!
//! Two principles, same as the original:
//!  1. Refuse rather than guess: every estimate carries its comp count, and
//!     `estimate` returns `None` when there are too few.
//!  2. Medians, never means.

use std::collections::{HashMap, HashSet};

use serde_json::{Map, Value};

use crate::baseline::{Baseline, Estimate, Limits, Market, Query, TrimFactor};
use crate::js::{fr_ca, num_str, opt_str, round, truthy};
use crate::listing::Listing;
use crate::stats::{max, median, min, percentile};
use crate::trim::{assign_trim, induce_vocabulary, InduceOptions};

#[derive(Debug, Clone, PartialEq)]
pub struct ModelConfig {
    pub market: Market,
    /// Below this, a bucket is not a baseline.
    pub min_comps: f64,
    /// Below `min_comps` but still reported, marked with an asterisk. 0 disables.
    pub min_thin_comps: f64,
    /// Widest year window used when an exact year has too few comps.
    pub max_year_spread: f64,
    /// Plausible depreciation per km, used to sanity-check a fitted slope.
    pub km_slope_bounds: (f64, f64),
    /// How far a car's mileage may sit from the bucket median without a slope.
    pub max_km_gap_without_slope: f64,
    /// Interquartile range / median above which the median means nothing.
    pub max_spread_ratio: f64,
    pub min_plausible_price: f64,
    pub max_plausible_discount: f64,
    pub induce: InduceOptions,
}

impl Default for ModelConfig {
    fn default() -> Self {
        ModelConfig {
            market: Market::Dealer,
            min_comps: 8.0,
            min_thin_comps: 5.0,
            max_year_spread: 1.0,
            km_slope_bounds: (-0.60, -0.005),
            max_km_gap_without_slope: 40_000.0,
            max_spread_ratio: 0.75,
            min_plausible_price: 1_000.0,
            max_plausible_discount: 0.6,
            induce: InduceOptions::default(),
        }
    }
}

impl ModelConfig {
    /// Apply JS-style option overrides (`{ minComps: 3, market: 'private', ... }`).
    pub fn with_overrides(mut self, options: &Value) -> Self {
        let num = |k: &str| options.get(k).and_then(Value::as_f64);
        if let Some(m) = options.get("market").and_then(Value::as_str) {
            self.market = if m == "private" { Market::Private } else { Market::Dealer };
        }
        if let Some(v) = num("minComps") { self.min_comps = v; }
        if let Some(v) = num("minThinComps") { self.min_thin_comps = v; }
        if let Some(v) = num("maxYearSpread") { self.max_year_spread = v; }
        if let Some(v) = num("maxKmGapWithoutSlope") { self.max_km_gap_without_slope = v; }
        if let Some(v) = num("maxSpreadRatio") { self.max_spread_ratio = v; }
        if let Some(v) = num("minPlausiblePrice") { self.min_plausible_price = v; }
        if let Some(v) = num("maxPlausibleDiscount") { self.max_plausible_discount = v; }
        if let Some(v) = num("minCount") { self.induce.min_count = v; }
        if let Some(v) = num("minShare") { self.induce.min_share = v; }
        if let Some(b) = options.get("kmSlopeBounds").and_then(Value::as_array) {
            if let (Some(lo), Some(hi)) = (b.first().and_then(Value::as_f64), b.get(1).and_then(Value::as_f64)) {
                self.km_slope_bounds = (lo, hi);
            }
        }
        self
    }
}

/// One usable comparable, with the trim the model assigned it.
#[derive(Debug, Clone)]
struct Comp {
    price: f64,
    year: f64,
    km: f64,
    make: Option<String>,
    model: Option<String>,
    province: Option<String>,
    trim: Option<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct BucketStats {
    pub n: usize,
    pub median: f64,
    pub p25: f64,
    pub p75: f64,
    pub min_price: f64,
    pub max_price: f64,
    pub median_km: Option<f64>,
    pub min_km: Option<f64>,
    pub max_km: Option<f64>,
    pub km_slope: Option<f64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Coverage {
    pub listings: usize,
    pub usable: usize,
    pub with_trim: usize,
    pub buckets: usize,
}

pub struct PriceModel {
    pub config: ModelConfig,
    vocabularies: HashMap<String, Vec<String>>,
    buckets: HashMap<String, BucketStats>,
    comps: Vec<Comp>,
    /// Comps grouped by region|make|model, for the year-range fallbacks.
    by_model: HashMap<String, Vec<usize>>,
    trim_factors: HashMap<String, TrimFactor>,
    pub coverage: Coverage,
}

/// Least-squares slope of price against km, or `None` when it cannot be trusted.
pub fn fit_km_slope(points: &[(f64, f64)], config: &ModelConfig) -> Option<f64> {
    if (points.len() as f64) < config.min_comps {
        return None;
    }
    let kms: Vec<f64> = points.iter().map(|p| p.0).collect();
    if max(&kms) - min(&kms) < 30_000.0 {
        return None; // too tight to define a slope
    }
    let len = points.len() as f64;
    let mean_km = kms.iter().fold(0.0, |a, b| a + b) / len;
    let mean_price = points.iter().fold(0.0, |a, p| a + p.1) / len;

    let mut numerator = 0.0;
    let mut denominator = 0.0;
    for (km, price) in points {
        numerator += (km - mean_km) * (price - mean_price);
        denominator += (km - mean_km) * (km - mean_km);
    }
    if denominator == 0.0 {
        return None;
    }
    let slope = numerator / denominator;
    let (low, high) = config.km_slope_bounds;
    if !slope.is_finite() || slope < low || slope > high {
        return None;
    }
    Some(slope)
}

fn summarize(comps: &[&Comp], config: &ModelConfig) -> BucketStats {
    let prices: Vec<f64> = comps.iter().map(|c| c.price).collect();
    let kms: Vec<f64> = comps.iter().map(|c| c.km).collect();
    let points: Vec<(f64, f64)> = comps.iter().map(|c| (c.km, c.price)).collect();
    BucketStats {
        n: comps.len(),
        median: median(&prices).unwrap_or(f64::NAN),
        p25: percentile(&prices, 25.0).unwrap_or(f64::NAN),
        p75: percentile(&prices, 75.0).unwrap_or(f64::NAN),
        min_price: min(&prices),
        max_price: max(&prices),
        median_km: median(&kms),
        min_km: if kms.is_empty() { None } else { Some(min(&kms)) },
        max_km: if kms.is_empty() { None } else { Some(max(&kms)) },
        km_slope: fit_km_slope(&points, config),
    }
}

/// Region leads the key: prices are regional, and blending provinces would
/// produce a median that describes neither.
fn bucket_key(region: Option<&str>, make: Option<&str>, model: Option<&str>, year: f64, trim: Option<&str>) -> String {
    format!(
        "{}|{}|{}|{}|{}",
        region.unwrap_or("?"),
        opt_str(make),
        opt_str(model),
        num_str(year),
        trim.unwrap_or("?")
    )
}

fn model_key(make: Option<&str>, model: Option<&str>) -> String {
    format!("{}|{}", opt_str(make), opt_str(model))
}

impl PriceModel {
    /// Build the model from a set of listings (normally active dealer listings).
    pub fn build(listings: &[Listing], config: ModelConfig) -> PriceModel {
        let usable: Vec<&Listing> = listings
            .iter()
            .filter(|l| match (l.price, l.year, l.km) {
                (Some(p), Some(_), Some(_)) => {
                    p >= config.min_plausible_price && !truthy(&l.is_conditional_price) && !truthy(&l.is_damaged)
                }
                _ => false,
            })
            .collect();

        // Induce one trim vocabulary per make|model, in first-seen order.
        let mut group_order: Vec<String> = Vec::new();
        let mut groups: HashMap<String, Vec<&Listing>> = HashMap::new();
        for l in &usable {
            let key = model_key(l.make.as_deref(), l.model.as_deref());
            groups.entry(key.clone()).or_insert_with(|| {
                group_order.push(key.clone());
                Vec::new()
            });
            groups.get_mut(&key).unwrap().push(l);
        }
        let mut vocabularies = HashMap::new();
        for key in &group_order {
            // The JS splits the key back apart, so a null make becomes the string "null".
            let mut parts = key.split('|');
            let make = parts.next();
            let model = parts.next();
            let texts = groups[key].iter().map(|l| l.trim_text.as_deref());
            vocabularies.insert(key.clone(), induce_vocabulary(texts, make, model, config.induce));
        }

        let empty: Vec<String> = Vec::new();
        let comps: Vec<Comp> = usable
            .iter()
            .map(|l| {
                let vocab = vocabularies
                    .get(&model_key(l.make.as_deref(), l.model.as_deref()))
                    .unwrap_or(&empty);
                Comp {
                    price: l.price.unwrap(),
                    year: l.year.unwrap(),
                    km: l.km.unwrap(),
                    make: l.make.clone(),
                    model: l.model.clone(),
                    province: l.province.clone(),
                    trim: assign_trim(l.trim_text.as_deref(), vocab, l.make.as_deref(), l.model.as_deref()),
                }
            })
            .collect();

        let mut bucket_members: HashMap<String, Vec<usize>> = HashMap::new();
        let mut by_model: HashMap<String, Vec<usize>> = HashMap::new();
        for (i, c) in comps.iter().enumerate() {
            let (region, make, model) = (c.province.as_deref(), c.make.as_deref(), c.model.as_deref());
            bucket_members.entry(bucket_key(region, make, model, c.year, c.trim.as_deref())).or_default().push(i);
            // A trim-agnostic bucket, used when the trim-specific one is too thin.
            bucket_members.entry(bucket_key(region, make, model, c.year, None)).or_default().push(i);
            by_model
                .entry(format!("{}|{}|{}", opt_str(region), opt_str(make), opt_str(model)))
                .or_default()
                .push(i);
        }

        let buckets: HashMap<String, BucketStats> = bucket_members
            .into_iter()
            .map(|(k, idx)| {
                let members: Vec<&Comp> = idx.iter().map(|&i| &comps[i]).collect();
                (k, summarize(&members, &config))
            })
            .collect();

        let coverage = Coverage {
            listings: listings.len(),
            usable: usable.len(),
            with_trim: comps.iter().filter(|c| c.trim.is_some()).count(),
            buckets: buckets.values().filter(|b| b.n as f64 >= config.min_comps).count(),
        };

        let trim_factors = measure_trim_factors(&comps);
        PriceModel { config, vocabularies, buckets, comps, by_model, trim_factors, coverage }
    }

    pub fn bucket(&self, key: &str) -> Option<&BucketStats> {
        self.buckets.get(key)
    }

    pub fn trim_factor(&self, key: &str) -> Option<&TrimFactor> {
        self.trim_factors.get(key)
    }

    pub fn coverage_json(&self) -> Value {
        let mut m = Map::new();
        m.insert("listings".into(), Value::from(self.coverage.listings));
        m.insert("usable".into(), Value::from(self.coverage.usable));
        m.insert("withTrim".into(), Value::from(self.coverage.with_trim));
        m.insert("buckets".into(), Value::from(self.coverage.buckets));
        Value::Object(m)
    }

    fn finish(
        &self,
        bucket: &BucketStats,
        basis: &str,
        trim: Option<String>,
        km: Option<f64>,
        years_pooled: Option<usize>,
        trim_factor: Option<&TrimFactor>,
    ) -> Estimate {
        let config = &self.config;
        let mut baseline = bucket.median;
        let mut km_adjustment = 0.0;
        let mut warnings = Vec::new();

        // Trim factor first: the slope is about mileage, not equipment level.
        let mut low = Some(bucket.p25);
        let mut high = Some(bucket.p75);
        if let Some(tf) = trim_factor {
            baseline = round(baseline * tf.factor);
            low = low.map(|v| round(v * tf.factor));
            high = high.map(|v| round(v * tf.factor));
        }

        let spread = if bucket.median > 0.0 { (bucket.p75 - bucket.p25) / bucket.median } else { 0.0 };
        if spread > config.max_spread_ratio {
            warnings.push(format!(
                "comps range from {} $ to {} $ — too spread out for the median to mean anything{}",
                fr_ca(round(bucket.p25)),
                fr_ca(round(bucket.p75)),
                if basis == "trim" { "" } else { ", probably several trims in one bucket" }
            ));
        }

        match (bucket.km_slope, km, bucket.median_km) {
            (Some(slope), Some(km), Some(median_km)) => {
                // Clamp to the fitted range: a line extrapolated past its data goes negative.
                let clamped = km.max(bucket.min_km.unwrap_or(km)).min(bucket.max_km.unwrap_or(km));
                if clamped != km {
                    warnings.push(format!(
                        "{} km is outside the {}–{} km range of the comps, so the mileage adjustment was capped at the edge of the data",
                        fr_ca(km),
                        bucket.min_km.map_or("undefined".into(), fr_ca),
                        bucket.max_km.map_or("undefined".into(), fr_ca)
                    ));
                }
                km_adjustment = round(slope * (clamped - median_km));
                baseline = round(baseline + km_adjustment);
            }
            (None, Some(km), Some(median_km)) => {
                if (km - median_km).abs() > config.max_km_gap_without_slope {
                    warnings.push(format!(
                        "comps average {} km but this car has {} km, and the bucket has no usable mileage slope",
                        fr_ca(round(median_km)),
                        fr_ca(km)
                    ));
                }
            }
            _ => {}
        }

        // Nothing downstream may divide by a baseline that is not a price.
        // Written as partial_cmp so a NaN baseline is caught too, as `!(a > b)` does in JS.
        if baseline.partial_cmp(&config.min_plausible_price) != Some(std::cmp::Ordering::Greater) {
            warnings.push(format!("the adjusted baseline came out at {} $, which is not a price", num_str(round(baseline))));
        }

        let reliable = warnings.is_empty();
        Estimate {
            baseline,
            low,
            high,
            n: bucket.n,
            basis: if trim_factor.is_some() { format!("{basis}+trim") } else { basis.to_string() },
            trim_factor: trim_factor.map(|tf| TrimFactor { factor: round(tf.factor * 1000.0) / 1000.0, ..tf.clone() }),
            trim,
            km_adjustment,
            km_slope: bucket.km_slope,
            median_km: bucket.median_km,
            spread: round(spread * 100.0) / 100.0,
            warnings,
            reliable,
            years_pooled: years_pooled.filter(|y| *y > 0),
            dealer_baseline: None,
            seller_adjustment: None,
        }
    }
}

impl Baseline for PriceModel {
    fn market(&self) -> Market {
        self.config.market
    }

    fn limits(&self) -> Limits {
        Limits {
            min_plausible_price: self.config.min_plausible_price,
            max_plausible_discount: self.config.max_plausible_discount,
        }
    }

    /// Price one car, widening only as far as it must:
    /// trim -> trim-year-range -> model-year -> year-range -> thin (*) -> None.
    /// Region is never given up.
    fn estimate(&self, q: &Query) -> Option<Estimate> {
        let config = &self.config;
        let (make, model_name, year) = (q.make?, q.model?, q.year?);
        let region = q.province?;

        let resolved: Option<String> = match q.trim {
            Some(t) => Some(t.to_string()),
            None => match q.trim_text {
                Some(text) if !text.is_empty() => {
                    let empty = Vec::new();
                    let vocab = self.vocabularies.get(&format!("{make}|{model_name}")).unwrap_or(&empty);
                    assign_trim(Some(text), vocab, Some(make), Some(model_name))
                }
                _ => None,
            },
        };
        let trim_known = resolved.as_deref().is_some_and(|t| !t.is_empty());

        if trim_known {
            let key = bucket_key(Some(region), Some(make), Some(model_name), year, resolved.as_deref());
            if let Some(exact) = self.buckets.get(&key) {
                if exact.n as f64 >= config.min_comps {
                    return Some(self.finish(exact, "trim", resolved, q.km, None, None));
                }
            }
        }

        let same_model: Vec<&Comp> = self
            .by_model
            .get(&format!("{region}|{make}|{model_name}"))
            .map(|idx| idx.iter().map(|&i| &self.comps[i]).collect())
            .unwrap_or_default();

        // Widen the year before giving up the trim: a 2019 XL beats a 2020 Lariat.
        if trim_known {
            let across: Vec<&Comp> = same_model
                .iter()
                .copied()
                .filter(|c| c.trim == resolved && (c.year - year).abs() <= config.max_year_spread)
                .collect();
            if across.len() as f64 >= config.min_comps {
                let stats = summarize(&across, config);
                return Some(self.finish(&stats, "trim-year-range", resolved, q.km, None, None));
            }
        }

        let trim_factor = if trim_known {
            self.trim_factors.get(&format!("{region}|{make}|{model_name}|{}", resolved.as_deref().unwrap_or("")))
        } else {
            None
        };

        let model_year = self.buckets.get(&bucket_key(Some(region), Some(make), Some(model_name), year, None));
        if let Some(b) = model_year {
            if b.n as f64 >= config.min_comps {
                return Some(self.finish(b, "model-year", resolved, q.km, None, trim_factor));
            }
        }

        let pooled: Vec<&Comp> = same_model
            .iter()
            .copied()
            .filter(|c| (c.year - year).abs() <= config.max_year_spread)
            .collect();
        let years_pooled = || {
            let years: HashSet<u64> = pooled.iter().map(|c| c.year.to_bits()).collect();
            years.len()
        };
        if pooled.len() as f64 >= config.min_comps {
            let stats = summarize(&pooled, config);
            return Some(self.finish(&stats, "year-range", resolved, q.km, Some(years_pooled()), trim_factor));
        }

        // Below the floor, marked with an asterisk. Purely additive.
        let thin = config.min_thin_comps;
        if thin != 0.0 && !thin.is_nan() && thin < config.min_comps {
            if let Some(b) = model_year {
                if b.n as f64 >= thin {
                    return Some(self.finish(b, "model-year*", resolved, q.km, None, trim_factor));
                }
            }
            if pooled.len() as f64 >= thin {
                let stats = summarize(&pooled, config);
                return Some(self.finish(&stats, "year-range*", resolved, q.km, Some(years_pooled()), trim_factor));
            }
        }
        None
    }
}

/// How much each trim is worth relative to its model-year as a whole.
fn measure_trim_factors(comps: &[Comp]) -> HashMap<String, TrimFactor> {
    const MIN_SAMPLES: usize = 5;
    const MIN_YEARS: usize = 2;
    const MIN_GROUP: usize = 5;

    let group_key = |c: &Comp| {
        format!(
            "{}|{}|{}|{}",
            opt_str(c.province.as_deref()),
            opt_str(c.make.as_deref()),
            opt_str(c.model.as_deref()),
            num_str(c.year)
        )
    };

    let mut group_prices: HashMap<String, Vec<f64>> = HashMap::new();
    for c in comps {
        group_prices.entry(group_key(c)).or_default().push(c.price);
    }
    let group_median: HashMap<String, f64> = group_prices
        .into_iter()
        .filter(|(_, p)| p.len() >= MIN_GROUP)
        .filter_map(|(k, p)| median(&p).map(|m| (k, m)))
        .collect();

    // One ratio per car, not per dense bucket.
    let mut samples: HashMap<String, (Vec<f64>, HashSet<u64>)> = HashMap::new();
    for c in comps {
        let Some(trim) = c.trim.as_deref() else { continue };
        let Some(&gm) = group_median.get(&group_key(c)) else { continue };
        if gm.is_nan() || gm <= 0.0 {
            continue;
        }
        let key = format!(
            "{}|{}|{}|{}",
            opt_str(c.province.as_deref()),
            opt_str(c.make.as_deref()),
            opt_str(c.model.as_deref()),
            trim
        );
        let entry = samples.entry(key).or_default();
        entry.0.push(c.price / gm);
        entry.1.insert(c.year.to_bits());
    }

    let mut factors = HashMap::new();
    for (key, (ratios, years)) in samples {
        if ratios.len() < MIN_SAMPLES || years.len() < MIN_YEARS {
            continue;
        }
        let Some(value) = median(&ratios) else { continue };
        // Worth less than half or more than double its model-year: a mis-parse.
        #[allow(clippy::manual_range_contains)] // keeps the JS NaN behaviour exactly
        let implausible = value < 0.5 || value > 2.0;
        if implausible {
            continue;
        }
        factors.insert(key, TrimFactor { factor: value, years: years.len(), n: ratios.len() });
    }
    factors
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[allow(clippy::too_many_arguments)]
    pub fn comps(n: usize, make: &str, model: &str, year: f64, trim: &str, province: &str, base_price: f64, base_km: f64, km_step: f64, slope: f64) -> Vec<Listing> {
        (0..n)
            .map(|i| {
                let km = base_km + i as f64 * km_step;
                Listing::from_value(&json!({
                    "id": format!("{province}-{make}-{model}-{year}-{trim}-{i}"),
                    "make": make, "model": model, "year": year, "km": km, "province": province,
                    "price": round(base_price + slope * (km - base_km)),
                    "trimText": trim, "isDamaged": false,
                }))
            })
            .collect()
    }

    fn rav4(n: usize, trim: &str, base_price: f64) -> Vec<Listing> {
        comps(n, "Toyota", "RAV4", 2019.0, trim, "QC", base_price, 60_000.0, 5_000.0, -0.1)
    }

    fn q<'a>(year: f64, km: f64, trim: Option<&'a str>) -> Query<'a> {
        Query { make: Some("Toyota"), model: Some("RAV4"), year: Some(year), km: Some(km), trim, trim_text: None, province: Some("QC") }
    }

    #[test]
    fn fit_km_slope_recovers_a_known_slope() {
        let pts: Vec<(f64, f64)> = rav4(12, "XLE", 30_000.0).iter().map(|l| (l.km.unwrap(), l.price.unwrap())).collect();
        let s = fit_km_slope(&pts, &ModelConfig::default()).unwrap();
        assert!((s + 0.1).abs() < 0.001);
        assert_eq!(fit_km_slope(&pts[..4], &ModelConfig::default()), None);
    }

    #[test]
    fn fit_km_slope_refuses_positive_or_flat() {
        let up: Vec<(f64, f64)> = (0..12).map(|i| (60_000.0 + i as f64 * 5000.0, 30_000.0 + i as f64 * 500.0)).collect();
        assert_eq!(fit_km_slope(&up, &ModelConfig::default()), None);
        let flat: Vec<(f64, f64)> = (0..20).map(|i| (15.0 + i as f64, 86_000.0)).collect();
        assert_eq!(fit_km_slope(&flat, &ModelConfig::default()), None);
    }

    #[test]
    fn builds_trim_and_model_year_buckets_by_region() {
        let mut l = rav4(12, "XLE", 30_000.0);
        l.extend(rav4(12, "LE", 26_000.0));
        let m = PriceModel::build(&l, ModelConfig::default());
        assert!(m.bucket("QC|Toyota|RAV4|2019|XLE").is_some());
        assert!(m.bucket("QC|Toyota|RAV4|2019|LE").is_some());
        assert_eq!(m.bucket("QC|Toyota|RAV4|2019|?").unwrap().n, 24);
    }

    #[test]
    fn excludes_damaged_placeholder_and_conditional_rows() {
        let mut l = rav4(10, "XLE", 30_000.0);
        l.push(Listing::from_value(&json!({"make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":5000,"isDamaged":true,"province":"QC"})));
        l.push(Listing::from_value(&json!({"make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":1,"province":"QC"})));
        l.push(Listing::from_value(&json!({"make":"Toyota","model":"RAV4","year":2019,"km":90000,"price":9000,"isConditionalPrice":true,"province":"QC"})));
        l.push(Listing::from_value(&json!({"make":"Toyota","model":"RAV4","year":null,"km":90000,"price":25000,"province":"QC"})));
        assert_eq!(PriceModel::build(&l, ModelConfig::default()).coverage.usable, 10);
    }

    #[test]
    fn prices_against_the_matching_trim_and_adjusts_for_km() {
        let mut l = rav4(12, "XLE", 30_000.0);
        l.extend(rav4(12, "LE", 26_000.0));
        let m = PriceModel::build(&l, ModelConfig::default());
        let xle = m.estimate(&q(2019.0, 60_000.0, Some("XLE"))).unwrap();
        assert_eq!(xle.basis, "trim");
        assert_eq!(xle.n, 12);
        assert!((xle.baseline - 30_000.0).abs() < 1500.0);
        let le = m.estimate(&q(2019.0, 60_000.0, Some("LE"))).unwrap();
        assert!(xle.baseline > le.baseline);
        let low = m.estimate(&q(2019.0, 70_000.0, Some("XLE"))).unwrap();
        let high = m.estimate(&q(2019.0, 110_000.0, Some("XLE"))).unwrap();
        assert!(low.baseline > high.baseline);
    }

    #[test]
    fn resolves_trim_from_text_and_falls_back_to_model_year() {
        let mut l = rav4(12, "XLE", 30_000.0);
        l.extend(rav4(12, "LE", 26_000.0));
        let m = PriceModel::build(&l, ModelConfig::default());
        let mut query = q(2019.0, 60_000.0, None);
        query.trim_text = Some("XLE AWD - Remote Starter");
        let r = m.estimate(&query).unwrap();
        assert_eq!(r.trim.as_deref(), Some("XLE"));
        assert_eq!(r.basis, "trim");
        query.trim_text = Some("Bas kilométrage");
        let r = m.estimate(&query).unwrap();
        assert_eq!(r.basis, "model-year");
        assert_eq!(r.n, 24);
    }

    #[test]
    fn refuses_other_provinces_and_missing_fields() {
        let m = PriceModel::build(&rav4(12, "XLE", 30_000.0), ModelConfig::default());
        let mut query = q(2019.0, 60_000.0, Some("XLE"));
        query.province = Some("AB");
        assert!(m.estimate(&query).is_none());
        query.province = None;
        assert!(m.estimate(&query).is_none());
        assert!(m.estimate(&Query::default()).is_none());
    }

    #[test]
    fn thin_tier_is_marked_and_can_be_disabled() {
        let tiny = PriceModel::build(&rav4(5, "XLE", 30_000.0), ModelConfig::default());
        let r = tiny.estimate(&q(2019.0, 60_000.0, None)).unwrap();
        assert!(r.basis.ends_with('*'));
        assert_eq!(r.n, 5);
        let tinier = PriceModel::build(&rav4(4, "XLE", 30_000.0), ModelConfig::default());
        assert!(tinier.estimate(&q(2019.0, 60_000.0, None)).is_none());
        let off = PriceModel::build(&rav4(5, "XLE", 30_000.0), ModelConfig { min_thin_comps: 0.0, ..ModelConfig::default() });
        assert!(off.estimate(&q(2019.0, 60_000.0, None)).is_none());
    }

    #[test]
    fn clamps_mileage_extrapolation() {
        let m = PriceModel::build(&rav4(12, "XLE", 30_000.0), ModelConfig::default());
        let edge = m.estimate(&q(2019.0, 115_000.0, Some("XLE"))).unwrap();
        let past = m.estimate(&q(2019.0, 400_000.0, Some("XLE"))).unwrap();
        assert_eq!(edge.baseline, past.baseline);
        assert!(past.warnings[0].contains("outside the"));
        assert!(!past.reliable);
    }

    #[test]
    fn near_new_bucket_flags_far_off_mileage() {
        let l = comps(20, "RAM", "1500", 2026.0, "Sport", "QC", 86_000.0, 15.0, 2.0, -0.1);
        let m = PriceModel::build(&l, ModelConfig::default());
        let mut query = Query { make: Some("RAM"), model: Some("1500"), year: Some(2026.0), km: Some(30.0), trim: Some("SPORT"), trim_text: None, province: Some("QC") };
        assert!(m.estimate(&query).unwrap().reliable);
        query.km = Some(60_000.0);
        let r = m.estimate(&query).unwrap();
        assert!(!r.reliable);
        assert!(r.warnings[0].contains("no usable mileage slope"));
    }

    #[test]
    fn learns_trim_factors_across_years() {
        let f = |year: f64, trim: &str, price: f64, n: usize| comps(n, "Ford", "F-150", year, trim, "QC", price, 60_000.0, 5_000.0, -0.05);
        let mut l = f(2020.0, "XL", 30_000.0, 10);
        l.extend(f(2020.0, "Lariat", 60_000.0, 10));
        l.extend(f(2019.0, "XL", 28_000.0, 10));
        l.extend(f(2019.0, "Lariat", 56_000.0, 10));
        l.extend(f(2016.0, "Mixed", 44_000.0, 20));
        let m = PriceModel::build(&l, ModelConfig::default());
        assert!(m.trim_factor("QC|Ford|F-150|XL").unwrap().factor < 0.85);
        assert!(m.trim_factor("QC|Ford|F-150|LARIAT").unwrap().factor > 1.15);
        let r = m
            .estimate(&Query { make: Some("Ford"), model: Some("F-150"), year: Some(2016.0), km: Some(60_000.0), trim: Some("XL"), trim_text: None, province: Some("QC") })
            .unwrap();
        assert!(r.basis.ends_with("+trim"));
        assert!(r.baseline < 40_000.0);
    }

    #[test]
    fn flags_a_bucket_too_spread_out() {
        let mut l = comps(5, "Ford", "Mustang", 2020.0, "Base", "QC", 26_000.0, 60_000.0, 5_000.0, -0.05);
        l.extend(comps(5, "Ford", "Mustang", 2020.0, "Shelby", "QC", 120_000.0, 60_000.0, 5_000.0, -0.05));
        let m = PriceModel::build(&l, ModelConfig::default());
        let r = m
            .estimate(&Query { make: Some("Ford"), model: Some("Mustang"), year: Some(2020.0), km: Some(60_000.0), trim: None, trim_text: None, province: Some("QC") })
            .unwrap();
        assert_eq!(r.basis, "model-year");
        assert!(!r.reliable);
        assert!(r.warnings[0].contains("too spread out"));
    }

    #[test]
    fn overrides_apply_like_object_spread() {
        let c = ModelConfig::default().with_overrides(&json!({"market": "private", "minComps": 3}));
        assert_eq!(c.market, Market::Private);
        assert_eq!(c.min_comps, 3.0);
        assert_eq!(c.min_thin_comps, 5.0);
    }
}
