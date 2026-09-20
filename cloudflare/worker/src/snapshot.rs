//! The daily "deals snapshot": what the cron trigger stores after re-scoring.
//!
//! One `deal_snapshots` row per run plus one `deal_snapshot_items` row per
//! deal, written in a single D1 batch (a transaction), then old snapshots
//! beyond `KEEP` are pruned.

use serde_json::{json, Value};

use crate::sql::{Param, Stmt};

/// Deals kept per snapshot.
pub const SNAPSHOT_DEALS: usize = 100;
/// Snapshots kept; older ones are deleted by each run.
pub const KEEP: usize = 30;

/// Statements that store `deals` (the output of `deals::best_deals`).
pub fn statements(deals: &Value, created_at: &str, cron: Option<&str>, min_comps: Option<f64>) -> Vec<Stmt> {
    let count = |k: &str| Param::num(deals.get(k).and_then(Value::as_f64).or(Some(0.0)));
    let mut out = vec![Stmt::new(
        "INSERT INTO deal_snapshots (created_at, cron, min_comps, comps, scored, matched, appraiser) \
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
        vec![
            Param::Text(created_at.to_string()),
            Param::text(cron),
            Param::num(min_comps),
            count("comps"),
            count("scored"),
            count("matched"),
            Param::Text(deals.get("appraiser").cloned().unwrap_or(Value::Null).to_string()),
        ],
    )];
    let items = deals.get("deals").and_then(Value::as_array).cloned().unwrap_or_default();
    for (rank, d) in items.iter().enumerate() {
        let s = |k: &str| Param::text(d.get(k).and_then(Value::as_str));
        let n = |v: Option<&Value>| Param::num(v.and_then(Value::as_f64));
        let score = d.get("score");
        out.push(Stmt::new(
            // Inside the batch transaction MAX(id) is the header just inserted.
            "INSERT INTO deal_snapshot_items \
             (snapshot_id, rank, listing_id, make, model, year, price, baseline, discount_pct, url, deal) \
             VALUES ((SELECT MAX(id) FROM deal_snapshots), ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
            vec![
                Param::Int(rank as i64 + 1),
                s("id"),
                s("make"),
                s("model"),
                n(d.get("year")),
                n(d.get("price")),
                n(score.and_then(|s| s.get("baseline"))),
                // Not Param::num: a percentage must stay REAL even when whole.
                score
                    .and_then(|s| s.get("discountPct"))
                    .and_then(Value::as_f64)
                    .map_or(Param::Null, Param::Real),
                s("url"),
                Param::Text(d.to_string()),
            ],
        ));
    }
    let old = format!("SELECT id FROM deal_snapshots ORDER BY id DESC LIMIT -1 OFFSET {KEEP}");
    out.push(Stmt::new(format!("DELETE FROM deal_snapshot_items WHERE snapshot_id IN ({old})"), vec![]));
    out.push(Stmt::new(format!("DELETE FROM deal_snapshots WHERE id IN ({old})"), vec![]));
    out
}

pub const LATEST_HEADER_SQL: &str =
    "SELECT id, created_at, cron, min_comps, comps, scored, matched, appraiser FROM deal_snapshots ORDER BY id DESC LIMIT 1";
pub const ITEMS_SQL: &str = "SELECT deal FROM deal_snapshot_items WHERE snapshot_id = ?1 ORDER BY rank";

/// The API shape of a stored snapshot: the header plus its deals, like `/api/deals`.
pub fn assemble(header: &Value, items: &[Value]) -> Value {
    let parse = |v: Option<&Value>| {
        v.and_then(Value::as_str).and_then(|s| serde_json::from_str::<Value>(s).ok()).unwrap_or(Value::Null)
    };
    json!({
        "id": header.get("id"),
        "createdAt": header.get("created_at"),
        "cron": header.get("cron"),
        "minComps": header.get("min_comps"),
        "comps": header.get("comps"),
        "scored": header.get("scored"),
        "matched": header.get("matched"),
        "appraiser": parse(header.get("appraiser")),
        "deals": items.iter().map(|i| parse(i.get("deal"))).collect::<Vec<_>>(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn header_items_and_pruning() {
        let deals = json!({"comps": 20, "scored": 17, "matched": 2, "appraiser": {"sources": []}, "deals": [
            {"id": "a", "make": "Toyota", "model": "RAV4", "year": 2019, "price": 21000, "url": "u",
             "score": {"baseline": 25000.0, "discountPct": 16.0}},
            {"id": "b", "score": {"baseline": 1.0, "discountPct": -2.5}}
        ]});
        let s = statements(&deals, "T", Some("0 11 * * *"), Some(3.0));
        assert_eq!(s.len(), 5);
        assert!(s[0].sql.starts_with("INSERT INTO deal_snapshots"));
        assert_eq!(s[0].params[4], Param::Int(17));
        assert_eq!(s[1].params[0], Param::Int(1));
        assert_eq!(s[1].params[7], Param::Real(16.0));
        assert_eq!(s[2].params[1], Param::Text("b".into()));
        assert!(s[3].sql.contains("OFFSET 30"));
        assert!(s[4].sql.starts_with("DELETE FROM deal_snapshots"));
    }

    #[test]
    fn assemble_parses_json_columns() {
        let h = json!({"id": 4.0, "created_at": "T", "appraiser": "{\"sources\":[\"autohebdo\"]}", "comps": 20.0});
        let items = vec![json!({"deal": "{\"id\":\"a\"}"})];
        let v = assemble(&h, &items);
        assert_eq!(v["appraiser"]["sources"][0], "autohebdo");
        assert_eq!(v["deals"][0]["id"], "a");
        assert_eq!(v["createdAt"], "T");
    }
}
