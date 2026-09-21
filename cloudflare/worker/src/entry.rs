//! Worker entry points: the HTTP router and the cron handler. Composition
//! root of the Worker: the only place `D1Store` is chosen and injected.
//!
//! Routes:
//!   GET  /api/health             liveness
//!   POST /api/listings           ingest (Authorization: Bearer <INGEST_TOKEN>)
//!   GET  /api/deals              ?make=&model=&seller=dealer|private&limit=&minComps=
//!   GET  /api/snapshots/latest   what the last cron run stored
//!   GET  /api/alerts             ?limit=  newest snipe alerts (new_deal_alerts)
//!
//! The read routes answer only through the Pages service binding
//! (host `carbuyer-api.internal`) unless the var PUBLIC_READ_API is "true".

use serde_json::{json, Value};
use worker::*;

use crate::alerts::{AlertRule, AlertsQuery, NoNotifier};
use crate::auth::{bearer_ok, read_allowed};
use crate::d1::D1Store;
use crate::deals::DealsQuery;
use crate::notify_http::{AnyNotifier, TelegramNotifier};
use crate::service::{self, ApiError, SnapshotStore};
use crate::telegram::ComposioConfig;
use crate::vision::VisionConfig;

fn now_iso() -> String {
    js_sys::Date::new_0().to_iso_string().as_string().unwrap_or_default()
}

fn json_response(status: u16, body: &Value) -> Result<Response> {
    let headers = Headers::new();
    headers.set("Content-Type", "application/json; charset=utf-8")?;
    headers.set("Cache-Control", "no-store")?;
    Ok(Response::from_json(body)?.with_status(status).with_headers(headers))
}

fn error_response(e: &ApiError) -> Result<Response> {
    if e.status >= 500 {
        console_error!("{}", e.message);
    }
    json_response(e.status, &json!({ "error": e.message }))
}

/// `DEFAULT_MIN_COMPS` from [vars], when set: used by the cron job and by
/// /api/deals calls that do not pass `minComps`.
fn default_min_comps(env: &Env) -> Option<f64> {
    env.var("DEFAULT_MIN_COMPS").ok().and_then(|v| v.to_string().trim().parse::<f64>().ok()).filter(|n| *n >= 1.0)
}

/// `PUBLIC_READ_API` from [vars] / .dev.vars: serve reads on the public host too.
fn public_read_api(env: &Env) -> bool {
    env.var("PUBLIC_READ_API").map(|v| v.to_string().trim().eq_ignore_ascii_case("true")).unwrap_or(false)
}

/// The snipe-alert rule from [vars] (ALERTS_ENABLED, ALERT_MIN_DISCOUNT_PCT,
/// ALERT_MIN_COMPS, ALERT_INCLUDE_DAMAGED). A bad value is logged and the
/// defaults are used, so a typo never stops ingest.
fn alert_rule(env: &Env) -> AlertRule {
    AlertRule::from_vars(|k| env.var(k).ok().map(|v| v.to_string())).unwrap_or_else(|e| {
        console_error!("alert settings ignored, using defaults: {e}");
        AlertRule::default()
    })
}

fn query_pairs(url: &Url) -> Vec<(String, String)> {
    url.query_pairs().map(|(k, v)| (k.into_owned(), v.into_owned())).collect()
}

fn store(env: &Env) -> Result<D1Store> {
    Ok(D1Store::new(env.d1("DB")?))
}

/// Telegram (through Composio) when COMPOSIO_API_KEY, COMPOSIO_USER_ID and
/// TELEGRAM_CHAT_ID are all set, with a Baseten photo check when
/// BASETEN_API_KEY is too. Otherwise alerts stay dashboard-only. Keys are
/// read as secrets first, then as vars, so either works in .dev.vars.
fn notifier(env: &Env) -> Result<AnyNotifier> {
    let get = |k: &str| {
        env.secret(k).ok().map(|v| v.to_string()).or_else(|| env.var(k).ok().map(|v| v.to_string()))
    };
    Ok(match ComposioConfig::from_vars(get) {
        Some(composio) => AnyNotifier::Telegram(TelegramNotifier {
            composio,
            vision: VisionConfig::from_vars(get),
            store: store(env)?,
        }),
        None => AnyNotifier::None(NoNotifier),
    })
}

#[event(fetch)]
async fn fetch(mut req: Request, env: Env, _ctx: Context) -> Result<Response> {
    let url = req.url()?;
    let path = url.path().trim_end_matches('/').to_string();
    let method = req.method();

    // Reads are for the dashboard (via the Pages service binding). On the
    // public hostname they look like any unknown path unless PUBLIC_READ_API.
    let is_read = matches!(path.as_str(), "/api/deals" | "/api/snapshots/latest" | "/api/alerts");
    if is_read && !read_allowed(url.host_str(), public_read_api(&env)) {
        return error_response(&ApiError { status: 404, message: "not found".into() });
    }

    match (method, path.as_str()) {
        (Method::Get, "/api/health") => json_response(200, &json!({ "ok": true, "time": now_iso() })),

        (Method::Post, "/api/listings") => {
            let token = env.secret("INGEST_TOKEN").map(|s| s.to_string()).unwrap_or_default();
            if token.is_empty() {
                return error_response(&ApiError { status: 503, message: "ingest is disabled: the INGEST_TOKEN secret is not set".into() });
            }
            if !bearer_ok(req.headers().get("Authorization")?.as_deref(), &token) {
                let r = error_response(&ApiError { status: 401, message: "missing or wrong bearer token".into() })?;
                r.headers().set("WWW-Authenticate", "Bearer")?;
                return Ok(r);
            }
            let body: Value = match req.json().await {
                Ok(v) => v,
                Err(e) => return error_response(&ApiError::bad_request(format!("body is not valid JSON: {e}"))),
            };
            // Composition root: the notifier is chosen here (see `notifier`).
            // Without the Composio settings, alerts are dashboard-only.
            match service::ingest_and_alert(&store(&env)?, &notifier(&env)?, &alert_rule(&env), &body, &now_iso()).await {
                Ok(out) => {
                    if let Some(e) = &out.alert_error {
                        console_error!("alerts: {e}");
                    }
                    if out.new_alerts > 0 {
                        console_log!("alerts: {} new deal(s)", out.new_alerts);
                    }
                    json_response(200, &serde_json::to_value(out)?)
                }
                Err(e) => error_response(&e),
            }
        }

        (Method::Get, "/api/deals") => {
            let pairs = query_pairs(&url);
            let mut q = match DealsQuery::from_pairs(pairs.iter().map(|(k, v)| (k.as_str(), v.as_str()))) {
                Ok(q) => q,
                Err(m) => return error_response(&ApiError::bad_request(m)),
            };
            if q.min_comps.is_none() {
                q.min_comps = default_min_comps(&env);
            }
            match service::deals(&store(&env)?, &q, &now_iso()).await {
                Ok(v) => json_response(200, &v),
                Err(e) => error_response(&e),
            }
        }

        (Method::Get, "/api/snapshots/latest") => match store(&env)?.latest_snapshot().await {
            Ok(Some(v)) => json_response(200, &v),
            Ok(None) => error_response(&ApiError { status: 404, message: "no snapshot yet: the cron job has not run".into() }),
            Err(m) => error_response(&ApiError::internal(m)),
        },

        (Method::Get, "/api/alerts") => {
            let pairs = query_pairs(&url);
            let q = match AlertsQuery::from_pairs(pairs.iter().map(|(k, v)| (k.as_str(), v.as_str()))) {
                Ok(q) => q,
                Err(m) => return error_response(&ApiError::bad_request(m)),
            };
            match service::recent_alerts(&store(&env)?, &q, &alert_rule(&env), &now_iso()).await {
                Ok(v) => json_response(200, &v),
                Err(e) => error_response(&e),
            }
        }

        (_, "/api/health" | "/api/listings" | "/api/deals" | "/api/snapshots/latest" | "/api/alerts") => {
            error_response(&ApiError { status: 405, message: "method not allowed".into() })
        }
        _ => error_response(&ApiError { status: 404, message: "not found".into() }),
    }
}

#[event(scheduled)]
async fn scheduled(event: ScheduledEvent, env: Env, _ctx: ScheduleContext) {
    let db = match store(&env) {
        Ok(db) => db,
        Err(e) => {
            console_error!("snapshot: no D1 binding: {e}");
            return;
        }
    };
    let cron = event.cron();
    match service::take_snapshot(&db, default_min_comps(&env), &now_iso(), Some(cron.as_str())).await {
        Ok(v) => console_log!(
            "snapshot stored: {} comps, {} scored, {} deals",
            v["comps"],
            v["scored"],
            v["deals"].as_array().map_or(0, Vec::len)
        ),
        Err(e) => console_error!("snapshot failed: {}", e.message),
    }
}
