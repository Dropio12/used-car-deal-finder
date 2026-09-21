//! The Telegram notifier (wasm only): for each new snipe alert, optionally an
//! AI photo check on Baseten, then a message through Composio. Chosen in
//! `entry.rs` when the Composio settings are present.

use serde_json::Value;
use worker::wasm_bindgen::JsValue;
use worker::{console_error, Fetch, Headers, Method, Request, RequestInit};

use crate::alerts::{Alert, NoNotifier, Notifier};
use crate::d1::D1Store;
use crate::telegram::{self, ComposioConfig};
use crate::vision::{self, VisionConfig};

/// One ingest can raise many alerts. Each photo check is a slow model call and
/// each message a subrequest, so both are capped per ingest; the rest still
/// land on the dashboard.
const MAX_PHOTO_CHECKS: usize = 3;
const MAX_MESSAGES: usize = 10;

pub struct TelegramNotifier {
    pub composio: ComposioConfig,
    pub vision: Option<VisionConfig>,
    pub store: D1Store,
}

async fn post_json(url: &str, headers: &[(&str, &str)], body: &Value) -> Result<(u16, String), String> {
    let h = Headers::new();
    h.set("Content-Type", "application/json").map_err(|e| e.to_string())?;
    for (k, v) in headers {
        h.set(k, v).map_err(|e| e.to_string())?;
    }
    let mut init = RequestInit::new();
    init.with_method(Method::Post)
        .with_headers(h)
        .with_body(Some(JsValue::from_str(&body.to_string())));
    let req = Request::new_with_init(url, &init).map_err(|e| e.to_string())?;
    let mut res = Fetch::Request(req).send().await.map_err(|e| e.to_string())?;
    let status = res.status_code();
    Ok((status, res.text().await.map_err(|e| e.to_string())?))
}

impl TelegramNotifier {
    async fn photo_notes(&self, alert: &Alert) -> Option<String> {
        let cfg = self.vision.as_ref()?;
        let urls = match self.store.image_urls(&alert.listing_id).await {
            Ok(u) => u,
            Err(e) => {
                console_error!("photo check: {e}");
                return None;
            }
        };
        let d = &alert.deal;
        let car = ["year", "make", "model"]
            .iter()
            .filter_map(|k| d.get(*k).map(|v| v.as_str().map(str::to_string).unwrap_or_else(|| v.to_string())))
            .filter(|s| s != "null")
            .collect::<Vec<_>>()
            .join(" ");
        let body = cfg.request_body(&car, &urls)?;
        let auth = format!("Bearer {}", cfg.api_key);
        match post_json(vision::CHAT_URL, &[("Authorization", &auth)], &body).await {
            Ok((200, text)) => vision::notes_from_reply(&text),
            Ok((status, text)) => {
                console_error!("photo check: Baseten {status}: {}", text.chars().take(200).collect::<String>());
                None
            }
            Err(e) => {
                console_error!("photo check: {e}");
                None
            }
        }
    }
}

impl Notifier for TelegramNotifier {
    async fn notify(&self, alerts: &[Alert]) -> Result<usize, String> {
        let mut sent = 0;
        let mut errors = Vec::new();
        for (i, alert) in alerts.iter().take(MAX_MESSAGES).enumerate() {
            // A failed photo check costs the notes, never the message.
            let notes = if i < MAX_PHOTO_CHECKS { self.photo_notes(alert).await } else { None };
            let body = self.composio.request_body(&telegram::message(alert, notes.as_deref()));
            match post_json(telegram::EXECUTE_URL, &[("x-api-key", &self.composio.api_key)], &body).await {
                Ok((status, text)) => match telegram::delivered(status, &text) {
                    Ok(()) => sent += 1,
                    Err(e) => errors.push(e),
                },
                Err(e) => errors.push(e),
            }
        }
        // Some delivered: report the count so those get notified_at. None
        // delivered: an error, so the service logs why.
        if sent == 0 && !errors.is_empty() {
            return Err(errors.join("; "));
        }
        for e in &errors {
            console_error!("telegram: {e}");
        }
        Ok(sent)
    }
}

/// The notifier chosen at the composition root.
pub enum AnyNotifier {
    None(NoNotifier),
    Telegram(TelegramNotifier),
}

impl Notifier for AnyNotifier {
    async fn notify(&self, alerts: &[Alert]) -> Result<usize, String> {
        match self {
            AnyNotifier::None(n) => n.notify(alerts).await,
            AnyNotifier::Telegram(n) => n.notify(alerts).await,
        }
    }
}
