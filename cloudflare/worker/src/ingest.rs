//! POST /api/listings: the same upsert rules as the Go `sqlitestore` (itself a
//! port of `src/db.js`), planned as plain statements.
//!
//! - New id: insert the row, and a price_history row when it has a price.
//! - Known id: overwrite the mutable columns, bump `price_changes` and add a
//!   price_history row when the price changed, clear `removed_at` (relisted).
//! - `is_sold` / `sold_at` can be turned on but never off; `page_read_at` is
//!   never blanked.

use std::collections::HashMap;

use serde::Serialize;
use serde_json::Value;

use crate::sql::{placeholders, Param, Stmt};

/// Columns written on insert, in the Go store's order.
pub const COLUMNS: [&str; 42] = [
    "id", "source", "url", "reference_id", "make_slug", "model_slug", "geo_slug",
    "make", "model", "model_detail", "trim_text", "year", "km", "price",
    "first_price", "transmission", "fuel", "engine_ccm", "is_damaged", "is_parts",
    "is_conditional_price", "is_sold", "sold_at", "page_read_at",
    "condition", "seller_type", "seller_id", "seller_name", "city",
    "postal_code", "province", "description", "image_count",
    "listed_at", "price_rating", "vin", "carfax_url", "latitude", "longitude",
    "image_urls",
    "first_seen", "last_seen",
];

/// Columns a re-crawl may overwrite.
pub const MUTABLE: [&str; 25] = [
    "url", "price", "km", "trim_text", "description", "image_count",
    "seller_name", "city", "postal_code", "province",
    "make", "model", "model_detail", "year",
    "listed_at", "price_rating", "vin", "carfax_url", "latitude", "longitude",
    "image_urls",
    "geo_slug",
    "is_sold", "sold_at",
    "page_read_at",
];

const STICKY: [&str; 2] = ["is_sold", "sold_at"];
const KEEP_IF_ABSENT: [&str; 1] = ["page_read_at"];

/// Most listings one request may carry. The Go client sends far fewer.
pub const MAX_LISTINGS_PER_REQUEST: usize = 1000;

/// Which query found the listings (Go `store.Scope`).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Scope {
    pub make_slug: Option<String>,
    pub model_slug: Option<String>,
    pub geo_slug: Option<String>,
    pub seen_at: String,
}

/// What a batch changed (Go `store.SaveStats`, camelCase on the wire).
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SaveStats {
    pub seen: usize,
    pub added: usize,
    pub relisted: usize,
    pub price_drops: usize,
    pub price_rises: usize,
    /// Listings refused because they had no string id.
    pub skipped: usize,
}

/// The stored state of a known listing that the upsert rules need.
#[derive(Debug, Clone, PartialEq)]
pub struct Existing {
    pub price: Option<f64>,
    pub price_changes: i64,
    pub removed: bool,
}

/// A parsed request body.
#[derive(Debug, Clone)]
pub struct Payload {
    pub listings: Vec<Value>,
    pub scope: Scope,
}

fn opt_str(v: Option<&Value>) -> Option<String> {
    v.and_then(Value::as_str).filter(|s| !s.is_empty()).map(str::to_string)
}

/// Accepts `[listing, ...]` or `{"listings": [...], "scope": {makeSlug, modelSlug, geoSlug, seenAt}}`.
/// `now` fills in `seenAt` when the sender gave none.
pub fn parse_payload(body: &Value, now: &str) -> Result<Payload, String> {
    let (rows, scope) = match body {
        Value::Array(rows) => (rows.clone(), None),
        Value::Object(obj) => {
            let rows = obj
                .get("listings")
                .and_then(Value::as_array)
                .cloned()
                .ok_or("body needs a \"listings\" array")?;
            (rows, obj.get("scope"))
        }
        _ => return Err("body must be a JSON array of listings or {\"listings\": [...], \"scope\": {...}}".into()),
    };
    if rows.len() > MAX_LISTINGS_PER_REQUEST {
        return Err(format!("too many listings in one request ({} > {MAX_LISTINGS_PER_REQUEST})", rows.len()));
    }
    let scope = Scope {
        make_slug: opt_str(scope.and_then(|s| s.get("makeSlug"))),
        model_slug: opt_str(scope.and_then(|s| s.get("modelSlug"))),
        geo_slug: opt_str(scope.and_then(|s| s.get("geoSlug"))),
        seen_at: opt_str(scope.and_then(|s| s.get("seenAt"))).unwrap_or_else(|| now.to_string()),
    };
    Ok(Payload { listings: rows, scope })
}

/// The listing's id, when it has a usable one.
pub fn listing_id(l: &Value) -> Option<&str> {
    l.get("id").and_then(Value::as_str).filter(|s| !s.is_empty())
}

fn flag(l: &Value, key: &str) -> Param {
    Param::Int(i64::from(l.get(key) == Some(&Value::Bool(true))))
}

/// The row for one listing (Go `toRow`), keyed by column.
pub fn to_row(l: &Value, scope: &Scope) -> HashMap<&'static str, Param> {
    let s = |k: &str| Param::text(l.get(k).and_then(Value::as_str));
    let n = |k: &str| Param::num(l.get(k).and_then(Value::as_f64));
    let source = l
        .get("source")
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .unwrap_or("autohebdo");
    let image_urls = match l.get("imageUrls").and_then(Value::as_array) {
        Some(a) if !a.is_empty() => Param::Text(Value::Array(a.clone()).to_string()),
        _ => Param::Null,
    };
    let image_count = l.get("imageCount").and_then(Value::as_f64).unwrap_or(0.0);
    HashMap::from([
        ("id", s("id")),
        ("source", Param::Text(source.to_string())),
        ("url", s("url")),
        ("reference_id", s("referenceId")),
        ("make_slug", Param::text(scope.make_slug.as_deref())),
        ("model_slug", Param::text(scope.model_slug.as_deref())),
        ("geo_slug", Param::text(scope.geo_slug.as_deref())),
        ("make", s("make")),
        ("model", s("model")),
        ("model_detail", s("modelDetail")),
        ("trim_text", s("trimText")),
        ("year", n("year")),
        ("km", n("km")),
        ("price", n("price")),
        ("first_price", n("price")),
        ("transmission", s("transmission")),
        ("fuel", s("fuel")),
        ("engine_ccm", n("engineCcm")),
        ("is_damaged", flag(l, "isDamaged")),
        ("is_parts", flag(l, "isParts")),
        ("is_conditional_price", flag(l, "isConditionalPrice")),
        // AutoHebdo publishes no sold flag, page-read time, dates, VIN or coordinates.
        ("is_sold", Param::Int(0)),
        ("sold_at", Param::Null),
        ("page_read_at", Param::Null),
        ("condition", s("condition")),
        ("seller_type", s("sellerType")),
        ("seller_id", s("sellerId")),
        ("seller_name", s("sellerName")),
        ("city", s("city")),
        ("postal_code", s("postalCode")),
        ("province", s("province")),
        ("description", s("description")),
        ("image_count", Param::num(Some(image_count.trunc()))),
        ("listed_at", Param::Null),
        ("price_rating", Param::Null),
        ("vin", Param::Null),
        ("carfax_url", Param::Null),
        ("latitude", Param::Null),
        ("longitude", Param::Null),
        ("image_urls", image_urls),
        ("first_seen", Param::Text(scope.seen_at.clone())),
        ("last_seen", Param::Text(scope.seen_at.clone())),
    ])
}

fn values(row: &HashMap<&'static str, Param>, keys: &[&str]) -> Vec<Param> {
    keys.iter().map(|k| row.get(k).cloned().unwrap_or(Param::Null)).collect()
}

fn insert_sql() -> String {
    format!("INSERT INTO listings ({}) VALUES ({})", COLUMNS.join(", "), placeholders(1, COLUMNS.len()))
}

/// `UPDATE listings SET <mutable> ..., last_seen = ?, price_changes = ?, removed_at = NULL WHERE id = ?`
fn update_sql() -> String {
    let sets: Vec<String> = MUTABLE
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let p = i + 1;
            if STICKY.contains(c) {
                format!("{c} = COALESCE(NULLIF({c}, 0), ?{p})")
            } else if KEEP_IF_ABSENT.contains(c) {
                format!("{c} = COALESCE(?{p}, {c})")
            } else {
                format!("{c} = ?{p}")
            }
        })
        .collect();
    let n = MUTABLE.len();
    format!(
        "UPDATE listings SET {}, last_seen = ?{}, price_changes = ?{}, removed_at = NULL WHERE id = ?{}",
        sets.join(", "),
        n + 1,
        n + 2,
        n + 3
    )
}

fn price_history(row: &HashMap<&'static str, Param>, seen_at: &str) -> Stmt {
    Stmt::new(
        "INSERT OR REPLACE INTO price_history (listing_id, seen_at, price, km) VALUES (?1, ?2, ?3, ?4)",
        vec![row["id"].clone(), Param::Text(seen_at.to_string()), row["price"].clone(), row["km"].clone()],
    )
}

/// Plans the writes for a batch. It remembers what it planned, so a listing
/// that appears twice in one batch is treated as known the second time, just
/// as the Go store sees its own earlier insert inside the transaction.
pub struct Planner {
    known: HashMap<String, Existing>,
    insert: String,
    update: String,
    pub stats: SaveStats,
    /// Ids that are new to the store, back after a removal, or cheaper than
    /// before: the only listings a "new deal" alert can be about. In plan
    /// order, without repeats.
    pub fresh: Vec<String>,
}

impl Planner {
    pub fn new(known: HashMap<String, Existing>) -> Planner {
        Planner { known, insert: insert_sql(), update: update_sql(), stats: SaveStats::default(), fresh: Vec::new() }
    }

    /// Statements for one listing (Go `upsert`). Listings without an id are skipped.
    pub fn plan(&mut self, l: &Value, scope: &Scope) -> Vec<Stmt> {
        let Some(id) = listing_id(l).map(str::to_string) else {
            self.stats.skipped += 1;
            return Vec::new();
        };
        let row = to_row(l, scope);
        let new_price = row["price"].as_f64();
        self.stats.seen += 1;

        let Some(existing) = self.known.get(&id).cloned() else {
            let mut out = vec![Stmt::new(self.insert.clone(), values(&row, &COLUMNS))];
            if new_price.is_some() {
                out.push(price_history(&row, &scope.seen_at));
            }
            self.stats.added += 1;
            self.mark_fresh(&id);
            self.known.insert(id, Existing { price: new_price, price_changes: 0, removed: false });
            return out;
        };

        let changed = new_price.is_some_and(|p| existing.price != Some(p));
        let next = existing.price_changes + i64::from(changed);
        let mut params = values(&row, &MUTABLE);
        params.push(Param::Text(scope.seen_at.clone()));
        params.push(Param::Int(next));
        params.push(Param::Text(id.clone()));
        let mut out = vec![Stmt::new(self.update.clone(), params)];
        if existing.removed {
            self.stats.relisted += 1;
            self.mark_fresh(&id);
        }
        if changed {
            out.push(price_history(&row, &scope.seen_at));
            // A missing old price counts as 0, as in JS: that is a rise.
            let delta = new_price.unwrap_or(0.0) - existing.price.unwrap_or(0.0);
            if delta < 0.0 {
                self.stats.price_drops += 1;
                self.mark_fresh(&id);
            } else {
                self.stats.price_rises += 1;
            }
        }
        self.known.insert(id, Existing { price: new_price, price_changes: next, removed: false });
        out
    }

    fn mark_fresh(&mut self, id: &str) {
        if !self.fresh.iter().any(|f| f == id) {
            self.fresh.push(id.to_string());
        }
    }
}

/// `SELECT ... WHERE id IN (...)` for the ids of a batch, chunked so each
/// statement stays under D1's bound-parameter limit (100).
pub fn existing_queries(ids: &[String]) -> Vec<Stmt> {
    ids.chunks(90)
        .map(|chunk| {
            Stmt::new(
                format!(
                    "SELECT id, price, price_changes, removed_at FROM listings WHERE id IN ({})",
                    placeholders(1, chunk.len())
                ),
                chunk.iter().map(|id| Param::Text(id.clone())).collect(),
            )
        })
        .collect()
}

/// Reads one row of an `existing_queries` result.
pub fn existing_from_row(row: &Value) -> Option<(String, Existing)> {
    let id = row.get("id")?.as_str()?.to_string();
    Some((
        id,
        Existing {
            price: row.get("price").and_then(Value::as_f64),
            price_changes: row.get("price_changes").and_then(Value::as_f64).unwrap_or(0.0) as i64,
            removed: row.get("removed_at").is_some_and(|v| !v.is_null()),
        },
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn scope(at: &str) -> Scope {
        Scope { make_slug: Some("toyota".into()), model_slug: Some("rav4".into()), geo_slug: Some("reg_qc".into()), seen_at: at.into() }
    }

    #[test]
    fn payload_accepts_array_or_object() {
        let p = parse_payload(&json!([{"id": "a"}]), "NOW").unwrap();
        assert_eq!(p.listings.len(), 1);
        assert_eq!(p.scope.seen_at, "NOW");
        let p = parse_payload(
            &json!({"listings": [], "scope": {"makeSlug": "toyota", "seenAt": "2026-01-01T00:00:00.000Z"}}),
            "NOW",
        )
        .unwrap();
        assert_eq!(p.scope.make_slug.as_deref(), Some("toyota"));
        assert_eq!(p.scope.model_slug, None);
        assert_eq!(p.scope.seen_at, "2026-01-01T00:00:00.000Z");
        assert!(parse_payload(&json!({"nope": 1}), "NOW").is_err());
        assert!(parse_payload(&json!("x"), "NOW").is_err());
        let too_many: Vec<Value> = (0..=MAX_LISTINGS_PER_REQUEST).map(|i| json!({"id": i.to_string()})).collect();
        assert!(parse_payload(&Value::Array(too_many), "NOW").is_err());
    }

    #[test]
    fn row_matches_the_go_store() {
        let l = json!({
            "id": "a1", "url": "https://x", "make": "Toyota", "model": "RAV4", "year": 2019, "km": 60000,
            "price": 24000, "trimText": "XLE", "sellerType": "Dealer", "isDamaged": true, "isParts": "yes",
            "imageCount": 3, "imageUrls": ["u1", "u2"], "source": ""
        });
        let r = to_row(&l, &scope("T1"));
        assert_eq!(r.len(), COLUMNS.len());
        assert_eq!(r["source"], Param::Text("autohebdo".into()));
        assert_eq!(r["year"], Param::Int(2019));
        assert_eq!(r["first_price"], Param::Int(24000));
        assert_eq!(r["is_damaged"], Param::Int(1));
        assert_eq!(r["is_parts"], Param::Int(0), "only a real true counts");
        assert_eq!(r["image_urls"], Param::Text("[\"u1\",\"u2\"]".into()));
        assert_eq!(r["make_slug"], Param::Text("toyota".into()));
        assert_eq!(r["first_seen"], Param::Text("T1".into()));
    }

    #[test]
    fn insert_then_price_drop_then_relist() {
        let mut p = Planner::new(HashMap::new());
        let ins = p.plan(&json!({"id": "a", "price": 20000, "km": 1}), &scope("T1"));
        assert_eq!(ins.len(), 2, "insert + price history");
        assert!(ins[0].sql.starts_with("INSERT INTO listings"));
        assert_eq!(ins[0].params.len(), COLUMNS.len());
        assert!(ins[1].sql.contains("price_history"));

        // Same id again in the same batch: now an update with a price drop.
        let upd = p.plan(&json!({"id": "a", "price": 19000}), &scope("T2"));
        assert_eq!(upd.len(), 2);
        assert!(upd[0].sql.starts_with("UPDATE listings SET url = ?1"));
        assert!(upd[0].sql.contains("is_sold = COALESCE(NULLIF(is_sold, 0), ?23)"));
        assert!(upd[0].sql.contains("page_read_at = COALESCE(?25, page_read_at)"));
        assert!(upd[0].sql.ends_with("last_seen = ?26, price_changes = ?27, removed_at = NULL WHERE id = ?28"));
        assert_eq!(upd[0].params[26], Param::Int(1));
        assert_eq!(upd[0].params[27], Param::Text("a".into()));

        // Unchanged price: update only, no history row.
        assert_eq!(p.plan(&json!({"id": "a", "price": 19000}), &scope("T3")).len(), 1);
        // No id: skipped.
        assert!(p.plan(&json!({"price": 1}), &scope("T3")).is_empty());
        assert_eq!(
            p.stats,
            SaveStats { seen: 3, added: 1, relisted: 0, price_drops: 1, price_rises: 0, skipped: 1 }
        );
    }

    #[test]
    fn known_rows_count_relists_and_rises() {
        let known = HashMap::from([
            ("r".to_string(), Existing { price: Some(10000.0), price_changes: 2, removed: true }),
            ("n".to_string(), Existing { price: None, price_changes: 0, removed: false }),
        ]);
        let mut p = Planner::new(known);
        let s = p.plan(&json!({"id": "r", "price": 11000}), &scope("T"));
        assert_eq!(s[0].params[26], Param::Int(3));
        p.plan(&json!({"id": "n", "price": 5000}), &scope("T"));
        assert_eq!(p.stats.relisted, 1);
        assert_eq!(p.stats.price_rises, 2, "a first price on a priceless row is a rise, as in JS");
    }

    #[test]
    fn fresh_ids_are_new_relisted_or_cheaper() {
        let known = HashMap::from([
            ("same".to_string(), Existing { price: Some(100.0), price_changes: 0, removed: false }),
            ("rise".to_string(), Existing { price: Some(100.0), price_changes: 0, removed: false }),
            ("drop".to_string(), Existing { price: Some(100.0), price_changes: 0, removed: false }),
            ("back".to_string(), Existing { price: Some(100.0), price_changes: 0, removed: true }),
        ]);
        let mut p = Planner::new(known);
        for l in [
            json!({"id": "new", "price": 1}),
            json!({"id": "same", "price": 100}),
            json!({"id": "rise", "price": 120}),
            json!({"id": "drop", "price": 90}),
            json!({"id": "back", "price": 100}),
            json!({"id": "new", "price": 1}),
        ] {
            p.plan(&l, &scope("T"));
        }
        assert_eq!(p.fresh, vec!["new", "drop", "back"], "each id once, in plan order");
    }

    #[test]
    fn existing_lookup_is_chunked_and_parsed() {
        let ids: Vec<String> = (0..200).map(|i| i.to_string()).collect();
        let qs = existing_queries(&ids);
        assert_eq!(qs.len(), 3);
        assert_eq!(qs[0].params.len(), 90);
        assert!(qs[2].sql.ends_with("IN (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20)"));
        let (id, e) = existing_from_row(&json!({"id": "x", "price": 5.0, "price_changes": 1.0, "removed_at": "T"})).unwrap();
        assert_eq!(id, "x");
        assert_eq!(e, Existing { price: Some(5.0), price_changes: 1, removed: true });
    }
}
