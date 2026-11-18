//! Logical data model of a generated project and an independent, deliberately
//! naive evaluator of the filter-expression semantics (`spec/query.md` §2,
//! `spec/flags.md` §4). Nothing here shares code with the query service.

use serde_json::Value;
use std::collections::{BTreeMap, HashMap};

/// A property value as the SDK sent it.
#[derive(Debug, Clone, PartialEq)]
pub enum PVal {
    Str(String),
    Int(i64),
    Float(f64),
    Bool(bool),
}

impl PVal {
    pub fn to_json(&self) -> Value {
        match self {
            PVal::Str(s) => Value::String(s.clone()),
            PVal::Int(i) => Value::from(*i),
            PVal::Float(f) => Value::from(*f),
            PVal::Bool(b) => Value::Bool(*b),
        }
    }

    pub fn is_number(&self) -> bool {
        matches!(self, PVal::Int(_) | PVal::Float(_))
    }

    /// `str(x)`
    pub fn text(&self) -> String {
        match self {
            PVal::Str(s) => s.clone(),
            PVal::Int(i) => i.to_string(),
            PVal::Float(f) if f.fract() == 0.0 && f.abs() < 1e15 => format!("{}", *f as i64),
            PVal::Float(f) => format!("{f}"),
            PVal::Bool(b) => b.to_string(),
        }
    }

    /// Number, or a string matching `^-?[0-9]+(\.[0-9]+)?$`; never a boolean.
    pub fn number(&self) -> Option<f64> {
        match self {
            PVal::Int(i) => Some(*i as f64),
            PVal::Float(f) => Some(*f),
            PVal::Str(s) => numeric_string(s),
            PVal::Bool(_) => None,
        }
    }
}

fn numeric_string(s: &str) -> Option<f64> {
    let body = s.strip_prefix('-').unwrap_or(s);
    let mut parts = body.splitn(2, '.');
    let int = parts.next().unwrap_or("");
    let frac = parts.next();
    let digits = |x: &str| !x.is_empty() && x.chars().all(|c| c.is_ascii_digit());
    if digits(int) && frac.is_none_or(digits) {
        s.parse().ok()
    } else {
        None
    }
}

fn json_to_pval(v: &Value) -> Option<PVal> {
    match v {
        Value::Null => None,
        Value::Bool(b) => Some(PVal::Bool(*b)),
        Value::Number(n) => Some(match n.as_i64() {
            Some(i) => PVal::Int(i),
            None => PVal::Float(n.as_f64().unwrap_or(0.0)),
        }),
        Value::String(s) => Some(PVal::Str(s.clone())),
        other => Some(PVal::Str(other.to_string())),
    }
}

/// One event of the logical `events` table (after identity resolution).
#[derive(Debug, Clone, PartialEq)]
pub struct Row {
    pub uuid: [u8; 16],
    pub ts: i64,
    pub recv_ts: i64,
    pub event: String,
    pub person_key: String,
    pub anon_id: String,
    pub user_id: Option<String>,
    pub session_id: Option<[u8; 16]>,
    pub platform: Option<String>,
    pub country: Option<String>,
    pub props: BTreeMap<String, PVal>,
}

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// Current person properties: `person_key → key → value`.
pub type Persons = HashMap<String, BTreeMap<String, PVal>>;

/// Resolves a `Ref` for a row: `None` = missing/null.
pub fn lookup(row: &Row, persons: &Persons, reference: &str) -> Option<PVal> {
    if let Some(k) = reference.strip_prefix("props.") {
        return row.props.get(k).cloned();
    }
    if let Some(k) = reference.strip_prefix("person.") {
        return persons.get(&row.person_key).and_then(|p| p.get(k)).cloned();
    }
    let s = match reference {
        "event" => Some(row.event.clone()),
        "person_key" => Some(row.person_key.clone()),
        "anon_id" => Some(row.anon_id.clone()),
        "user_id" => row.user_id.clone(),
        "session_id" => row.session_id.as_ref().map(|s| hex(s)),
        "platform" => row.platform.clone(),
        "country" => row.country.clone(),
        // The generator leaves the remaining fixed columns null.
        _ => None,
    };
    s.map(PVal::Str)
}

fn loose_eq(p: &PVal, v: &PVal) -> bool {
    if p.is_number() && v.is_number() {
        return p.number() == v.number();
    }
    p.text() == v.text()
}

/// Evaluates a filter tree given as request JSON.
pub fn eval(filter: &Value, row: &Row, persons: &Persons) -> bool {
    let obj = filter.as_object().expect("filter object");
    if let Some(items) = obj.get("and") {
        return items.as_array().expect("and").iter().all(|f| eval(f, row, persons));
    }
    if let Some(items) = obj.get("or") {
        return items.as_array().expect("or").iter().any(|f| eval(f, row, persons));
    }
    if let Some(inner) = obj.get("not") {
        return !eval(inner, row, persons);
    }
    let p = lookup(row, persons, obj["prop"].as_str().expect("prop"));
    let op = obj["op"].as_str().expect("op");
    let value = obj.get("value").cloned().unwrap_or(Value::Null);
    let Some(p) = p else {
        // Missing value: is_set false, ne / not_in true, everything else false.
        return matches!(op, "ne" | "not_in");
    };
    let one = |v: &Value| json_to_pval(v).is_some_and(|v| loose_eq(&p, &v));
    match op {
        "is_set" => true,
        "eq" => one(&value),
        "ne" => !one(&value),
        "in" => value.as_array().expect("in list").iter().any(one),
        "not_in" => !value.as_array().expect("in list").iter().any(one),
        "gt" | "gte" | "lt" | "lte" => {
            let (Some(a), Some(b)) = (p.number(), json_to_pval(&value).and_then(|v| v.number())) else {
                return false;
            };
            match op {
                "gt" => a > b,
                "gte" => a >= b,
                "lt" => a < b,
                _ => a <= b,
            }
        }
        "contains" => json_to_pval(&value).is_some_and(|v| p.text().contains(&v.text())),
        other => panic!("unknown op {other}"),
    }
}

/// `{event, filter}` selector.
pub fn matches(sel: &Value, row: &Row, persons: &Persons) -> bool {
    let by_event = match sel.get("event") {
        Some(Value::String(e)) => &row.event == e,
        _ => true,
    };
    by_event && sel.get("filter").filter(|f| !f.is_null()).is_none_or(|f| eval(f, row, persons))
}

/// Optional top-level `filter`.
pub fn passes(filter: Option<&Value>, row: &Row, persons: &Persons) -> bool {
    filter.filter(|f| !f.is_null()).is_none_or(|f| eval(f, row, persons))
}

/// Breakdown value: `str(p)` or `None`.
pub fn text_of(row: &Row, persons: &Persons, reference: &str) -> Option<String> {
    lookup(row, persons, reference).map(|p| p.text())
}

/// Aggregation value: loose number or `None`.
pub fn number_of(row: &Row, persons: &Persons, reference: &str) -> Option<f64> {
    lookup(row, persons, reference).and_then(|p| p.number())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn row(props: &[(&str, PVal)]) -> Row {
        Row {
            uuid: [1; 16],
            ts: 0,
            recv_ts: 0,
            event: "e".into(),
            person_key: "u:1".into(),
            anon_id: "a".into(),
            user_id: Some("1".into()),
            session_id: None,
            platform: Some("ios".into()),
            country: None,
            props: props.iter().map(|(k, v)| (k.to_string(), v.clone())).collect(),
        }
    }

    #[test]
    fn loose_semantics() {
        let persons = Persons::new();
        let r = row(&[("n", PVal::Int(3)), ("s", PVal::Str("3".into())), ("f", PVal::Float(3.0)), ("b", PVal::Bool(true))]);
        let t = |f: Value| eval(&f, &r, &persons);
        assert!(t(json!({"prop": "props.n", "op": "eq", "value": 3.0})));
        assert!(t(json!({"prop": "props.n", "op": "eq", "value": "3"})));
        assert!(t(json!({"prop": "props.s", "op": "eq", "value": 3})));
        assert!(!t(json!({"prop": "props.s", "op": "eq", "value": "3.0"})));
        assert!(t(json!({"prop": "props.f", "op": "eq", "value": "3"})));
        assert!(t(json!({"prop": "props.s", "op": "gte", "value": 3})));
        assert!(!t(json!({"prop": "props.b", "op": "gte", "value": 0})));
        assert!(t(json!({"prop": "props.b", "op": "eq", "value": "true"})));
        assert!(t(json!({"prop": "props.zz", "op": "ne", "value": 1})));
        assert!(!t(json!({"prop": "props.zz", "op": "eq", "value": 1})));
        assert!(t(json!({"not": {"prop": "props.zz", "op": "gt", "value": 1}})));
        assert!(t(json!({"prop": "country", "op": "not_in", "value": ["TR"]})));
        assert!(!t(json!({"prop": "country", "op": "is_set"})));
        assert!(t(json!({"prop": "platform", "op": "contains", "value": "o"})));
        assert!(t(json!({"and": []})) && !t(json!({"or": []})));
    }
}
