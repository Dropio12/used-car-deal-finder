//! The listing shape the scorer reads: the camelCase JSON the crawler writes
//! (same keys as `normalizeListing()` / `loadListings()` in the JS code).
//!
//! Reading is deliberately lenient, like the JS it replaces: a field that is
//! missing or of the wrong type is treated as absent rather than failing the
//! whole batch. Boolean flags keep their raw JSON value because the JS code
//! tests some of them for truthiness and one (`isDamaged === true`) strictly.

use serde_json::Value;

#[derive(Debug, Clone, Default)]
pub struct Listing {
    pub id: Value,
    pub source: Option<String>,
    pub make: Option<String>,
    pub model: Option<String>,
    pub year: Option<f64>,
    pub km: Option<f64>,
    pub price: Option<f64>,
    pub trim_text: Option<String>,
    /// An already-resolved trim, if the caller has one. Normally absent.
    pub trim: Option<String>,
    pub seller_type: Option<String>,
    pub province: Option<String>,
    pub is_damaged: Value,
    pub is_parts: Value,
    pub is_conditional_price: Value,
}

fn string(v: Option<&Value>) -> Option<String> {
    v.and_then(Value::as_str).map(str::to_string)
}

fn number(v: Option<&Value>) -> Option<f64> {
    v.and_then(Value::as_f64)
}

impl Listing {
    pub fn from_value(v: &Value) -> Listing {
        let get = |k: &str| v.get(k);
        Listing {
            id: get("id").cloned().unwrap_or(Value::Null),
            source: string(get("source")),
            make: string(get("make")),
            model: string(get("model")),
            year: number(get("year")),
            km: number(get("km")),
            price: number(get("price")),
            trim_text: string(get("trimText")),
            trim: string(get("trim")),
            seller_type: string(get("sellerType")),
            province: string(get("province")),
            is_damaged: get("isDamaged").cloned().unwrap_or(Value::Null),
            is_parts: get("isParts").cloned().unwrap_or(Value::Null),
            is_conditional_price: get("isConditionalPrice").cloned().unwrap_or(Value::Null),
        }
    }

    pub fn is_seller(&self, kind: &str) -> bool {
        self.seller_type.as_deref() == Some(kind)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn reads_the_crawler_shape() {
        let l = Listing::from_value(&json!({
            "id": "abc", "source": "autohebdo", "make": "Toyota", "model": "RAV4",
            "year": 2019, "km": 60000, "price": 24000, "trimText": "XLE AWD",
            "sellerType": "Dealer", "province": "QC", "isDamaged": false
        }));
        assert_eq!(l.make.as_deref(), Some("Toyota"));
        assert_eq!(l.year, Some(2019.0));
        assert!(l.is_seller("Dealer"));
        assert_eq!(l.is_damaged, Value::Bool(false));
    }

    #[test]
    fn wrong_types_read_as_missing() {
        let l = Listing::from_value(&json!({ "price": "24 000 $", "make": 5 }));
        assert_eq!(l.price, None);
        assert_eq!(l.make, None);
        assert_eq!(l.is_parts, Value::Null);
    }
}
