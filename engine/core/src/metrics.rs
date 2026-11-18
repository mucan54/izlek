//! Minimal Prometheus text-format registry shared by the engine binaries.
//! No dependency, no labels-as-types machinery: a metric is a name plus an
//! optional fixed label string.

use std::collections::BTreeMap;
use std::fmt::Write;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Mutex, OnceLock};

#[derive(Debug, Default)]
pub struct Counter(AtomicU64);

impl Counter {
    pub const fn new() -> Self {
        Self(AtomicU64::new(0))
    }
    pub fn inc(&self) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
    pub fn add(&self, n: u64) {
        self.0.fetch_add(n, Ordering::Relaxed);
    }
    pub fn get(&self) -> u64 {
        self.0.load(Ordering::Relaxed)
    }
}

#[derive(Debug, Default)]
pub struct Gauge(AtomicI64);

impl Gauge {
    pub const fn new() -> Self {
        Self(AtomicI64::new(0))
    }
    pub fn set(&self, v: i64) {
        self.0.store(v, Ordering::Relaxed);
    }
    pub fn add(&self, v: i64) {
        self.0.fetch_add(v, Ordering::Relaxed);
    }
    pub fn get(&self) -> i64 {
        self.0.load(Ordering::Relaxed)
    }
}

/// Incremented by [`crate::manifest::commit`] on every lost race.
pub static MANIFEST_CONFLICTS: Counter = Counter::new();

const BUCKETS: [f64; 14] = [0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0];

#[derive(Debug, Default, Clone)]
struct Hist {
    buckets: [u64; BUCKETS.len()],
    count: u64,
    sum: f64,
}

/// Dynamic metrics keyed by `name{labels}`; static hot-path counters should be
/// plain [`Counter`] statics registered once via [`Registry::register_counter`].
#[derive(Default)]
pub struct Registry {
    prefix: String,
    counters: Mutex<BTreeMap<String, u64>>,
    gauges: Mutex<BTreeMap<String, f64>>,
    hists: Mutex<BTreeMap<String, Hist>>,
    statics: Mutex<Vec<(String, &'static Counter)>>,
    static_gauges: Mutex<Vec<(String, &'static Gauge)>>,
}

fn series(name: &str, labels: &[(&str, &str)]) -> String {
    if labels.is_empty() {
        return name.to_owned();
    }
    let mut s = String::with_capacity(name.len() + 32);
    s.push_str(name);
    s.push('{');
    for (i, (k, v)) in labels.iter().enumerate() {
        if i > 0 {
            s.push(',');
        }
        let _ = write!(s, "{k}=\"{}\"", v.replace('\\', "\\\\").replace('"', "\\\"").replace('\n', "\\n"));
    }
    s.push('}');
    s
}

impl Registry {
    /// `prefix` is prepended to every metric name, e.g. `la_shipper_`.
    pub fn new(prefix: &str) -> Self {
        Self { prefix: prefix.to_owned(), ..Default::default() }
    }

    pub fn inc(&self, name: &str, labels: &[(&str, &str)], n: u64) {
        *self.counters.lock().unwrap().entry(series(name, labels)).or_default() += n;
    }

    pub fn set(&self, name: &str, labels: &[(&str, &str)], v: f64) {
        self.gauges.lock().unwrap().insert(series(name, labels), v);
    }

    pub fn observe(&self, name: &str, labels: &[(&str, &str)], seconds: f64) {
        let mut h = self.hists.lock().unwrap();
        let e = h.entry(series(name, labels)).or_default();
        for (i, b) in BUCKETS.iter().enumerate() {
            if seconds <= *b {
                e.buckets[i] += 1;
            }
        }
        e.count += 1;
        e.sum += seconds;
    }

    pub fn register_counter(&self, name: &str, c: &'static Counter) {
        self.statics.lock().unwrap().push((name.to_owned(), c));
    }

    pub fn register_gauge(&self, name: &str, g: &'static Gauge) {
        self.static_gauges.lock().unwrap().push((name.to_owned(), g));
    }

    pub fn counter_value(&self, name: &str, labels: &[(&str, &str)]) -> u64 {
        self.counters.lock().unwrap().get(&series(name, labels)).copied().unwrap_or(0)
    }

    /// Prometheus text exposition format.
    pub fn render(&self) -> String {
        let p = &self.prefix;
        let mut out = String::new();
        for (name, c) in self.statics.lock().unwrap().iter() {
            let _ = writeln!(out, "{p}{name} {}", c.get());
        }
        for (name, g) in self.static_gauges.lock().unwrap().iter() {
            let _ = writeln!(out, "{p}{name} {}", g.get());
        }
        for (k, v) in self.counters.lock().unwrap().iter() {
            let _ = writeln!(out, "{p}{k} {v}");
        }
        for (k, v) in self.gauges.lock().unwrap().iter() {
            let _ = writeln!(out, "{p}{k} {v}");
        }
        for (k, h) in self.hists.lock().unwrap().iter() {
            let (name, labels) = match k.split_once('{') {
                Some((n, l)) => (n, format!("{},", l.trim_end_matches('}'))),
                None => (k.as_str(), String::new()),
            };
            for (i, b) in BUCKETS.iter().enumerate() {
                let _ = writeln!(out, "{p}{name}_bucket{{{labels}le=\"{b}\"}} {}", h.buckets[i]);
            }
            let _ = writeln!(out, "{p}{name}_bucket{{{labels}le=\"+Inf\"}} {}", h.count);
            let trimmed = labels.trim_end_matches(',');
            let suffix = if trimmed.is_empty() { String::new() } else { format!("{{{trimmed}}}") };
            let _ = writeln!(out, "{p}{name}_sum{suffix} {}", h.sum);
            let _ = writeln!(out, "{p}{name}_count{suffix} {}", h.count);
        }
        out
    }
}

/// Process-wide registry for binaries that want one.
pub fn global(prefix: &str) -> &'static Registry {
    static R: OnceLock<Registry> = OnceLock::new();
    R.get_or_init(|| {
        let r = Registry::new(prefix);
        r.register_counter("manifest_conflicts_total", &MANIFEST_CONFLICTS);
        r
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn renders_text_format() {
        let r = Registry::new("la_test_");
        r.inc("records_total", &[("reason", "bot")], 2);
        r.inc("records_total", &[("reason", "bot")], 1);
        r.inc("plain_total", &[], 5);
        r.set("lag_seconds", &[], 1.5);
        r.observe("query_seconds", &[("type", "funnel")], 0.02);
        r.observe("query_seconds", &[("type", "funnel")], 7.0);
        let text = r.render();
        assert!(text.contains("la_test_records_total{reason=\"bot\"} 3\n"), "{text}");
        assert!(text.contains("la_test_plain_total 5\n"));
        assert!(text.contains("la_test_lag_seconds 1.5\n"));
        assert!(text.contains("la_test_query_seconds_bucket{type=\"funnel\",le=\"0.025\"} 1\n"), "{text}");
        assert!(text.contains("la_test_query_seconds_bucket{type=\"funnel\",le=\"+Inf\"} 2\n"));
        assert!(text.contains("la_test_query_seconds_count{type=\"funnel\"} 2\n"));
        assert_eq!(r.counter_value("records_total", &[("reason", "bot")]), 3);
    }
}
