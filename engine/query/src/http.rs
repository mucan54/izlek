//! HTTP surface (`spec/query.md`): authentication, routing and the JSON
//! error envelope.

use crate::error::{ApiError, ApiResult};
use crate::q_misc;
use crate::service::{Kind, Service};
use crate::state::Engine;
use axum::body::Bytes;
use axum::extract::{DefaultBodyLimit, Query, State};
use axum::http::header::{AUTHORIZATION, CONTENT_ENCODING, CONTENT_TYPE};
use axum::http::{HeaderMap, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::Router;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::sync::Arc;

pub const MAX_BODY_BYTES: usize = 1 << 20;
pub const H_INTERNAL: &str = "x-la-internal";
pub const H_PROJECT: &str = "x-la-project";
pub const H_REQUEST_ID: &str = "x-request-id";

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let status = StatusCode::from_u16(self.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
        json_response(status, &self.body())
    }
}

fn json_response(status: StatusCode, body: &Value) -> Response {
    let bytes = serde_json::to_vec(body).unwrap_or_else(|_| b"{}".to_vec());
    (status, [(CONTENT_TYPE, HeaderValue::from_static("application/json"))], bytes).into_response()
}

/// Compares secrets without an early exit on the first differing byte.
fn same_secret(a: &str, b: &str) -> bool {
    Sha256::digest(a.as_bytes()) == Sha256::digest(b.as_bytes())
}

/// Resolves the request's project (`spec/query.md` §1). The project comes
/// from the secret key or from the internal header pair — never from the
/// request body or query string.
pub fn authenticate(engine: &Engine, headers: &HeaderMap) -> ApiResult<u32> {
    let project = credentials(engine, headers)?;
    // A closed project's data is being deleted; never serve half of it.
    if engine.project_config(project).closed {
        return Err(ApiError::forbidden("project is closed"));
    }
    Ok(project)
}

fn credentials(engine: &Engine, headers: &HeaderMap) -> ApiResult<u32> {
    if let Some(presented) = headers.get(H_INTERNAL) {
        // Presenting the internal header commits the request to that path:
        // a wrong or unconfigured secret is never silently ignored.
        let Some(secret) = engine.cfg.internal_secret.as_deref().filter(|s| !s.is_empty()) else {
            return Err(ApiError::unauthorized("internal access is not enabled"));
        };
        let presented = presented.to_str().map_err(|_| ApiError::unauthorized("invalid internal credential"))?;
        if !same_secret(presented, secret) {
            return Err(ApiError::unauthorized("invalid internal credential"));
        }
        let project = headers
            .get(H_PROJECT)
            .and_then(|v| v.to_str().ok())
            .filter(|s| !s.is_empty() && s.len() <= 10 && s.bytes().all(|b| b.is_ascii_digit()) && !s.starts_with('0'))
            .and_then(|s| s.parse::<u32>().ok())
            .ok_or_else(|| ApiError::bad_request("X-LA-Project must be a project id"))?;
        return Ok(project);
    }
    if headers.contains_key(H_PROJECT) {
        return Err(ApiError::unauthorized("X-LA-Project requires the internal credential"));
    }
    let auth = headers
        .get(AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| ApiError::unauthorized("missing Authorization header"))?;
    let token = auth
        .strip_prefix("Bearer ")
        .or_else(|| auth.strip_prefix("bearer "))
        .map(str::trim)
        .ok_or_else(|| ApiError::unauthorized("expected a Bearer token"))?;
    let keys = engine.keys();
    let key = keys.lookup(token).ok_or_else(|| ApiError::unauthorized("unknown key"))?;
    if !key.scopes.iter().any(|s| s == "query") {
        return Err(ApiError::forbidden("key lacks the `query` scope"));
    }
    if key.project_id == 0 {
        return Err(ApiError::unauthorized("unknown key"));
    }
    Ok(key.project_id)
}

fn parse_body(body: &Bytes) -> ApiResult<Value> {
    if body.is_empty() {
        return Ok(Value::Object(Default::default()));
    }
    serde_json::from_slice(body).map_err(|e| ApiError::bad_request(format!("invalid JSON body: {e}")))
}

/// The caller's `X-Request-Id` if it is a plain token, otherwise a fresh one.
fn request_id(headers: &HeaderMap) -> String {
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
    let given = headers.get(H_REQUEST_ID).and_then(|v| v.to_str().ok()).filter(|s| {
        !s.is_empty()
            && s.len() <= 64
            && s.bytes().all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
    });
    match given {
        Some(s) => s.to_owned(),
        None => {
            let n = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let seed = format!("{n}-{:?}", std::time::SystemTime::now());
            Sha256::digest(seed.as_bytes()).iter().take(8).map(|b| format!("{b:02x}")).collect()
        }
    }
}

fn with_request_id(mut response: Response, id: &str) -> Response {
    if let Ok(v) = HeaderValue::from_str(id) {
        response.headers_mut().insert(H_REQUEST_ID, v);
    }
    response
}

async fn run_query(svc: Arc<Service>, kind: Kind, headers: HeaderMap, body: Bytes) -> Response {
    let id = request_id(&headers);
    let result = async {
        let project = authenticate(&svc.engine, &headers)?;
        let body = parse_body(&body)?;
        svc.query_traced(project, kind, &body, &id).await
    }
    .await;
    let response = match result {
        Ok(v) => json_response(StatusCode::OK, &v),
        Err(e) => {
            if e.status == 401 || e.status == 403 {
                svc.metrics.inc("auth_failures_total", &[], 1);
            }
            e.into_response()
        }
    };
    with_request_id(response, &id)
}

macro_rules! query_route {
    ($name:ident, $kind:expr) => {
        async fn $name(State(svc): State<Arc<Service>>, headers: HeaderMap, body: Bytes) -> Response {
            run_query(svc, $kind, headers, body).await
        }
    };
}

query_route!(trends, Kind::Trends);
query_route!(funnel, Kind::Funnel);
query_route!(retention, Kind::Retention);
query_route!(events, Kind::Events);
query_route!(sql, Kind::Sql);

async fn schema(State(svc): State<Arc<Service>>, headers: HeaderMap, body: Bytes) -> Response {
    let result = async {
        let project = authenticate(&svc.engine, &headers)?;
        // The body carries nothing; it only has to be an (empty) object if present.
        if !matches!(parse_body(&body)?, Value::Object(m) if m.is_empty()) {
            return Err(ApiError::bad_request("/q/schema takes an empty JSON object"));
        }
        let snap = svc.engine.snapshot(project, 0, 1, true).await?;
        Ok(q_misc::schema_json(&snap.schema, &snap))
    }
    .await;
    match result {
        Ok(v) => json_response(StatusCode::OK, &v),
        Err(e) => e.into_response(),
    }
}

async fn replay(
    State(svc): State<Arc<Service>>,
    headers: HeaderMap,
    Query(params): Query<HashMap<String, String>>,
) -> Response {
    let result = async {
        let project = authenticate(&svc.engine, &headers)?;
        let key = params.get("key").ok_or_else(|| ApiError::bad_request("missing `key`"))?;
        q_misc::replay_chunk(&svc.engine, project, key).await
    }
    .await;
    match result {
        Ok((bytes, encoding)) => (
            StatusCode::OK,
            [
                (CONTENT_TYPE, HeaderValue::from_static("application/octet-stream")),
                (CONTENT_ENCODING, HeaderValue::from_static(encoding.header())),
            ],
            bytes,
        )
            .into_response(),
        Err(e) => e.into_response(),
    }
}

async fn healthz() -> &'static str {
    "ok\n"
}

async fn readyz(State(svc): State<Arc<Service>>) -> Response {
    if svc.engine.ready() {
        (StatusCode::OK, "ready\n").into_response()
    } else {
        (StatusCode::SERVICE_UNAVAILABLE, "control files not loaded\n").into_response()
    }
}

async fn metrics(State(svc): State<Arc<Service>>) -> Response {
    (StatusCode::OK, [(CONTENT_TYPE, HeaderValue::from_static("text/plain; version=0.0.4"))], svc.render_metrics())
        .into_response()
}

async fn not_found() -> Response {
    ApiError::not_found("no such endpoint").into_response()
}

pub fn router(svc: Arc<Service>) -> Router {
    Router::new()
        .route("/q/trends", post(trends))
        .route("/q/funnel", post(funnel))
        .route("/q/retention", post(retention))
        .route("/q/events", post(events))
        .route("/q/sql", post(sql))
        .route("/q/schema", post(schema))
        .route("/q/replay", get(replay))
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        .route("/metrics", get(metrics))
        .fallback(not_found)
        .layer(DefaultBodyLimit::max(MAX_BODY_BYTES))
        .with_state(svc)
}
