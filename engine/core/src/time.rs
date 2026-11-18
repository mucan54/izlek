//! UTC day helpers. Days are always `YYYY-MM-DD` strings in keys and manifests.

use chrono::{DateTime, Duration, NaiveDate, TimeZone, Utc};

pub const DAY_MS: i64 = 86_400_000;

pub fn now_ms() -> i64 {
    Utc::now().timestamp_millis()
}

/// UTC day of a unix-ms timestamp.
pub fn day_of(ts_ms: i64) -> String {
    match Utc.timestamp_millis_opt(ts_ms).single() {
        Some(d) => d.format("%Y-%m-%d").to_string(),
        None => "1970-01-01".to_owned(),
    }
}

pub fn parse_day(day: &str) -> Option<NaiveDate> {
    if day.len() != 10 {
        return None;
    }
    NaiveDate::parse_from_str(day, "%Y-%m-%d").ok()
}

/// Unix ms of `day` 00:00:00 UTC.
pub fn day_start_ms(day: &str) -> Option<i64> {
    let d = parse_day(day)?;
    Some(Utc.from_utc_datetime(&d.and_hms_opt(0, 0, 0)?).timestamp_millis())
}

/// Inclusive list of days `from..=to`; empty when `from > to`.
pub fn days_between(from: &str, to: &str) -> Vec<String> {
    let (Some(a), Some(b)) = (parse_day(from), parse_day(to)) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    let mut d = a;
    while d <= b {
        out.push(d.format("%Y-%m-%d").to_string());
        d += Duration::days(1);
    }
    out
}

/// `(YYYY-MM-DD, HH)` of a unix-ms timestamp, as used in `wal/dt=…/hh=…/`.
pub fn dt_hh(ts_ms: i64) -> (String, String) {
    let d: DateTime<Utc> = Utc.timestamp_millis_opt(ts_ms).single().unwrap_or_default();
    (d.format("%Y-%m-%d").to_string(), d.format("%H").to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn day_roundtrip() {
        let ms = day_start_ms("2026-11-18").unwrap();
        assert_eq!(ms % DAY_MS, 0);
        assert_eq!(day_of(ms), "2026-11-18");
        assert_eq!(day_of(ms + DAY_MS - 1), "2026-11-18");
        assert_eq!(day_of(ms - 1), "2026-11-17");
        assert_eq!(day_of(i64::MAX), "1970-01-01");
        assert_eq!(dt_hh(ms + 21 * 3_600_000 + 5), ("2026-11-18".into(), "21".into()));
    }

    #[test]
    fn ranges() {
        assert_eq!(days_between("2026-02-27", "2026-03-01"), ["2026-02-27", "2026-02-28", "2026-03-01"]);
        assert!(days_between("2026-03-02", "2026-03-01").is_empty());
        assert!(parse_day("2026-3-1").is_none());
        assert!(parse_day("../../etc").is_none());
    }
}
