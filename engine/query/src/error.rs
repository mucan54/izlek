//! API error type and the mapping from engine errors to HTTP status codes
//! (`spec/query.md` §1).

use datafusion::error::DataFusionError;
use std::fmt;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApiError {
    pub status: u16,
    pub code: &'static str,
    pub message: String,
}

pub type ApiResult<T> = Result<T, ApiError>;

impl ApiError {
    pub fn new(status: u16, code: &'static str, message: impl Into<String>) -> Self {
        Self { status, code, message: message.into() }
    }
    pub fn bad_request(message: impl Into<String>) -> Self {
        Self::new(400, "bad_request", message)
    }
    pub fn unauthorized(message: impl Into<String>) -> Self {
        Self::new(401, "unauthorized", message)
    }
    pub fn forbidden(message: impl Into<String>) -> Self {
        Self::new(403, "forbidden", message)
    }
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(404, "not_found", message)
    }
    pub fn timeout() -> Self {
        Self::new(408, "timeout", "query exceeded the time limit")
    }
    pub fn scan_limit(limit: u64) -> Self {
        Self::new(422, "scan_limit", format!("query would scan more than the allowed {limit} bytes"))
    }
    pub fn memory_limit() -> Self {
        Self::new(422, "memory_limit", "query exceeded its memory budget")
    }
    pub fn queue_full() -> Self {
        Self::new(429, "queue_full", "too many concurrent queries for this project")
    }
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(500, "internal", message)
    }

    pub fn body(&self) -> serde_json::Value {
        serde_json::json!({ "error": { "code": self.code, "message": self.message } })
    }
}

impl fmt::Display for ApiError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} {}: {}", self.status, self.code, self.message)
    }
}

impl std::error::Error for ApiError {}

/// Raised by the file reader when a query crosses its scanned-bytes budget.
#[derive(Debug, Clone, Copy)]
pub struct ScanLimitExceeded(pub u64);

impl fmt::Display for ScanLimitExceeded {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "scan limit of {} bytes exceeded", self.0)
    }
}

impl std::error::Error for ScanLimitExceeded {}

fn find_scan_limit(e: &(dyn std::error::Error + 'static)) -> Option<u64> {
    let mut cur: Option<&(dyn std::error::Error + 'static)> = Some(e);
    let mut depth = 0;
    while let Some(err) = cur {
        if let Some(s) = err.downcast_ref::<ScanLimitExceeded>() {
            return Some(s.0);
        }
        // DataFusion and parquet keep boxed causes outside `source()`.
        if let Some(df) = err.downcast_ref::<DataFusionError>() {
            match df {
                DataFusionError::External(inner) => return find_scan_limit(inner.as_ref()),
                DataFusionError::ParquetError(p) => return find_scan_limit(p.as_ref()),
                DataFusionError::ArrowError(a, _) => return find_scan_limit(a.as_ref()),
                DataFusionError::Context(_, inner) => return find_scan_limit(inner.as_ref()),
                DataFusionError::Shared(inner) => return find_scan_limit(inner.as_ref()),
                _ => {}
            }
        }
        if let Some(parquet::errors::ParquetError::External(inner)) = err.downcast_ref() {
            return find_scan_limit(inner.as_ref());
        }
        if let Some(arrow::error::ArrowError::ExternalError(inner)) = err.downcast_ref() {
            return find_scan_limit(inner.as_ref());
        }
        // The limit can also surface as text once an error crossed a task boundary.
        depth += 1;
        if depth > 16 {
            break;
        }
        cur = err.source();
    }
    None
}

impl From<DataFusionError> for ApiError {
    fn from(e: DataFusionError) -> Self {
        if let Some(limit) = find_scan_limit(&e) {
            return ApiError::scan_limit(limit);
        }
        let text = e.to_string();
        if let Some(i) = text.find("scan limit of ") {
            let digits: String = text[i + 14..].chars().take_while(char::is_ascii_digit).collect();
            return ApiError::scan_limit(digits.parse().unwrap_or(0));
        }
        match e.find_root() {
            DataFusionError::ResourcesExhausted(_) => ApiError::memory_limit(),
            DataFusionError::ObjectStore(_)
            | DataFusionError::IoError(_)
            | DataFusionError::Internal(_)
            | DataFusionError::External(_)
            | DataFusionError::ParquetError(_)
            | DataFusionError::ExecutionJoin(_) => {
                tracing::error!(error = %text, "query failed");
                ApiError::internal("query execution failed")
            }
            _ => ApiError::bad_request(strip_backtrace(&text)),
        }
    }
}

fn strip_backtrace(s: &str) -> String {
    let s = s.split("\n\nbacktrace:").next().unwrap_or(s);
    let mut out: String = s.chars().take(600).collect();
    if out.len() < s.len() {
        out.push('…');
    }
    out
}

impl From<la_core::Error> for ApiError {
    fn from(e: la_core::Error) -> Self {
        tracing::error!(error = %e, "storage error");
        ApiError::internal("storage error")
    }
}

impl From<arrow::error::ArrowError> for ApiError {
    fn from(e: arrow::error::ArrowError) -> Self {
        ApiError::from(DataFusionError::from(e))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scan_limit_is_found_through_wrappers() {
        let p = parquet::errors::ParquetError::External(Box::new(ScanLimitExceeded(42)));
        let e = DataFusionError::Context("x".into(), Box::new(DataFusionError::ParquetError(Box::new(p))));
        let api = ApiError::from(e);
        assert_eq!((api.status, api.code), (422, "scan_limit"));
        let api = ApiError::from(DataFusionError::Execution("task failed: scan limit of 77 bytes exceeded".into()));
        assert_eq!(api.status, 422);
        assert!(api.message.contains("77"));
    }

    #[test]
    fn classification() {
        assert_eq!(ApiError::from(DataFusionError::Plan("no such column".into())).status, 400);
        assert_eq!(ApiError::from(DataFusionError::ResourcesExhausted("pool".into())).status, 422);
        assert_eq!(ApiError::from(DataFusionError::Internal("bug".into())).status, 500);
        assert_eq!(ApiError::bad_request("x").body()["error"]["code"], "bad_request");
    }
}
