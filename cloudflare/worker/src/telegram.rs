//! Optional Telegram message per snipe alert, sent through Composio's
//! Telegram toolkit (`TELEGRAM_SEND_MESSAGE`). The bot token lives in
//! Composio; the Worker only holds a Composio API key.
//!
//! Off unless all three are set:
//!   COMPOSIO_API_KEY   secret  (`npx wrangler secret put COMPOSIO_API_KEY`)
//!   COMPOSIO_USER_ID   var     the user the Telegram account is connected for
//!   TELEGRAM_CHAT_ID   var     your chat with the bot
//!
//! Plain Rust: it shapes the request and the message; `notify_http.rs` sends.

use serde_json::{json, Value};

use crate::alerts::Alert;

pub const EXECUTE_URL: &str =
    "https://backend.composio.dev/api/v3.1/tools/execute/TELEGRAM_SEND_MESSAGE";
/// Telegram's own limit on one message.
const MAX_TEXT: usize = 4096;

#[derive(Debug, Clone, PartialEq)]
pub struct ComposioConfig {
    pub api_key: String,
    pub user_id: String,
    pub chat_id: String,
}

impl ComposioConfig {
    /// `None` unless every value is present and non-blank.
    pub fn from_vars(get: impl Fn(&str) -> Option<String>) -> Option<ComposioConfig> {
        let val = |k: &str| get(k).map(|v| v.trim().to_string()).filter(|v| !v.is_empty());
        Some(ComposioConfig {
            api_key: val("COMPOSIO_API_KEY")?,
            user_id: val("COMPOSIO_USER_ID")?,
            chat_id: val("TELEGRAM_CHAT_ID")?,
        })
    }

    /// The JSON body for Composio's execute endpoint.
    pub fn request_body(&self, text: &str) -> Value {
        let text: String = text.chars().take(MAX_TEXT).collect();
        json!({ "user_id": self.user_id, "arguments": { "chat_id": self.chat_id, "text": text } })
    }
}

/// Composio answers HTTP 200 with `successful: false` when Telegram refuses
/// (wrong chat, bot blocked). That is a failure, not a delivery.
pub fn delivered(status: u16, body: &str) -> Result<(), String> {
    let v: Value = serde_json::from_str(body).unwrap_or(Value::Null);
    if (200..300).contains(&status) && v.get("successful") != Some(&Value::Bool(false)) {
        return Ok(());
    }
    let why = v
        .get("error")
        .and_then(Value::as_str)
        .or_else(|| v.get("message").and_then(Value::as_str))
        .unwrap_or("send failed");
    Err(format!("Composio {status}: {why}"))
}

/// Plain text, no Markdown: seller-written trims would otherwise need escaping
/// for every `_` and `*`, and one miss makes Telegram reject the message.
pub fn message(alert: &Alert, photo_notes: Option<&str>) -> String {
    let d = &alert.deal;
    let text = |k: &str| d.get(k).and_then(Value::as_str).unwrap_or("").trim();
    let num = |v: Option<&Value>| v.and_then(Value::as_f64);
    let money = |v: Option<f64>| v.map_or("?".to_string(), |n| format!("{} $", group(n)));
    let score = d.get("score");

    let car = [
        num(d.get("year")).map(|y| format!("{y}")).unwrap_or_default(),
        text("make").to_string(),
        text("model").to_string(),
    ]
    .into_iter()
    .filter(|s| !s.is_empty())
    .collect::<Vec<_>>()
    .join(" ");

    let mut lines = vec![
        format!("🚗 {}: -{:.0}% under market", if car.is_empty() { "A car" } else { &car }, alert.discount_pct),
        format!(
            "{} vs {} typical ({} comparable cars)",
            money(num(d.get("price"))),
            money(num(score.and_then(|s| s.get("baseline")))),
            num(score.and_then(|s| s.get("n"))).map_or("?".into(), |n| format!("{n:.0}")),
        ),
    ];
    let place = [
        num(d.get("km")).map(|k| format!("{} km", group(k))).unwrap_or_default(),
        text("city").to_string(),
    ]
    .into_iter()
    .filter(|s| !s.is_empty())
    .collect::<Vec<_>>()
    .join(" · ");
    if !place.is_empty() {
        lines.push(place);
    }
    if let Some(n) = photo_notes.filter(|n| !n.is_empty()) {
        lines.push(format!("Photos (AI): {n}"));
    }
    if !text("url").is_empty() {
        lines.push(text("url").to_string());
    }
    lines.join("\n")
}

/// 27500 -> "27 500", the way the rest of the project writes money.
fn group(n: f64) -> String {
    let digits = format!("{}", n.round().abs() as i64);
    let mut out = String::new();
    for (i, c) in digits.chars().enumerate() {
        if i > 0 && (digits.len() - i) % 3 == 0 {
            out.push(' ');
        }
        out.push(c);
    }
    if n < 0.0 {
        format!("-{out}")
    } else {
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn alert() -> Alert {
        Alert {
            listing_id: "a1".into(),
            price: 19000,
            discount_pct: 30.9,
            deal: json!({"id": "a1", "year": 2021, "make": "Toyota", "model": "RAV4", "price": 19000,
                         "km": 129000, "city": "Laval", "url": "https://x/1",
                         "score": {"baseline": 27500, "n": 32, "discountPct": 30.9}}),
        }
    }

    #[test]
    fn config_needs_all_three_values() {
        let full = |k: &str| {
            Some(match k { "COMPOSIO_API_KEY" => "k", "COMPOSIO_USER_ID" => "me", "TELEGRAM_CHAT_ID" => "42", _ => "" }.to_string())
        };
        let c = ComposioConfig::from_vars(full).unwrap();
        assert_eq!(c.chat_id, "42");
        assert!(ComposioConfig::from_vars(|k| (k != "TELEGRAM_CHAT_ID").then(|| "x".to_string())).is_none());
        assert!(ComposioConfig::from_vars(|_| Some("  ".to_string())).is_none());
    }

    #[test]
    fn body_targets_the_user_and_chat() {
        let c = ComposioConfig { api_key: "k".into(), user_id: "me".into(), chat_id: "42".into() };
        assert_eq!(c.request_body("hi"), json!({"user_id": "me", "arguments": {"chat_id": "42", "text": "hi"}}));
        let long = "é".repeat(5000);
        assert_eq!(c.request_body(&long)["arguments"]["text"].as_str().unwrap().chars().count(), 4096);
    }

    #[test]
    fn a_refusal_inside_a_200_is_a_failure() {
        assert!(delivered(200, r#"{"successful": true}"#).is_ok());
        assert_eq!(delivered(200, r#"{"successful": false, "error": "chat not found"}"#).unwrap_err(), "Composio 200: chat not found");
        assert!(delivered(401, r#"{"message": "bad key"}"#).unwrap_err().contains("bad key"));
    }

    #[test]
    fn message_says_the_gap_prices_photos_and_link() {
        let m = message(&alert(), Some("Clean body"));
        assert!(m.starts_with("🚗 2021 Toyota RAV4: -31% under market"), "{m}");
        assert!(m.contains("19 000 $ vs 27 500 $ typical (32 comparable cars)"), "{m}");
        assert!(m.contains("129 000 km · Laval"), "{m}");
        assert!(m.contains("Photos (AI): Clean body"));
        assert!(m.ends_with("https://x/1"));
        assert!(!message(&alert(), None).contains("Photos"));
    }
}
