//! Funnel operator (design §11.4, `spec/query.md` §4).
//!
//! Consumes rows ordered by `(person_key, ts, uuid)` and keeps
//! `O(steps × breakdown values of the current person)` state; nothing is
//! retained per person once the next person starts.

use crate::error::{ApiError, ApiResult};
use crate::intern::Interner;
use std::collections::HashMap;
use tdigest::TDigest;

const NONE: i64 = i64::MIN;
/// Durations kept verbatim before switching to a t-digest. While a step has
/// at most this many samples its median is exact.
const EXACT_TOTAL: usize = 4096;
const EXACT_GROUP: usize = 256;
/// Distinct breakdown values a single funnel may produce.
pub const MAX_BREAKDOWN_VALUES: usize = 50_000;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Order {
    Ordered,
    Strict,
    Any,
}

impl Order {
    pub fn parse(s: Option<&str>) -> ApiResult<Self> {
        match s {
            None | Some("ordered") => Ok(Order::Ordered),
            Some("strict") => Ok(Order::Strict),
            Some("any") => Ok(Order::Any),
            Some(other) => Err(ApiError::bad_request(format!("unknown funnel order {other:?}"))),
        }
    }
}

#[derive(Debug, Clone, Copy)]
pub struct FunnelSpec {
    pub steps: usize,
    pub window_ms: i64,
    pub order: Order,
    pub breakdown: bool,
}

/// Median accumulator: exact while small, t-digest afterwards.
#[derive(Debug, Clone)]
pub struct Durations {
    buf: Vec<f64>,
    digest: Option<TDigest>,
    cap: usize,
    pub n: u64,
    pub sum: f64,
}

impl Durations {
    fn new(cap: usize) -> Self {
        Self { buf: Vec::new(), digest: None, cap, n: 0, sum: 0.0 }
    }

    fn push(&mut self, seconds: f64) {
        self.n += 1;
        self.sum += seconds;
        self.buf.push(seconds);
        if self.buf.len() > self.cap {
            self.flush();
        }
    }

    fn flush(&mut self) {
        if self.buf.is_empty() {
            return;
        }
        let base = self.digest.take().unwrap_or_else(|| TDigest::new_with_size(200));
        self.digest = Some(base.merge_unsorted(std::mem::take(&mut self.buf)));
    }

    fn merge(&mut self, mut other: Durations) {
        self.n += other.n;
        self.sum += other.sum;
        self.buf.append(&mut other.buf);
        if let Some(d) = other.digest {
            self.digest = Some(match self.digest.take() {
                Some(mine) => TDigest::merge_digests(vec![mine, d]),
                None => d,
            });
        }
        if self.digest.is_some() || self.buf.len() > self.cap {
            self.flush();
        }
    }

    pub fn median(&self) -> Option<f64> {
        if self.n == 0 {
            return None;
        }
        match &self.digest {
            None => {
                let mut v = self.buf.clone();
                v.sort_by(f64::total_cmp);
                let m = v.len() / 2;
                Some(if v.len() % 2 == 1 { v[m] } else { (v[m - 1] + v[m]) / 2.0 })
            }
            Some(d) => {
                let d = if self.buf.is_empty() { d.clone() } else { d.clone().merge_unsorted(self.buf.clone()) };
                Some(d.estimate_quantile(0.5))
            }
        }
    }
}

#[derive(Debug, Clone)]
pub struct StepStat {
    /// People who reached at least this step.
    pub count: u64,
    /// Time from the previous step, taken when the step was first reached.
    pub durations: Durations,
}

#[derive(Debug, Clone)]
pub struct FunnelResult {
    pub total: Vec<StepStat>,
    /// Per breakdown value (`None` = the null bucket); empty without breakdown.
    pub groups: Vec<(Option<String>, Vec<StepStat>)>,
}

fn new_stats(steps: usize, cap: usize) -> Vec<StepStat> {
    (0..steps).map(|_| StepStat { count: 0, durations: Durations::new(cap) }).collect()
}

impl FunnelResult {
    pub fn empty(steps: usize) -> Self {
        Self { total: new_stats(steps, EXACT_TOTAL), groups: Vec::new() }
    }

    pub fn merge(&mut self, other: FunnelResult) {
        fn add(into: &mut [StepStat], from: Vec<StepStat>) {
            for (a, b) in into.iter_mut().zip(from) {
                a.count += b.count;
                a.durations.merge(b.durations);
            }
        }
        add(&mut self.total, other.total);
        let mut index: HashMap<Option<String>, usize> =
            self.groups.iter().enumerate().map(|(i, (k, _))| (k.clone(), i)).collect();
        for (key, stats) in other.groups {
            match index.get(&key) {
                Some(i) => add(&mut self.groups[*i].1, stats),
                None => {
                    index.insert(key.clone(), self.groups.len());
                    self.groups.push((key, stats));
                }
            }
        }
    }
}

/// Chain state of one person (optionally restricted to one breakdown value).
#[derive(Debug, Clone)]
struct Chain {
    /// Per step: latest start time among chains that reached it.
    start: Vec<i64>,
    /// Per step: time of the event that completed it for that chain.
    at: Vec<i64>,
    best: usize,
}

impl Chain {
    fn new(steps: usize) -> Self {
        Self { start: vec![NONE; steps], at: vec![NONE; steps], best: 0 }
    }
    fn reset(&mut self) {
        self.start.fill(NONE);
        self.at.fill(NONE);
        self.best = 0;
    }
}

/// `any` mode state for one breakdown value.
#[derive(Debug, Clone, Default)]
struct AnyValue {
    /// Candidate anchors `(position, ts)`, ascending.
    anchors: Vec<(u64, i64)>,
    best: usize,
    reached_at: Vec<i64>,
}

fn seconds(ms: i64) -> f64 {
    ms as f64 / 1000.0
}

pub struct FunnelAcc {
    spec: FunnelSpec,
    total: Vec<StepStat>,
    groups: Vec<Vec<StepStat>>,
    values: Interner,
    /// Bytes of group state created since the caller last asked.
    new_bytes: usize,

    person: String,
    has_person: bool,
    pos: u64,
    // ordered
    chain: Chain,
    value_chains: Vec<(u32, Chain)>,
    spare: Vec<Chain>,
    // strict
    prev_start: Vec<i64>,
    prev_bd: Vec<u32>,
    cur_start: Vec<i64>,
    cur_bd: Vec<u32>,
    prev_ts: i64,
    value_best: Vec<(u32, usize)>,
    // any
    last_ts: Vec<i64>,
    last_pos: Vec<u64>,
    reached_at: Vec<i64>,
    any_values: Vec<(u32, AnyValue)>,
}

impl FunnelAcc {
    pub fn new(spec: FunnelSpec) -> Self {
        let s = spec.steps;
        Self {
            spec,
            total: new_stats(s, EXACT_TOTAL),
            groups: Vec::new(),
            values: Interner::default(),
            new_bytes: 0,
            person: String::new(),
            has_person: false,
            pos: 0,
            chain: Chain::new(s),
            value_chains: Vec::new(),
            spare: Vec::new(),
            prev_start: vec![NONE; s],
            prev_bd: vec![0; s],
            cur_start: vec![NONE; s],
            cur_bd: vec![0; s],
            prev_ts: NONE,
            value_best: Vec::new(),
            last_ts: vec![NONE; s],
            last_pos: vec![0; s],
            reached_at: vec![NONE; s + 1],
            any_values: Vec::new(),
        }
    }

    /// Bytes of breakdown state allocated since the previous call (for the
    /// query's memory budget).
    pub fn take_new_bytes(&mut self) -> usize {
        std::mem::take(&mut self.new_bytes)
    }

    fn group_id(&mut self, value: Option<&str>) -> ApiResult<u32> {
        let (id, created) = self.values.intern(value);
        if created {
            if self.groups.len() >= MAX_BREAKDOWN_VALUES {
                return Err(ApiError::bad_request(format!(
                    "breakdown has more than {MAX_BREAKDOWN_VALUES} distinct values; narrow the query"
                )));
            }
            self.new_bytes += 160 + self.spec.steps * 96 + value.map_or(0, str::len) * 2;
            self.groups.push(new_stats(self.spec.steps, EXACT_GROUP));
        }
        Ok(id)
    }

    fn end_person(&mut self) {
        if !self.has_person {
            return;
        }
        match self.spec.order {
            Order::Ordered => {
                for k in 0..self.chain.best {
                    self.total[k].count += 1;
                }
                self.chain.reset();
                for (id, mut c) in self.value_chains.drain(..) {
                    for k in 0..c.best {
                        self.groups[id as usize][k].count += 1;
                    }
                    c.reset();
                    self.spare.push(c);
                }
            }
            Order::Strict => {
                for k in 0..self.chain.best {
                    self.total[k].count += 1;
                }
                self.chain.best = 0;
                for (id, best) in self.value_best.drain(..) {
                    for k in 0..best {
                        self.groups[id as usize][k].count += 1;
                    }
                }
                self.prev_start.fill(NONE);
                self.prev_ts = NONE;
            }
            Order::Any => {
                for k in 0..self.chain.best {
                    self.total[k].count += 1;
                }
                self.chain.best = 0;
                self.last_ts.fill(NONE);
                self.last_pos.fill(0);
                self.reached_at.fill(NONE);
                for (id, v) in self.any_values.drain(..) {
                    for k in 0..v.best {
                        self.groups[id as usize][k].count += 1;
                    }
                }
            }
        }
        self.pos = 0;
    }

    /// Feeds one row. `mask` bit `j` is set when the row satisfies step `j`;
    /// `breakdown` is the row's breakdown value (ignored without breakdown).
    pub fn push(&mut self, person: &str, ts: i64, mask: u32, breakdown: Option<&str>) -> ApiResult<()> {
        if !self.has_person || self.person != person {
            self.end_person();
            self.person.clear();
            self.person.push_str(person);
            self.has_person = true;
        }
        self.pos += 1;
        match self.spec.order {
            Order::Ordered => self.push_ordered(ts, mask, breakdown),
            Order::Strict => self.push_strict(ts, mask, breakdown),
            Order::Any => self.push_any(ts, mask, breakdown),
        }
    }

    fn push_ordered(&mut self, ts: i64, mask: u32, breakdown: Option<&str>) -> ApiResult<()> {
        if mask == 0 {
            return Ok(());
        }
        let (s, w) = (self.spec.steps, self.spec.window_ms);
        advance(&mut self.chain, &mut self.total, s, w, ts, mask, mask & 1 != 0);
        if !self.spec.breakdown {
            return Ok(());
        }
        let starts = mask & 1 != 0;
        let mut own: Option<u32> = None;
        if starts {
            let id = self.group_id(breakdown)?;
            own = Some(id);
            if !self.value_chains.iter().any(|(v, _)| *v == id) {
                let c = self.spare.pop().unwrap_or_else(|| Chain::new(s));
                self.value_chains.push((id, c));
            }
        }
        for (id, chain) in &mut self.value_chains {
            advance(chain, &mut self.groups[*id as usize], s, w, ts, mask, own == Some(*id));
        }
        Ok(())
    }

    fn push_strict(&mut self, ts: i64, mask: u32, breakdown: Option<&str>) -> ApiResult<()> {
        let (s, w) = (self.spec.steps, self.spec.window_ms);
        let bd = if self.spec.breakdown && mask & 1 != 0 { self.group_id(breakdown)? } else { 0 };
        self.cur_start.fill(NONE);
        for j in (1..s).rev() {
            if mask >> j & 1 == 0 {
                continue;
            }
            let start = self.prev_start[j - 1];
            if start == NONE || ts - start > w {
                continue;
            }
            self.cur_start[j] = start;
            self.cur_bd[j] = self.prev_bd[j - 1];
            let gap = seconds(ts - self.prev_ts);
            if self.chain.best < j + 1 {
                self.chain.best = j + 1;
                self.total[j].durations.push(gap);
            }
            if self.spec.breakdown {
                let id = self.cur_bd[j];
                if let Some((_, best)) = self.value_best.iter_mut().find(|(v, _)| *v == id) {
                    if *best < j + 1 {
                        *best = j + 1;
                        self.groups[id as usize][j].durations.push(gap);
                    }
                }
            }
        }
        if mask & 1 != 0 {
            self.cur_start[0] = ts;
            self.cur_bd[0] = bd;
            self.chain.best = self.chain.best.max(1);
            if self.spec.breakdown && !self.value_best.iter().any(|(v, _)| *v == bd) {
                self.value_best.push((bd, 1));
            }
        }
        std::mem::swap(&mut self.prev_start, &mut self.cur_start);
        std::mem::swap(&mut self.prev_bd, &mut self.cur_bd);
        self.prev_ts = ts;
        Ok(())
    }

    fn push_any(&mut self, ts: i64, mask: u32, breakdown: Option<&str>) -> ApiResult<()> {
        if mask == 0 {
            return Ok(());
        }
        let (s, w) = (self.spec.steps, self.spec.window_ms);
        for j in 0..s {
            if mask >> j & 1 != 0 {
                self.last_ts[j] = ts;
                self.last_pos[j] = self.pos;
            }
        }
        // Without breakdown the anchor is the earliest of the steps' most
        // recent occurrences that are still inside the window.
        let count = self.last_ts.iter().filter(|t| **t != NONE && ts - **t <= w).count();
        while self.chain.best < count {
            self.chain.best += 1;
            let k = self.chain.best;
            self.reached_at[k] = ts;
            if k >= 2 {
                self.total[k - 1].durations.push(seconds(ts - self.reached_at[k - 1]));
            }
        }
        if !self.spec.breakdown {
            return Ok(());
        }
        // With breakdown the value comes from the anchor event, so anchors are
        // tracked per value. An anchor is dropped once a later anchor of the
        // same value sees the same set of steps (it can only expire sooner).
        let id = self.group_id(breakdown)?;
        match self.any_values.iter_mut().find(|(v, _)| *v == id) {
            Some((_, v)) => v.anchors.push((self.pos, ts)),
            None => {
                let v = AnyValue { anchors: vec![(self.pos, ts)], best: 0, reached_at: vec![NONE; s + 1] };
                self.any_values.push((id, v));
            }
        }
        let last_pos = &self.last_pos;
        for (id, v) in &mut self.any_values {
            let expired = v.anchors.iter().take_while(|(_, t)| ts - *t > w).count();
            v.anchors.drain(..expired);
            let mut i = 0;
            while i + 1 < v.anchors.len() {
                let (lo, hi) = (v.anchors[i].0, v.anchors[i + 1].0);
                if last_pos.iter().any(|p| *p >= lo && *p < hi) {
                    i += 1;
                } else {
                    v.anchors.remove(i);
                }
            }
            let Some((front, _)) = v.anchors.first().copied() else { continue };
            let count = last_pos.iter().filter(|p| **p >= front).count();
            while v.best < count {
                v.best += 1;
                let k = v.best;
                v.reached_at[k] = ts;
                if k >= 2 {
                    self.groups[*id as usize][k - 1].durations.push(seconds(ts - v.reached_at[k - 1]));
                }
            }
        }
        Ok(())
    }

    pub fn finish(mut self) -> FunnelResult {
        self.end_person();
        FunnelResult { total: self.total, groups: self.values.into_names().into_iter().zip(self.groups).collect() }
    }
}

/// The state transition of design §11.4 for one row.
#[inline]
fn advance(chain: &mut Chain, stats: &mut [StepStat], steps: usize, window: i64, ts: i64, mask: u32, starts: bool) {
    // High to low so one row never counts for two consecutive steps.
    for j in (1..steps).rev() {
        if mask >> j & 1 == 0 {
            continue;
        }
        let start = chain.start[j - 1];
        if start == NONE || ts - start > window {
            continue;
        }
        if start > chain.start[j] {
            chain.start[j] = start;
            chain.at[j] = ts;
        }
        if chain.best < j + 1 {
            chain.best = j + 1;
            stats[j].durations.push(seconds(ts - chain.at[j - 1]));
        }
    }
    if starts {
        chain.start[0] = ts;
        chain.at[0] = ts;
        chain.best = chain.best.max(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(spec: FunnelSpec, rows: &[(&str, i64, u32, Option<&str>)]) -> FunnelResult {
        let mut acc = FunnelAcc::new(spec);
        for (p, ts, mask, bd) in rows {
            acc.push(p, *ts, *mask, *bd).unwrap();
        }
        acc.finish()
    }

    fn counts(stats: &[StepStat]) -> Vec<u64> {
        stats.iter().map(|s| s.count).collect()
    }

    fn spec(steps: usize, window_ms: i64, order: Order) -> FunnelSpec {
        FunnelSpec { steps, window_ms, order, breakdown: false }
    }

    #[test]
    fn ordered_basics_and_window_boundary() {
        let s = spec(3, 1000, Order::Ordered);
        // A: full funnel exactly on the boundary; B: one ms too late for step 3;
        // C: steps out of order; D: only step 1.
        let r = run(
            s,
            &[
                ("A", 0, 0b001, None),
                ("A", 500, 0b010, None),
                ("A", 1000, 0b100, None),
                ("B", 0, 0b001, None),
                ("B", 10, 0b010, None),
                ("B", 1001, 0b100, None),
                ("C", 0, 0b010, None),
                ("C", 5, 0b001, None),
                ("C", 6, 0b100, None),
                ("D", 0, 0b001, None),
            ],
        );
        assert_eq!(counts(&r.total), [4, 2, 1]);
        assert!((r.total[1].durations.median().unwrap() - 0.255).abs() < 1e-12);
        assert_eq!(r.total[2].durations.median(), Some(0.5));
        assert_eq!(r.total[0].durations.median(), None);
    }

    #[test]
    fn ordered_uses_the_latest_start_and_one_row_counts_once() {
        let s = spec(2, 1000, Order::Ordered);
        // An early start has expired, a later one makes it.
        let r = run(s, &[("A", 0, 0b01, None), ("A", 5000, 0b01, None), ("A", 5500, 0b10, None)]);
        assert_eq!(counts(&r.total), [1, 1]);
        assert_eq!(r.total[1].durations.median(), Some(0.5));
        // A single row matching both steps is not a conversion...
        let r = run(s, &[("A", 0, 0b11, None)]);
        assert_eq!(counts(&r.total), [1, 0]);
        // ...but two distinct rows in the same millisecond are.
        let r = run(s, &[("A", 7, 0b11, None), ("A", 7, 0b11, None)]);
        assert_eq!(counts(&r.total), [1, 1]);
        assert_eq!(r.total[1].durations.median(), Some(0.0));
    }

    #[test]
    fn repeated_step_events_keep_the_chain_alive() {
        // step2 at 900 belongs to the chain started at 0; a newer start at 950
        // has no step2 of its own before step3 arrives at 1900 (> 0+1000 but
        // the 950 chain never reached step 2) → step 3 is not reached.
        let s = spec(3, 1000, Order::Ordered);
        let rows = [("A", 0, 0b001, None), ("A", 900, 0b010, None), ("A", 950, 0b001, None), ("A", 1900, 0b100, None)];
        assert_eq!(counts(&run(s, &rows).total), [1, 1, 0]);
        // With a second step2 for the newer start it is.
        let rows = [
            ("A", 0, 0b001, None),
            ("A", 900, 0b010, None),
            ("A", 950, 0b001, None),
            ("A", 960, 0b010, None),
            ("A", 1900, 0b100, None),
        ];
        assert_eq!(counts(&run(s, &rows).total), [1, 1, 1]);
    }

    #[test]
    fn strict_requires_adjacent_rows() {
        let s = spec(3, 10_000, Order::Strict);
        let ok = [("A", 1, 0b001, None), ("A", 2, 0b010, None), ("A", 3, 0b100, None)];
        assert_eq!(counts(&run(s, &ok).total), [1, 1, 1]);
        // An unrelated event between step 2 and 3 breaks the chain.
        let broken = [("A", 1, 0b001, None), ("A", 2, 0b010, None), ("A", 3, 0, None), ("A", 4, 0b100, None)];
        assert_eq!(counts(&run(s, &broken).total), [1, 1, 0]);
        // Restarting: s1 s1 s2 s3 works off the second s1.
        let restart =
            [("A", 1, 0b001, None), ("A", 2, 0b001, None), ("A", 3, 0b010, None), ("A", 4, 0b100, None)];
        let r = run(s, &restart);
        assert_eq!(counts(&r.total), [1, 1, 1]);
        assert_eq!(r.total[2].durations.median(), Some(0.001));
        // Window still applies.
        let slow = [("A", 0, 0b001, None), ("A", 5, 0b010, None), ("A", 10_001, 0b100, None)];
        assert_eq!(counts(&run(s, &slow).total), [1, 1, 0]);
    }

    #[test]
    fn any_counts_distinct_steps_inside_a_window() {
        let s = spec(3, 100, Order::Any);
        // Steps in reverse order, all within 100 ms.
        let r = run(s, &[("A", 0, 0b100, None), ("A", 50, 0b010, None), ("A", 100, 0b001, None)]);
        assert_eq!(counts(&r.total), [1, 1, 1]);
        // The first event falls out of the window before the third arrives.
        let r = run(s, &[("A", 0, 0b100, None), ("A", 50, 0b010, None), ("A", 101, 0b001, None)]);
        assert_eq!(counts(&r.total), [1, 1, 0]);
        // Repeats of one step never count twice.
        let r = run(s, &[("A", 0, 0b001, None), ("A", 1, 0b001, None), ("A", 2, 0b001, None)]);
        assert_eq!(counts(&r.total), [1, 0, 0]);
        // One event satisfying two steps covers both.
        let r = run(s, &[("A", 0, 0b011, None)]);
        assert_eq!(counts(&r.total), [1, 1, 0]);
    }

    #[test]
    fn breakdown_takes_the_value_of_the_chain_start() {
        let s = FunnelSpec { steps: 2, window_ms: 1000, order: Order::Ordered, breakdown: true };
        let r = run(
            s,
            &[
                // A starts on ios, converts on web → counted under ios only.
                ("A", 0, 0b01, Some("ios")),
                ("A", 10, 0b10, Some("web")),
                // B starts on both; only the web start is inside the window.
                ("B", 0, 0b01, Some("ios")),
                ("B", 5000, 0b01, Some("web")),
                ("B", 5100, 0b10, None),
                // C has a null breakdown.
                ("C", 0, 0b01, None),
            ],
        );
        assert_eq!(counts(&r.total), [3, 2]);
        let by: HashMap<_, _> = r.groups.iter().map(|(k, v)| (k.clone(), counts(v))).collect();
        assert_eq!(by[&Some("ios".to_owned())], [2, 1]);
        assert_eq!(by[&Some("web".to_owned())], [1, 1]);
        assert_eq!(by[&None], [1, 0]);
    }

    #[test]
    fn any_breakdown_uses_the_anchor_value() {
        let s = FunnelSpec { steps: 2, window_ms: 100, order: Order::Any, breakdown: true };
        let r = run(
            s,
            &[
                // Anchor on "x" sees both steps; the later "y" event only itself.
                ("A", 0, 0b01, Some("x")),
                ("A", 50, 0b10, Some("y")),
                // Old "x" anchor expired; the newer "x" anchor still reaches 2.
                ("B", 0, 0b01, Some("x")),
                ("B", 500, 0b01, Some("x")),
                ("B", 550, 0b10, Some("z")),
            ],
        );
        assert_eq!(counts(&r.total), [2, 2]);
        let by: HashMap<_, _> = r.groups.iter().map(|(k, v)| (k.clone().unwrap(), counts(v))).collect();
        assert_eq!(by["x"], [2, 2]);
        assert_eq!(by["y"], [1, 0]);
        assert_eq!(by["z"], [1, 0]);
    }

    #[test]
    fn merge_adds_counts_and_keeps_small_medians_exact() {
        let s = spec(2, 1000, Order::Ordered);
        let mut a = run(s, &[("A", 0, 1, None), ("A", 100, 2, None)]);
        let b = run(s, &[("B", 0, 1, None), ("B", 300, 2, None), ("C", 0, 1, None)]);
        a.merge(b);
        assert_eq!(counts(&a.total), [3, 2]);
        assert_eq!(a.total[1].durations.median(), Some(0.2));
        assert_eq!(a.total[1].durations.n, 2);
    }

    #[test]
    fn large_duration_sets_fall_back_to_the_digest() {
        let mut d = Durations::new(64);
        for i in 0..10_000 {
            d.push(i as f64);
        }
        let m = d.median().unwrap();
        assert!((m - 5000.0).abs() < 100.0, "{m}");
        assert_eq!(d.n, 10_000);
    }
}
