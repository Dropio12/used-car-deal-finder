//! GET /api/deals: score every active stored listing with the Rust scorer
//! (`scorer::score::run`, the same code as the native `carbuyer-scorer`) and
//! return the best-priced ones.
//!
//! Like the Go pipeline, the baseline is built from all active listings; the
//! make/model/seller filters only choose which scored cars are returned.

use serde_json::{json, Map, Value};

use crate::sql::json_num;

pub const DEFAULT_LIMIT: usize = 20;
pub const MAX_LIMIT: usize = 200;

/// Columns read for scoring and display. Descriptions and image lists are
/// left out: the scorer does not need them and they are the bulk of a row.
pub const LOAD_SQL: &str = "SELECT id, source, url, make, model, year, km, price, first_price, trim_text, \
     seller_type, seller_name, city, province, is_damaged, is_parts, is_conditional_price, \
     price_changes, first_seen, last_seen \
     FROM listings WHERE removed_at IS NULL ORDER BY rowid";

#[derive(Debug, Clone, PartialEq)]
pub struct DealsQuery {
    pub make: Option<String>,
    pub model: Option<String>,
    /// "Dealer" or "PrivateSeller".
    pub seller_type: Option<String>,
    pub limit: usize,
    /// Scorer `model.minComps` override (the CLI's `--min-comps`).
    pub min_comps: Option<f64>,
}

impl Default for DealsQuery {
    fn default() -> Self {
        DealsQuery { make: None, model: None, seller_type: None, limit: DEFAULT_LIMIT, min_comps: None }
    }
}

impl DealsQuery {
    /// From URL query pairs: `make`, `model`, `seller` (dealer|private), `limit`, `minComps`.
    pub fn from_pairs<'a>(pairs: impl IntoIterator<Item = (&'a str, &'a str)>) -> Result<DealsQuery, String> {
        let mut q = DealsQuery::default();
        for (k, v) in pairs {
            let v = v.trim();
            if v.is_empty() {
                continue;
            }
            match k {
                "make" => q.make = Some(v.to_string()),
                "model" => q.model = Some(v.to_string()),
                "seller" => {
                    q.seller_type = Some(match v.to_ascii_lowercase().as_str() {
                        "dealer" => "Dealer".to_string(),
                        "private" => "PrivateSeller".to_string(),
                        _ => return Err(format!("seller must be dealer or private, not {v:?}")),
                    })
                }
                "limit" => {
                    let n: usize = v.parse().map_err(|_| format!("limit: {v:?} is not a whole number"))?;
                    q.limit = n.clamp(1, MAX_LIMIT);
                }
                "minComps" | "min_comps" => {
                    let n: f64 = v.parse().map_err(|_| format!("minComps: {v:?} is not a number"))?;
                    if !n.is_finite() || n < 1.0 {
                        return Err("minComps must be a number >= 1".into());
                    }
                    q.min_comps = Some(n);
                }
                _ => {}
            }
        }
        Ok(q)
    }

    fn matches(&self, l: &Value) -> bool {
        let eq = |want: &Option<String>, key: &str| match want {
            None => true,
            Some(w) => l.get(key).and_then(Value::as_str).is_some_and(|v| same(v, w)),
        };
        eq(&self.make, "make") && eq(&self.model, "model") && eq(&self.seller_type, "sellerType")
    }
}

/// Case- and punctuation-insensitive match, so `rav4` finds "RAV4" and
/// `mercedes-benz` finds "Mercedes-Benz".
fn same(a: &str, b: &str) -> bool {
    let norm = |s: &str| s.chars().filter(|c| c.is_alphanumeric()).flat_map(char::to_lowercase).collect::<String>();
    norm(a) == norm(b)
}

/// A D1 row (snake_case columns) back into the crawler's camelCase listing
/// shape, like the Go `LoadListings`. 0/1 flags become real booleans: the
/// scorer tests `isDamaged === true` strictly.
pub fn listing_from_row(row: &Value) -> Value {
    let mut m = Map::new();
    let text = |k: &str| row.get(k).filter(|v| v.is_string()).cloned().unwrap_or(Value::Null);
    let num = |k: &str| row.get(k).and_then(Value::as_f64).map_or(Value::Null, json_num);
    let flag = |k: &str| Value::Bool(row.get(k).and_then(Value::as_f64) == Some(1.0));
    for (out, col) in [("id", "id"), ("source", "source"), ("url", "url"), ("make", "make"), ("model", "model")] {
        m.insert(out.into(), text(col));
    }
    m.insert("year".into(), num("year"));
    m.insert("km".into(), num("km"));
    m.insert("price".into(), num("price"));
    m.insert("trimText".into(), text("trim_text"));
    m.insert("sellerType".into(), text("seller_type"));
    m.insert("sellerName".into(), text("seller_name"));
    m.insert("city".into(), text("city"));
    m.insert("province".into(), text("province"));
    m.insert("isDamaged".into(), flag("is_damaged"));
    m.insert("isParts".into(), flag("is_parts"));
    m.insert("isConditionalPrice".into(), flag("is_conditional_price"));
    m.insert("firstPrice".into(), num("first_price"));
    m.insert("priceChanges".into(), num("price_changes"));
    m.insert("firstSeen".into(), text("first_seen"));
    m.insert("lastSeen".into(), text("last_seen"));
    Value::Object(m)
}

/// Scores `listings` and returns `{comps, scored, matched, appraiser, deals}`,
/// deals best discount first (stable, like the Go pipeline).
pub fn best_deals(listings: &[Value], q: &DealsQuery) -> Result<Value, String> {
    let mut input = json!({ "listings": listings });
    if let Some(n) = q.min_comps {
        input["options"] = json!({ "model": { "minComps": n } });
    }
    let out = scorer::score::run(&input)?;
    let scores = out.get("scores").and_then(Value::as_array).cloned().unwrap_or_default();

    let mut scored = 0usize;
    let mut deals: Vec<(f64, Value)> = Vec::new();
    for (l, s) in listings.iter().zip(scores.iter()) {
        let Some(score) = s.get("score").filter(|v| !v.is_null()) else { continue };
        scored += 1;
        if !q.matches(l) {
            continue;
        }
        let pct = score.get("discountPct").and_then(Value::as_f64).unwrap_or(f64::NEG_INFINITY);
        let mut d = l.clone();
        d["score"] = score.clone();
        deals.push((pct, d));
    }
    let matched = deals.len();
    deals.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));
    deals.truncate(q.limit);
    Ok(json!({
        "comps": listings.len(),
        "scored": scored,
        "matched": matched,
        "appraiser": out.get("appraiser").cloned().unwrap_or(Value::Null),
        "deals": deals.into_iter().map(|(_, d)| d).collect::<Vec<_>>(),
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn market() -> Vec<Value> {
        // Twelve dealer RAV4s around 25k and two cheap ones, plus a Civic.
        let mut v: Vec<Value> = (0..12)
            .map(|i| {
                json!({"id": format!("d{i}"), "source": "autohebdo", "make": "Toyota", "model": "RAV4",
                       "year": 2019, "km": 60000 + i * 1000, "price": 25000 + (i % 3) * 500,
                       "sellerType": "Dealer", "province": "QC", "isDamaged": false})
            })
            .collect();
        v.push(json!({"id": "cheap1", "source": "autohebdo", "make": "Toyota", "model": "RAV4", "year": 2019,
                      "km": 61000, "price": 21000, "sellerType": "Dealer", "province": "QC", "isDamaged": false}));
        v.push(json!({"id": "cheap2", "source": "autohebdo", "make": "Toyota", "model": "RAV4", "year": 2019,
                      "km": 62000, "price": 22500, "sellerType": "Dealer", "province": "QC", "isDamaged": false}));
        v.push(json!({"id": "civic", "source": "autohebdo", "make": "Honda", "model": "Civic", "year": 2018,
                      "km": 50000, "price": 15000, "sellerType": "Dealer", "province": "QC", "isDamaged": false}));
        v
    }

    #[test]
    fn query_params() {
        let q = DealsQuery::from_pairs([("make", "toyota"), ("limit", "500"), ("minComps", "3"), ("seller", "Private"), ("x", "y")])
            .unwrap();
        assert_eq!(q.make.as_deref(), Some("toyota"));
        assert_eq!(q.limit, MAX_LIMIT);
        assert_eq!(q.min_comps, Some(3.0));
        assert_eq!(q.seller_type.as_deref(), Some("PrivateSeller"));
        assert!(DealsQuery::from_pairs([("limit", "ten")]).is_err());
        assert!(DealsQuery::from_pairs([("minComps", "0")]).is_err());
        assert!(DealsQuery::from_pairs([("seller", "robot")]).is_err());
        assert_eq!(DealsQuery::from_pairs([("make", " ")]).unwrap(), DealsQuery::default());
    }

    #[test]
    fn same_ignores_case_and_punctuation() {
        assert!(same("RAV4", "rav4"));
        assert!(same("Mercedes-Benz", "mercedes benz"));
        assert!(!same("Civic", "Accord"));
    }

    #[test]
    fn scores_ranks_filters_and_limits() {
        let listings = market();
        let all = best_deals(&listings, &DealsQuery::default()).unwrap();
        assert_eq!(all["comps"], 15);
        let deals = all["deals"].as_array().unwrap();
        assert!(!deals.is_empty());
        assert_eq!(deals[0]["id"], "cheap1", "biggest discount first: {deals:?}");
        let pcts: Vec<f64> = deals.iter().map(|d| d["score"]["discountPct"].as_f64().unwrap()).collect();
        assert!(pcts.windows(2).all(|w| w[0] >= w[1]));

        // Same numbers as calling the scorer directly.
        let direct = scorer::score::run(&json!({ "listings": listings })).unwrap();
        let s = direct["scores"].as_array().unwrap().iter().find(|s| s["id"] == "cheap1").unwrap();
        assert_eq!(s["score"], deals[0]["score"]);

        let q = DealsQuery { make: Some("honda".into()), ..Default::default() };
        assert_eq!(best_deals(&listings, &q).unwrap()["deals"].as_array().unwrap().len(), 0, "one civic has no comps");
        let q = DealsQuery { limit: 1, ..Default::default() };
        let one = best_deals(&listings, &q).unwrap();
        assert_eq!(one["deals"].as_array().unwrap().len(), 1);
        assert_eq!(one["matched"], all["matched"]);
    }

    #[test]
    fn rows_become_listings() {
        let row = json!({"id": "a", "source": "autohebdo", "year": 2019.0, "price": 24000.0, "km": null,
                         "is_damaged": 1.0, "is_parts": 0.0, "trim_text": "XLE", "seller_type": "Dealer"});
        let l = listing_from_row(&row);
        assert_eq!(l["year"].to_string(), "2019");
        assert_eq!(l["isDamaged"], true);
        assert_eq!(l["isParts"], false);
        assert_eq!(l["isConditionalPrice"], false);
        assert_eq!(l["trimText"], "XLE");
        assert_eq!(l["km"], Value::Null);
    }
}
