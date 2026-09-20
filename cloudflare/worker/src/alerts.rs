//! Snipe alerts. When an ingest brings a listing that is new to the store,
//! back after a removal, or cheaper than before, it is scored against every
//! active listing; if it passes the [`AlertRule`] it is stored once in
//! `new_deal_alerts` (keyed by listing id + price) and handed to a
//! [`Notifier`].
//!
//! Plain Rust (no D1): it plans statements and shapes JSON, so `cargo test`
//! covers it natively. `d1.rs` runs the statements; `service.rs` wires it.

use std::collections::HashSet;

use serde_json::{json, Map, Value};

use crate::sql::{json_num, placeholders, Param, Stmt};

/// Defaults of the Worker vars below.
pub const DEFAULT_MIN_DISCOUNT_PCT: f64 = 15.0;
pub const DEFAULT_MIN_COMPS: f64 = 6.0;
pub const DEFAULT_LIST_LIMIT: usize = 20;
pub const MAX_LIST_LIMIT: usize = 200;

/// What counts as a deal worth an alert. Built from the Worker vars
/// `ALERTS_ENABLED`, `ALERT_MIN_DISCOUNT_PCT`, `ALERT_MIN_COMPS`,
/// `ALERT_INCLUDE_DAMAGED`.
#[derive(Debug, Clone, PartialEq)]
pub struct AlertRule {
    pub enabled: bool,
    /// At least this many % under the local baseline.
    pub min_discount_pct: f64,
    /// At least this many comparable cars behind the baseline. Also passed to
    /// the scorer as `minComps`, so buckets this small can price at all.
    pub min_comps: f64,
    /// Damaged cars are priced against undamaged comps, so their "discount"
    /// is mostly the damage. Off by default.
    pub include_damaged: bool,
}

impl Default for AlertRule {
    fn default() -> Self {
        AlertRule {
            enabled: true,
            min_discount_pct: DEFAULT_MIN_DISCOUNT_PCT,
            min_comps: DEFAULT_MIN_COMPS,
            include_damaged: false,
        }
    }
}

impl AlertRule {
    /// Reads the vars through `get`; blank or missing values keep the defaults.
    pub fn from_vars(get: impl Fn(&str) -> Option<String>) -> Result<AlertRule, String> {
        let mut r = AlertRule::default();
        let val = |k: &str| {
            get(k)
                .map(|v| v.trim().to_string())
                .filter(|v| !v.is_empty())
        };
        let flag = |k: &str, v: String| match v.to_ascii_lowercase().as_str() {
            "true" | "1" | "yes" => Ok(true),
            "false" | "0" | "no" => Ok(false),
            _ => Err(format!("{k}: {v:?} is not true or false")),
        };
        if let Some(v) = val("ALERTS_ENABLED") {
            r.enabled = flag("ALERTS_ENABLED", v)?;
        }
        if let Some(v) = val("ALERT_INCLUDE_DAMAGED") {
            r.include_damaged = flag("ALERT_INCLUDE_DAMAGED", v)?;
        }
        if let Some(v) = val("ALERT_MIN_DISCOUNT_PCT") {
            r.min_discount_pct = v
                .parse::<f64>()
                .ok()
                .filter(|n| n.is_finite() && *n > 0.0 && *n < 100.0)
                .ok_or(format!(
                    "ALERT_MIN_DISCOUNT_PCT: {v:?} is not a number between 0 and 100"
                ))?;
        }
        if let Some(v) = val("ALERT_MIN_COMPS") {
            r.min_comps = v
                .parse::<f64>()
                .ok()
                .filter(|n| n.is_finite() && *n >= 1.0)
                .ok_or(format!("ALERT_MIN_COMPS: {v:?} is not a number >= 1"))?;
        }
        Ok(r)
    }

    /// True when `score` (the scorer's JSON for `listing`) is a strong deal.
    pub fn accepts(&self, listing: &Value, score: &Value) -> bool {
        let num = |v: &Value, k: &str| v.get(k).and_then(Value::as_f64);
        let yes = |v: &Value, k: &str| v.get(k) == Some(&Value::Bool(true));
        self.enabled
            && num(score, "discountPct").is_some_and(|d| d >= self.min_discount_pct)
            && num(score, "n").is_some_and(|n| n >= self.min_comps)
            && !yes(score, "thin")
            && (self.include_damaged || !yes(score, "damaged"))
            && !yes(listing, "isParts")
            && !yes(listing, "isConditionalPrice")
            && num(listing, "price").is_some_and(|p| p > 0.0)
    }

    /// The rule as the API reports it.
    pub fn to_json(&self) -> Value {
        json!({
            "enabled": self.enabled,
            "minDiscountPct": json_num(self.min_discount_pct),
            "minComps": json_num(self.min_comps),
            "includeDamaged": self.include_damaged,
        })
    }
}

/// One strong deal to alert about.
#[derive(Debug, Clone, PartialEq)]
pub struct Alert {
    pub listing_id: String,
    /// Asking price, whole dollars (part of the key).
    pub price: i64,
    pub discount_pct: f64,
    /// The listing plus its `score`, the same shape as an `/api/deals` item.
    pub deal: Value,
}

impl Alert {
    pub fn key(&self) -> (String, i64) {
        (self.listing_id.clone(), self.price)
    }
}

/// Scores `active` (every active stored listing, the baseline) and returns an
/// alert for each `fresh` id that passes `rule`, best discount first.
pub fn find(active: &[Value], fresh: &[String], rule: &AlertRule) -> Result<Vec<Alert>, String> {
    if !rule.enabled || fresh.is_empty() {
        return Ok(Vec::new());
    }
    let wanted: HashSet<&str> = fresh.iter().map(String::as_str).collect();
    let input =
        json!({ "listings": active, "options": { "model": { "minComps": rule.min_comps } } });
    let out = scorer::score::run(&input)?;
    let scores = out
        .get("scores")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();

    let mut alerts = Vec::new();
    let mut seen = HashSet::new();
    for (l, s) in active.iter().zip(scores.iter()) {
        let Some(id) = l.get("id").and_then(Value::as_str) else {
            continue;
        };
        if !wanted.contains(id) || !seen.insert(id) {
            continue;
        }
        let Some(score) = s.get("score").filter(|v| v.is_object()) else {
            continue;
        };
        if !rule.accepts(l, score) {
            continue;
        }
        let mut deal = l.clone();
        deal["score"] = score.clone();
        alerts.push(Alert {
            listing_id: id.to_string(),
            price: l
                .get("price")
                .and_then(Value::as_f64)
                .unwrap_or(0.0)
                .round() as i64,
            discount_pct: score
                .get("discountPct")
                .and_then(Value::as_f64)
                .unwrap_or(0.0),
            deal,
        });
    }
    alerts.sort_by(|a, b| {
        b.discount_pct
            .partial_cmp(&a.discount_pct)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    Ok(alerts)
}

/// Which (listing, price) keys are already alerted, chunked under D1's
/// 100-parameter limit.
pub fn existing_queries(ids: &[String]) -> Vec<Stmt> {
    ids.chunks(90)
        .map(|chunk| {
            Stmt::new(
                format!(
                    "SELECT listing_id, price FROM new_deal_alerts WHERE listing_id IN ({})",
                    placeholders(1, chunk.len())
                ),
                chunk.iter().map(|id| Param::Text(id.clone())).collect(),
            )
        })
        .collect()
}

pub fn key_from_row(row: &Value) -> Option<(String, i64)> {
    Some((
        row.get("listing_id")?.as_str()?.to_string(),
        row.get("price")?.as_f64()?.round() as i64,
    ))
}

/// `INSERT OR IGNORE`: even two overlapping ingests cannot store a key twice.
pub fn insert_statements(alerts: &[Alert], now: &str) -> Vec<Stmt> {
    alerts
        .iter()
        .map(|a| {
            let d = &a.deal;
            let s = |k: &str| Param::text(d.get(k).and_then(Value::as_str));
            let score = d.get("score");
            let sn = |k: &str| Param::num(score.and_then(|s| s.get(k)).and_then(Value::as_f64));
            Stmt::new(
                "INSERT OR IGNORE INTO new_deal_alerts \
                 (listing_id, price, created_at, discount_pct, baseline, comps, make, model, year, url, deal) \
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)",
                vec![
                    Param::Text(a.listing_id.clone()),
                    Param::Int(a.price),
                    Param::Text(now.to_string()),
                    // Not Param::num: a percentage must stay REAL even when whole.
                    Param::Real(a.discount_pct),
                    sn("baseline"),
                    sn("n"),
                    s("make"),
                    s("model"),
                    Param::num(d.get("year").and_then(Value::as_f64)),
                    s("url"),
                    Param::Text(d.to_string()),
                ],
            )
        })
        .collect()
}

/// Marks alerts a notifier delivered.
pub fn notified_statements(alerts: &[Alert], now: &str) -> Vec<Stmt> {
    alerts
        .iter()
        .map(|a| {
            Stmt::new(
                "UPDATE new_deal_alerts SET notified_at = ?1 WHERE listing_id = ?2 AND price = ?3",
                vec![
                    Param::Text(now.to_string()),
                    Param::Text(a.listing_id.clone()),
                    Param::Int(a.price),
                ],
            )
        })
        .collect()
}

/// Newest first. `?1` = limit.
pub const LIST_SQL: &str = "SELECT created_at, notified_at, deal FROM new_deal_alerts \
     ORDER BY created_at DESC, rowid DESC LIMIT ?1";

/// A stored row as the API returns it: the deal, plus when it was alerted.
pub fn from_row(row: &Value) -> Value {
    let mut deal = row
        .get("deal")
        .and_then(Value::as_str)
        .and_then(|s| serde_json::from_str::<Value>(s).ok())
        .filter(Value::is_object)
        .unwrap_or_else(|| Value::Object(Map::new()));
    deal["alertedAt"] = row.get("created_at").cloned().unwrap_or(Value::Null);
    deal["notifiedAt"] = row.get("notified_at").cloned().unwrap_or(Value::Null);
    deal
}

/// `GET /api/alerts?limit=`.
#[derive(Debug, Clone, PartialEq)]
pub struct AlertsQuery {
    pub limit: usize,
}

impl Default for AlertsQuery {
    fn default() -> Self {
        AlertsQuery {
            limit: DEFAULT_LIST_LIMIT,
        }
    }
}

impl AlertsQuery {
    pub fn from_pairs<'a>(
        pairs: impl IntoIterator<Item = (&'a str, &'a str)>,
    ) -> Result<AlertsQuery, String> {
        let mut q = AlertsQuery::default();
        for (k, v) in pairs {
            let v = v.trim();
            if k == "limit" && !v.is_empty() {
                let n: usize = v
                    .parse()
                    .map_err(|_| format!("limit: {v:?} is not a whole number"))?;
                q.limit = n.clamp(1, MAX_LIST_LIMIT);
            }
        }
        Ok(q)
    }
}

/// Where alerts go besides the table. Add email or push by implementing this
/// and choosing it in `entry.rs` (the composition root); nothing else changes.
/// Returns how many alerts were delivered; those get `notified_at` set.
#[allow(async_fn_in_trait)] // single-threaded wasm: no Send bound needed
pub trait Notifier {
    async fn notify(&self, alerts: &[Alert]) -> Result<usize, String>;
}

/// The notifier in use today: alerts are only stored and shown on the
/// dashboard.
pub struct NoNotifier;

impl Notifier for NoNotifier {
    async fn notify(&self, _alerts: &[Alert]) -> Result<usize, String> {
        Ok(0)
    }
}

/// Plain-text summary a mail or push notifier can send.
pub fn summary(alerts: &[Alert]) -> String {
    let money = |v: Option<f64>| v.map_or("?".to_string(), |n| format!("{} $", n.round() as i64));
    alerts
        .iter()
        .map(|a| {
            let d = &a.deal;
            let txt = |k: &str| d.get(k).and_then(Value::as_str).unwrap_or("");
            let year = d
                .get("year")
                .and_then(Value::as_f64)
                .map_or(String::new(), |y| format!("{y} "));
            format!(
                "-{:.1}% {}{} {} {}: {} (baseline {}) {}",
                a.discount_pct,
                year,
                txt("make"),
                txt("model"),
                txt("trimText"),
                money(d.get("price").and_then(Value::as_f64)),
                money(
                    d.get("score")
                        .and_then(|s| s.get("baseline"))
                        .and_then(Value::as_f64)
                ),
                txt("url"),
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Ten dealer RAV4s around 25k and one at 20k (20% under).
    fn market() -> Vec<Value> {
        let mut v: Vec<Value> = (0..10)
            .map(|i| {
                json!({"id": format!("d{i}"), "source": "autohebdo", "make": "Toyota", "model": "RAV4",
                       "year": 2019, "km": 60000 + i * 1000, "price": 24800 + (i % 3) * 200,
                       "sellerType": "Dealer", "province": "QC", "isDamaged": false})
            })
            .collect();
        v.push(json!({"id": "cheap", "source": "autohebdo", "make": "Toyota", "model": "RAV4", "year": 2019,
                      "km": 61000, "price": 20000, "sellerType": "Dealer", "province": "QC", "isDamaged": false,
                      "url": "https://www.example.com/annonces/cheap", "trimText": "LE"}));
        v
    }

    #[test]
    fn rule_from_vars() {
        let none = AlertRule::from_vars(|_| None).unwrap();
        assert_eq!(none, AlertRule::default());
        let vars = |k: &str| {
            Some(
                match k {
                    "ALERTS_ENABLED" => "false",
                    "ALERT_MIN_DISCOUNT_PCT" => "12.5",
                    "ALERT_MIN_COMPS" => " 4 ",
                    "ALERT_INCLUDE_DAMAGED" => "yes",
                    _ => "",
                }
                .to_string(),
            )
        };
        let r = AlertRule::from_vars(vars).unwrap();
        assert_eq!(
            r,
            AlertRule {
                enabled: false,
                min_discount_pct: 12.5,
                min_comps: 4.0,
                include_damaged: true
            }
        );
        assert!(AlertRule::from_vars(
            |k| (k == "ALERT_MIN_DISCOUNT_PCT").then(|| "lots".to_string())
        )
        .is_err());
        assert!(
            AlertRule::from_vars(|k| (k == "ALERT_MIN_COMPS").then(|| "0".to_string())).is_err()
        );
        assert!(
            AlertRule::from_vars(|k| (k == "ALERTS_ENABLED").then(|| "maybe".to_string())).is_err()
        );
        assert_eq!(r.to_json()["minDiscountPct"], 12.5);
    }

    #[test]
    fn rule_accepts_only_strong_trustworthy_deals() {
        let r = AlertRule::default();
        let l = json!({"price": 20000});
        let s =
            |pct: f64, n: u32| json!({"discountPct": pct, "n": n, "thin": false, "damaged": false});
        assert!(r.accepts(&l, &s(15.0, 6)));
        assert!(!r.accepts(&l, &s(14.9, 6)), "under the threshold");
        assert!(!r.accepts(&l, &s(30.0, 5)), "too few comps");
        let mut thin = s(30.0, 10);
        thin["thin"] = json!(true);
        assert!(!r.accepts(&l, &thin));
        let mut damaged = s(30.0, 10);
        damaged["damaged"] = json!(true);
        assert!(!r.accepts(&l, &damaged));
        assert!(AlertRule {
            include_damaged: true,
            ..r.clone()
        }
        .accepts(&l, &damaged));
        assert!(!r.accepts(&json!({"price": 20000, "isParts": true}), &s(30.0, 10)));
        assert!(!r.accepts(
            &json!({"price": 20000, "isConditionalPrice": true}),
            &s(30.0, 10)
        ));
        assert!(!r.accepts(&json!({}), &s(30.0, 10)), "no price");
        assert!(!AlertRule {
            enabled: false,
            ..r
        }
        .accepts(&l, &s(30.0, 10)));
    }

    #[test]
    fn find_alerts_only_fresh_strong_deals() {
        let m = market();
        let rule = AlertRule::default();
        let got = find(&m, &["cheap".into(), "d1".into(), "missing".into()], &rule).unwrap();
        assert_eq!(got.len(), 1, "{got:?}");
        let a = &got[0];
        assert_eq!(a.key(), ("cheap".to_string(), 20000));
        assert!(a.discount_pct >= 15.0);
        assert_eq!(
            a.deal["score"]["discountPct"].as_f64(),
            Some(a.discount_pct)
        );
        assert_eq!(a.deal["url"], "https://www.example.com/annonces/cheap");

        // Same numbers as the scorer gives /api/deals with the same minComps.
        let direct =
            scorer::score::run(&json!({"listings": m, "options": {"model": {"minComps": 6.0}}}))
                .unwrap();
        let s = direct["scores"]
            .as_array()
            .unwrap()
            .iter()
            .find(|s| s["id"] == "cheap")
            .unwrap();
        assert_eq!(s["score"], a.deal["score"]);

        assert!(
            find(&m, &["d1".into()], &rule).unwrap().is_empty(),
            "a normal price is no alert"
        );
        assert!(find(&m, &[], &rule).unwrap().is_empty());
        let strict = AlertRule {
            min_discount_pct: 50.0,
            ..rule
        };
        assert!(find(&m, &["cheap".into()], &strict).unwrap().is_empty());
    }

    #[test]
    fn statements_and_rows() {
        let alerts = find(&market(), &["cheap".into()], &AlertRule::default()).unwrap();
        let ins = insert_statements(&alerts, "T");
        assert_eq!(ins.len(), 1);
        assert!(ins[0]
            .sql
            .starts_with("INSERT OR IGNORE INTO new_deal_alerts"));
        assert_eq!(ins[0].params[0], Param::Text("cheap".into()));
        assert_eq!(ins[0].params[1], Param::Int(20000));
        assert!(matches!(ins[0].params[3], Param::Real(_)));
        assert_eq!(ins[0].params[8], Param::Int(2019));
        let n = notified_statements(&alerts, "T2");
        assert_eq!(
            n[0].params,
            vec![
                Param::Text("T2".into()),
                Param::Text("cheap".into()),
                Param::Int(20000)
            ]
        );

        let row = json!({"created_at": "T", "notified_at": null, "deal": ins[0].params[10].as_str().unwrap()});
        let v = from_row(&row);
        assert_eq!(v["id"], "cheap");
        assert_eq!(v["alertedAt"], "T");
        assert_eq!(v["notifiedAt"], Value::Null);
        assert!(v["score"]["baseline"].as_f64().unwrap() > 20000.0);
        assert_eq!(
            from_row(&json!({"deal": "not json"}))["alertedAt"],
            Value::Null
        );

        let ids: Vec<String> = (0..95).map(|i| i.to_string()).collect();
        let q = existing_queries(&ids);
        assert_eq!((q.len(), q[0].params.len(), q[1].params.len()), (2, 90, 5));
        assert_eq!(
            key_from_row(&json!({"listing_id": "a", "price": 5.0})),
            Some(("a".into(), 5))
        );
    }

    #[test]
    fn list_query() {
        assert_eq!(
            AlertsQuery::from_pairs([]).unwrap().limit,
            DEFAULT_LIST_LIMIT
        );
        assert_eq!(
            AlertsQuery::from_pairs([("limit", "999")]).unwrap().limit,
            MAX_LIST_LIMIT
        );
        assert_eq!(
            AlertsQuery::from_pairs([("limit", "5"), ("x", "y")])
                .unwrap()
                .limit,
            5
        );
        assert!(AlertsQuery::from_pairs([("limit", "many")]).is_err());
    }

    #[test]
    fn summary_is_one_line_per_alert() {
        let alerts = find(&market(), &["cheap".into()], &AlertRule::default()).unwrap();
        let s = summary(&alerts);
        assert_eq!(s.lines().count(), 1);
        assert!(s.contains("2019 Toyota RAV4 LE: 20000 $"), "{s}");
        assert!(s.contains("https://www.example.com/annonces/cheap"));
    }
}
