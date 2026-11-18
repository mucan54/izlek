//! Request-body helpers shared by the endpoints: strict JSON object access
//! (unknown keys are rejected) and UTC time buckets.

use crate::error::{ApiError, ApiResult};
use crate::filter::{self, Filter, Ref};
use chrono::{Datelike, TimeZone, Utc};
use la_core::time::DAY_MS;
use serde_json::{Map, Value};

/// A JSON object whose every key must be consumed.
pub struct Obj<'a> {
    map: &'a Map<String, Value>,
    seen: Vec<&'static str>,
    what: &'static str,
}

impl<'a> Obj<'a> {
    pub fn new(v: &'a Value, what: &'static str) -> ApiResult<Self> {
        match v {
            Value::Object(map) => Ok(Self { map, seen: Vec::new(), what }),
            _ => Err(ApiError::bad_request(format!("{what} must be a JSON object"))),
        }
    }

    pub fn get(&mut self, key: &'static str) -> Option<&'a Value> {
        self.seen.push(key);
        self.map.get(key).filter(|v| !v.is_null())
    }

    pub fn str(&mut self, key: &'static str) -> ApiResult<Option<&'a str>> {
        match self.get(key) {
            None => Ok(None),
            Some(Value::String(s)) => Ok(Some(s)),
            Some(_) => Err(ApiError::bad_request(format!("`{key}` must be a string"))),
        }
    }

    pub fn req_str(&mut self, key: &'static str) -> ApiResult<&'a str> {
        self.str(key)?.ok_or_else(|| ApiError::bad_request(format!("{} needs `{key}`", self.what)))
    }

    pub fn u64(&mut self, key: &'static str) -> ApiResult<Option<u64>> {
        match self.get(key) {
            None => Ok(None),
            Some(v) => v
                .as_u64()
                .map(Some)
                .ok_or_else(|| ApiError::bad_request(format!("`{key}` must be a non-negative integer"))),
        }
    }

    pub fn array(&mut self, key: &'static str) -> ApiResult<Option<&'a Vec<Value>>> {
        match self.get(key) {
            None => Ok(None),
            Some(Value::Array(a)) => Ok(Some(a)),
            Some(_) => Err(ApiError::bad_request(format!("`{key}` must be an array"))),
        }
    }

    pub fn filter(&mut self, key: &'static str) -> ApiResult<Option<Filter>> {
        filter::parse_optional(self.get(key))
    }

    pub fn reference(&mut self, key: &'static str) -> ApiResult<Option<Ref>> {
        self.str(key)?.map(Ref::parse).transpose()
    }

    /// `from` / `to` as `[from_ms, to_ms)`.
    pub fn range(&mut self) -> ApiResult<(i64, i64)> {
        let from = self.req_str("from")?;
        let to = self.req_str("to")?;
        crate::state::parse_range(from, to)
    }

    /// Fails on keys nobody asked for (typos must not silently change a query,
    /// and no body field may ever select a project).
    pub fn finish(self) -> ApiResult<()> {
        match self.map.keys().find(|k| !self.seen.contains(&k.as_str())) {
            None => Ok(()),
            Some(k) => Err(ApiError::bad_request(format!("unknown field {k:?} in {}", self.what))),
        }
    }
}

/// `{ "event": name|null, "filter": Expr }` as used by steps and cohorts.
#[derive(Debug, Clone)]
pub struct EventSel {
    pub event: Option<String>,
    pub filter: Option<Filter>,
}

impl EventSel {
    pub fn parse(v: &Value, what: &'static str) -> ApiResult<Self> {
        let mut o = Obj::new(v, what)?;
        let sel = Self { event: o.str("event")?.map(str::to_owned), filter: o.filter("filter")? };
        o.finish()?;
        Ok(sel)
    }

    pub fn uses_person(&self) -> bool {
        self.filter.as_ref().is_some_and(Filter::uses_person)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Interval {
    Hour,
    Day,
    Week,
    Month,
}

impl Interval {
    pub fn parse(s: &str, allow_hour: bool) -> ApiResult<Self> {
        match s {
            "hour" if allow_hour => Ok(Interval::Hour),
            "day" => Ok(Interval::Day),
            "week" => Ok(Interval::Week),
            "month" => Ok(Interval::Month),
            other => Err(ApiError::bad_request(format!("unsupported interval {other:?}"))),
        }
    }

    /// Start of the bucket containing `ts` (UTC; weeks start on Monday).
    pub fn floor(self, ts: i64) -> i64 {
        match self {
            Interval::Hour => ts.div_euclid(3_600_000) * 3_600_000,
            Interval::Day => ts.div_euclid(DAY_MS) * DAY_MS,
            // 1970-01-01 was a Thursday: day 4 is the first Monday.
            Interval::Week => ((ts.div_euclid(DAY_MS) + 3).div_euclid(7) * 7 - 3) * DAY_MS,
            Interval::Month => {
                let d = Utc.timestamp_millis_opt(ts).single().unwrap_or_default();
                Utc.with_ymd_and_hms(d.year(), d.month(), 1, 0, 0, 0).single().map(|x| x.timestamp_millis()).unwrap_or(ts)
            }
        }
    }

    /// Start of the bucket after the one starting at `start`.
    pub fn next(self, start: i64) -> i64 {
        match self {
            Interval::Hour => start + 3_600_000,
            Interval::Day => start + DAY_MS,
            Interval::Week => start + 7 * DAY_MS,
            Interval::Month => {
                let d = Utc.timestamp_millis_opt(start).single().unwrap_or_default();
                let (y, m) = if d.month() == 12 { (d.year() + 1, 1) } else { (d.year(), d.month() + 1) };
                Utc.with_ymd_and_hms(y, m, 1, 0, 0, 0).single().map(|x| x.timestamp_millis()).unwrap_or(start + 31 * DAY_MS)
            }
        }
    }
}

/// Bucket start instants covering `[from_ms, to_ms)`.
pub fn buckets(from_ms: i64, to_ms: i64, interval: Interval) -> Vec<i64> {
    let mut out = Vec::new();
    let mut b = interval.floor(from_ms);
    while b < to_ms {
        out.push(b);
        b = interval.next(b);
    }
    out
}

/// `2026-11-01T00:00:00Z`
pub fn iso(ms: i64) -> String {
    Utc.timestamp_millis_opt(ms).single().unwrap_or_default().format("%Y-%m-%dT%H:%M:%SZ").to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::time::day_start_ms;
    use serde_json::json;

    #[test]
    fn strict_objects() {
        let v = json!({"from": "2026-11-01", "to": "2026-11-02", "limit": 5, "x": null});
        let mut o = Obj::new(&v, "request").unwrap();
        assert_eq!(o.range().unwrap().1 - o.range().unwrap().0, 2 * DAY_MS);
        assert_eq!(o.u64("limit").unwrap(), Some(5));
        assert!(o.finish().is_err(), "x was never read");
        let mut o = Obj::new(&v, "request").unwrap();
        o.range().unwrap();
        o.u64("limit").unwrap();
        assert_eq!(o.str("x").unwrap(), None, "null is absent");
        o.finish().unwrap();
        assert!(Obj::new(&json!([]), "request").is_err());
        let bad = json!({"limit": -1});
        assert!(Obj::new(&bad, "request").unwrap().u64("limit").is_err());
    }

    #[test]
    fn bucket_boundaries() {
        let d = |s: &str| day_start_ms(s).unwrap();
        // 2026-11-01 is a Sunday; its week starts on Monday 2026-10-26.
        assert_eq!(Interval::Week.floor(d("2026-11-01") + 5), d("2026-10-26"));
        assert_eq!(Interval::Week.floor(d("2026-11-02")), d("2026-11-02"));
        assert_eq!(Interval::Week.floor(d("2026-11-02") - 1), d("2026-10-26"));
        assert_eq!(Interval::Month.floor(d("2026-11-18") + 77), d("2026-11-01"));
        assert_eq!(Interval::Month.next(d("2026-12-01")), d("2027-01-01"));
        assert_eq!(Interval::Hour.floor(d("2026-11-01") + 3_600_001), d("2026-11-01") + 3_600_000);
        assert_eq!(Interval::Day.floor(-1), -DAY_MS);
        let b = buckets(d("2026-11-01") + 1, d("2026-11-03"), Interval::Day);
        assert_eq!(b, [d("2026-11-01"), d("2026-11-02")]);
        let b = buckets(d("2026-10-30"), d("2026-12-02"), Interval::Month);
        assert_eq!(b, [d("2026-10-01"), d("2026-11-01"), d("2026-12-01")]);
        let b = buckets(d("2026-11-01"), d("2026-11-10"), Interval::Week);
        assert_eq!(b, [d("2026-10-26"), d("2026-11-02"), d("2026-11-09")]);
        assert_eq!(iso(d("2026-11-02")), "2026-11-02T00:00:00Z");
    }
}
