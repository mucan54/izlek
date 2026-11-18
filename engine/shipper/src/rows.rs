//! Decoded events → `events` rows (`spec/parquet.md` §2 steps 4–6) and the
//! schema statistics that drive promotion (`spec/manifest.md` §5).

use crate::decode::RawEvent;
use crate::props::{find, to_props_json, PropVal};
use la_core::events::{Cell, EventRow};
use la_core::identity::IdentityMap;
use la_core::schema::{DayStats, PromotedColumn, PropType, STATS_MAX_KEYS_PER_DAY};
use la_core::time::day_of;
use std::collections::BTreeMap;

/// Keys tracked per day while a plan is being accumulated; the stored stats
/// are trimmed to `STATS_MAX_KEYS_PER_DAY` afterwards.
const STATS_KEY_BUDGET: usize = 4 * STATS_MAX_KEYS_PER_DAY;

/// The cell a value contributes to a column of type `ty`, if it fits
/// (`int` widens into `float`; nothing else converts).
fn cell_for(value: &PropVal, ty: PropType) -> Option<Cell> {
    match (ty, value) {
        (PropType::String, PropVal::Str(s)) => Some(Cell::Str(s.clone())),
        (PropType::Int, PropVal::Int(i)) => Some(Cell::Int(*i)),
        (PropType::Float, PropVal::Float(f)) => Some(Cell::Float(*f)),
        (PropType::Float, PropVal::Int(i)) => Some(Cell::Float(*i as f64)),
        (PropType::Bool, PropVal::Bool(b)) => Some(Cell::Bool(*b)),
        _ => None,
    }
}

/// Stage-1 identity resolution plus the split between promoted columns and
/// the `props` JSON.
pub fn build_row(mut ev: RawEvent, identity: &IdentityMap, promoted: &[PromotedColumn]) -> EventRow {
    let person_key = identity.person_key(ev.user_id.as_deref(), &ev.anon_id);
    let mut cells: Vec<Option<Cell>> = Vec::with_capacity(promoted.len());
    if ev.synthetic {
        cells.resize(promoted.len(), None);
    } else {
        for c in promoted {
            let cell = find(&ev.props, &c.key).and_then(|i| {
                let cell = cell_for(&ev.props[i].1, c.ty)?;
                ev.props.remove(i);
                Some(cell)
            });
            cells.push(cell);
        }
    }
    EventRow {
        uuid: ev.uuid,
        ts: ev.ts,
        recv_ts: ev.recv_ts,
        event: ev.name,
        person_key,
        anon_id: ev.anon_id,
        user_id: ev.user_id,
        session_id: ev.session_id,
        dims: ev.dims,
        flags: ev.flags,
        promoted: cells,
        props: to_props_json(&ev.props),
    }
}

/// Per-day key/type/name counters of the events of one plan.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct PlanStats {
    pub days: BTreeMap<String, DayStats>,
}

impl PlanStats {
    pub fn observe(&mut self, ev: &RawEvent) {
        if ev.synthetic {
            return;
        }
        let day = self.days.entry(day_of(ev.ts)).or_default();
        day.events += 1;
        if day.names.len() < STATS_KEY_BUDGET || day.names.contains_key(&ev.name) {
            *day.names.entry(ev.name.clone()).or_default() += 1;
        }
        for (key, value) in &ev.props {
            if day.keys.len() >= STATS_KEY_BUDGET && !day.keys.contains_key(key) {
                continue;
            }
            let stat = day.keys.entry(key.clone()).or_default();
            stat.n += 1;
            *stat.types.entry(value.type_name().to_owned()).or_default() += 1;
        }
    }

    /// Adds these counters to a stored stats window.
    pub fn add_to(&self, stats: &mut la_core::schema::SchemaStats) {
        for (day, add) in &self.days {
            let d = stats.days.entry(day.clone()).or_default();
            d.events += add.events;
            for (k, s) in &add.keys {
                let e = d.keys.entry(k.clone()).or_default();
                e.n += s.n;
                for (t, n) in &s.types {
                    *e.types.entry(t.clone()).or_default() += n;
                }
            }
            for (name, n) in &add.names {
                *d.names.entry(name.clone()).or_default() += n;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::props::sort_last_wins;

    fn ev(props: Vec<(&str, PropVal)>) -> RawEvent {
        RawEvent {
            uuid: [1; 16],
            ts: 1_795_046_400_000 - 3_600_000, // 2026-11-18T23:00:00Z
            recv_ts: 1_795_046_400_000,
            name: "purchase".into(),
            anon_id: "dev".into(),
            user_id: None,
            session_id: None,
            dims: Default::default(),
            flags: 0,
            props: sort_last_wins(props.into_iter().map(|(k, v)| (k.to_owned(), v)).collect()),
            synthetic: false,
        }
    }

    fn col(key: &str, ty: PropType) -> PromotedColumn {
        PromotedColumn { key: key.into(), ty, since: 1 }
    }

    #[test]
    fn promoted_cells_are_type_checked() {
        let cols = [
            col("plan", PropType::String),
            col("amount", PropType::Float),
            col("n", PropType::Int),
            col("ok", PropType::Bool),
            col("absent", PropType::String),
        ];
        let id = IdentityMap::default();
        let row = build_row(
            ev(vec![
                ("plan", PropVal::Str("pro".into())),
                ("amount", PropVal::Int(3)),
                ("n", PropVal::Float(1.5)), // float does not narrow into an int column
                ("ok", PropVal::Str("yes".into())),
                ("rest", PropVal::Null),
            ]),
            &id,
            &cols,
        );
        assert_eq!(row.promoted, [Some(Cell::Str("pro".into())), Some(Cell::Float(3.0)), None, None, None]);
        assert_eq!(row.props, r#"{"n":1.5,"ok":"yes","rest":null}"#);
        assert_eq!(row.person_key, "a:dev");
    }

    #[test]
    fn synthetic_rows_keep_their_props_and_stay_out_of_stats() {
        let mut e = ev(vec![("key", PropVal::Str("replay/p=1/x".into()))]);
        e.synthetic = true;
        let mut stats = PlanStats::default();
        stats.observe(&e);
        assert!(stats.days.is_empty());
        let row = build_row(e, &IdentityMap::default(), &[col("key", PropType::String)]);
        assert_eq!(row.promoted, [None]);
        assert_eq!(row.props, r#"{"key":"replay/p=1/x"}"#);
    }

    #[test]
    fn stats_count_keys_types_and_names_per_day() {
        let mut stats = PlanStats::default();
        stats.observe(&ev(vec![("plan", PropVal::Str("a".into())), ("n", PropVal::Int(1))]));
        stats.observe(&ev(vec![("plan", PropVal::Null), ("n", PropVal::Float(1.5))]));
        let mut other = ev(vec![]);
        other.ts += 86_400_000;
        other.name = "view".into();
        stats.observe(&other);
        let d = &stats.days["2026-11-18"];
        assert_eq!((d.events, d.names["purchase"]), (2, 2));
        assert_eq!(d.keys["plan"].n, 2);
        assert_eq!(d.keys["plan"].types, [("null".to_owned(), 1), ("string".to_owned(), 1)].into());
        assert_eq!(d.keys["n"].types, [("float".to_owned(), 1), ("int".to_owned(), 1)].into());
        assert_eq!(stats.days["2026-11-19"].names["view"], 1);
        let mut stored = la_core::schema::SchemaStats::default();
        stats.add_to(&mut stored);
        stats.add_to(&mut stored);
        assert_eq!(stored.days["2026-11-18"].keys["plan"].n, 4);
    }
}
