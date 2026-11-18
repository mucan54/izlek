//! Filter-expression tree (`spec/query.md` §2) and its compilation into
//! DataFusion expressions over the `events` table.
//!
//! Every compiled leaf is **two-valued** (never SQL NULL), so `not` behaves
//! as the spec demands: a missing property makes `eq` false and `ne` true.

use crate::error::{ApiError, ApiResult};
use crate::json;
use crate::udf::{self, View};
use arrow::datatypes::DataType;
use datafusion::common::ScalarValue;
use datafusion::logical_expr::{cast, lit, Expr, ScalarUDF};
use datafusion::prelude::{coalesce, ident};
use la_core::persons::PersonTable;
use la_core::schema::{PromotedColumn, PropType, DIM_COLUMNS};
use serde_json::Value;
use std::sync::Arc;

const MAX_DEPTH: usize = 32;
const MAX_NODES: usize = 2_000;
const MAX_IN_LIST: usize = 10_000;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Op {
    Eq,
    Ne,
    In,
    NotIn,
    Gt,
    Gte,
    Lt,
    Lte,
    Contains,
    IsSet,
}

impl Op {
    fn parse(s: &str) -> ApiResult<Self> {
        Ok(match s {
            "eq" => Op::Eq,
            "ne" => Op::Ne,
            "in" => Op::In,
            "not_in" => Op::NotIn,
            "gt" => Op::Gt,
            "gte" => Op::Gte,
            "lt" => Op::Lt,
            "lte" => Op::Lte,
            "contains" => Op::Contains,
            "is_set" => Op::IsSet,
            other => return Err(ApiError::bad_request(format!("unknown operator {other:?}"))),
        })
    }
}

/// A property reference.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Ref {
    /// Fixed string column (index into [`FIXED_REFS`]).
    Column(&'static str),
    Session,
    Prop(String),
    Person(String),
}

/// Fixed columns a filter may name (`session_id` is handled separately).
pub const FIXED_REFS: [&str; 4] = ["event", "person_key", "anon_id", "user_id"];

impl Ref {
    pub fn parse(s: &str) -> ApiResult<Self> {
        if let Some(k) = s.strip_prefix("props.") {
            if k.is_empty() {
                return Err(ApiError::bad_request("empty property name"));
            }
            return Ok(Ref::Prop(k.to_owned()));
        }
        if let Some(k) = s.strip_prefix("person.") {
            if k.is_empty() {
                return Err(ApiError::bad_request("empty person property name"));
            }
            return Ok(Ref::Person(k.to_owned()));
        }
        if s == "session_id" {
            return Ok(Ref::Session);
        }
        if let Some(c) = FIXED_REFS.iter().chain(DIM_COLUMNS.iter()).find(|c| **c == s) {
            return Ok(Ref::Column(c));
        }
        Err(ApiError::bad_request(format!("unknown property reference {s:?}")))
    }

    pub fn uses_person(&self) -> bool {
        matches!(self, Ref::Person(_))
    }
}

#[derive(Debug, Clone, PartialEq)]
pub enum Filter {
    And(Vec<Filter>),
    Or(Vec<Filter>),
    Not(Box<Filter>),
    Leaf { prop: Ref, op: Op, value: Value },
}

impl Filter {
    pub fn parse(v: &Value) -> ApiResult<Self> {
        let mut nodes = 0usize;
        Self::parse_at(v, 0, &mut nodes)
    }

    fn parse_at(v: &Value, depth: usize, nodes: &mut usize) -> ApiResult<Self> {
        *nodes += 1;
        if depth > MAX_DEPTH || *nodes > MAX_NODES {
            return Err(ApiError::bad_request("filter expression is too large"));
        }
        let Value::Object(m) = v else {
            return Err(ApiError::bad_request("filter expression must be an object"));
        };
        let list = |v: &Value, nodes: &mut usize| -> ApiResult<Vec<Filter>> {
            let Value::Array(items) = v else {
                return Err(ApiError::bad_request("`and`/`or` take an array"));
            };
            items.iter().map(|x| Self::parse_at(x, depth + 1, nodes)).collect()
        };
        if m.len() == 1 {
            if let Some(x) = m.get("and") {
                return Ok(Filter::And(list(x, nodes)?));
            }
            if let Some(x) = m.get("or") {
                return Ok(Filter::Or(list(x, nodes)?));
            }
            if let Some(x) = m.get("not") {
                return Ok(Filter::Not(Box::new(Self::parse_at(x, depth + 1, nodes)?)));
            }
        }
        if let Some(k) = m.keys().find(|k| !matches!(k.as_str(), "prop" | "op" | "value")) {
            return Err(ApiError::bad_request(format!("unexpected key {k:?} in filter expression")));
        }
        let prop = m.get("prop").and_then(Value::as_str).ok_or_else(|| ApiError::bad_request("filter needs `prop`"))?;
        let op = m.get("op").and_then(Value::as_str).ok_or_else(|| ApiError::bad_request("filter needs `op`"))?;
        let op = Op::parse(op)?;
        let value = m.get("value").cloned().unwrap_or(Value::Null);
        match (&op, &value) {
            (Op::In | Op::NotIn, Value::Array(items)) => {
                if items.len() > MAX_IN_LIST {
                    return Err(ApiError::bad_request("`in` list is too long"));
                }
                if items.iter().any(|x| x.is_array() || x.is_object()) {
                    return Err(ApiError::bad_request("`in` values must be scalars"));
                }
            }
            (Op::In | Op::NotIn, _) => return Err(ApiError::bad_request("`in`/`not_in` take an array value")),
            (Op::IsSet, _) => {}
            (_, Value::Array(_) | Value::Object(_)) => {
                return Err(ApiError::bad_request("filter value must be a scalar"))
            }
            _ => {}
        }
        Ok(Filter::Leaf { prop: Ref::parse(prop)?, op, value })
    }

    pub fn uses_person(&self) -> bool {
        match self {
            Filter::And(v) | Filter::Or(v) => v.iter().any(Filter::uses_person),
            Filter::Not(x) => x.uses_person(),
            Filter::Leaf { prop, .. } => prop.uses_person(),
        }
    }
}

/// Typed projections of a property reference.
pub struct Views {
    /// `str(p)`: Utf8, null when the value is missing/null.
    pub text: Expr,
    /// Float64, non-null only when the value **is** a number.
    pub strict_num: Option<Expr>,
    /// Float64: number or numeric-looking string.
    pub loose_num: Expr,
    /// Raw 16-byte column, when the reference is `session_id`.
    pub binary: Option<Expr>,
    pub nullable: bool,
}

/// Resolves references against one project's schema and person table.
#[derive(Clone)]
pub struct Compiler {
    promoted: Arc<Vec<PromotedColumn>>,
    persons: Arc<PersonTable>,
    prop: Arc<ScalarUDF>,
    prop_num: Arc<ScalarUDF>,
    prop_jnum: Arc<ScalarUDF>,
    to_num: Arc<ScalarUDF>,
    to_str: Arc<ScalarUDF>,
    hex: Arc<ScalarUDF>,
}

fn f64_of(e: Expr) -> Expr {
    cast(e, DataType::Float64)
}

fn guarded(nullable: bool, value: Expr, test: Expr) -> Expr {
    if nullable {
        value.is_not_null().and(test)
    } else {
        test
    }
}

impl Compiler {
    pub fn new(promoted: Vec<PromotedColumn>, persons: Arc<PersonTable>) -> Self {
        Self {
            promoted: Arc::new(promoted),
            persons,
            prop: Arc::new(udf::prop()),
            prop_num: Arc::new(udf::prop_num()),
            prop_jnum: Arc::new(udf::prop_jnum()),
            to_num: Arc::new(udf::str_to_num()),
            to_str: Arc::new(udf::num_to_str()),
            hex: Arc::new(udf::hex()),
        }
    }

    pub fn views(&self, r: &Ref) -> Views {
        match r {
            Ref::Column(c) => {
                let col = ident(*c);
                Views {
                    loose_num: self.to_num.call(vec![col.clone()]),
                    text: col,
                    strict_num: None,
                    binary: None,
                    nullable: !matches!(*c, "event" | "person_key" | "anon_id"),
                }
            }
            Ref::Session => {
                let text = self.hex.call(vec![ident("session_id")]);
                Views {
                    loose_num: self.to_num.call(vec![text.clone()]),
                    text,
                    strict_num: None,
                    binary: Some(ident("session_id")),
                    nullable: true,
                }
            }
            Ref::Prop(key) => {
                let args = || vec![ident("props"), lit(key.as_str())];
                let json_text = self.prop.call(args());
                let json_strict = self.prop_jnum.call(args());
                let json_loose = self.prop_num.call(args());
                let Some(pc) = self.promoted.iter().find(|c| &c.key == key) else {
                    return Views {
                        text: json_text,
                        strict_num: Some(json_strict),
                        loose_num: json_loose,
                        binary: None,
                        nullable: true,
                    };
                };
                // spec/parquet.md §3: coalesce(p_x, props->x). At most one side
                // holds a value, so each view can be coalesced separately.
                let col = ident(pc.column_name());
                let (text, strict, loose) = match pc.ty {
                    PropType::String => {
                        (col.clone(), json_strict, coalesce(vec![self.to_num.call(vec![col]), json_loose]))
                    }
                    PropType::Int => (
                        cast(col.clone(), DataType::Utf8),
                        coalesce(vec![f64_of(col.clone()), json_strict]),
                        coalesce(vec![f64_of(col), json_loose]),
                    ),
                    PropType::Float => (
                        self.to_str.call(vec![col.clone()]),
                        coalesce(vec![col.clone(), json_strict]),
                        coalesce(vec![col, json_loose]),
                    ),
                    PropType::Bool => (cast(col, DataType::Utf8), json_strict, json_loose),
                };
                Views {
                    text: coalesce(vec![text, json_text]),
                    strict_num: Some(strict),
                    loose_num: loose,
                    binary: None,
                    nullable: true,
                }
            }
            Ref::Person(key) => {
                let call = |view| {
                    Arc::new(udf::person_prop(view, self.persons.clone()))
                        .call(vec![ident("person_key"), lit(key.as_str())])
                };
                Views {
                    text: call(View::Str),
                    strict_num: Some(call(View::JNum)),
                    loose_num: call(View::Num),
                    binary: None,
                    nullable: true,
                }
            }
        }
    }

    /// `str(p)` of a reference (breakdowns).
    pub fn text(&self, r: &Ref) -> Expr {
        self.views(r).text
    }

    /// Loose numeric value of a reference (aggregations).
    pub fn number(&self, r: &Ref) -> Expr {
        self.views(r).loose_num
    }

    fn eq(&self, v: &Views, value: &Value) -> Expr {
        let Some(text) = json::request_value_str(value) else {
            return lit(false);
        };
        if let (Some(bin), Some(id)) = (&v.binary, la_codec::ids::parse_uuid(&text)) {
            let id = lit(ScalarValue::FixedSizeBinary(16, Some(id.to_vec())));
            return bin.clone().is_not_null().and(bin.clone().eq(id));
        }
        let by_text = guarded(v.nullable, v.text.clone(), v.text.clone().eq(lit(text)));
        match (value.as_f64().filter(|_| value.is_number()), &v.strict_num) {
            (Some(n), Some(num)) => {
                // Both numbers → numeric; otherwise compare the string forms.
                num.clone().is_not_null().and(num.clone().eq(lit(n))).or(num.clone().is_null().and(by_text))
            }
            _ => by_text,
        }
    }

    fn any_of(&self, v: &Views, items: &[Value]) -> Expr {
        let mut texts: Vec<Expr> = Vec::new();
        let mut ids: Vec<Expr> = Vec::new();
        let mut other: Vec<Expr> = Vec::new();
        let all_ids = v.binary.is_some()
            && items.iter().all(|x| x.as_str().is_some_and(|s| la_codec::ids::parse_uuid(s).is_some()));
        for item in items {
            if item.is_null() {
                continue;
            }
            if all_ids {
                let id = la_codec::ids::parse_uuid(item.as_str().unwrap_or_default()).unwrap_or_default();
                ids.push(lit(ScalarValue::FixedSizeBinary(16, Some(id.to_vec()))));
            } else if item.is_number() && v.strict_num.is_some() {
                other.push(self.eq(v, item));
            } else if let Some(s) = json::request_value_str(item) {
                texts.push(lit(s));
            }
        }
        if let (false, Some(bin)) = (ids.is_empty(), &v.binary) {
            other.push(bin.clone().is_not_null().and(bin.clone().in_list(ids, false)));
        }
        if !texts.is_empty() {
            let test = if texts.len() == 1 {
                v.text.clone().eq(texts.pop().unwrap_or_else(|| lit(false)))
            } else {
                v.text.clone().in_list(texts, false)
            };
            other.push(guarded(v.nullable, v.text.clone(), test));
        }
        other.into_iter().reduce(Expr::or).unwrap_or_else(|| lit(false))
    }

    pub fn compile(&self, f: &Filter) -> Expr {
        match f {
            Filter::And(items) => items.iter().map(|x| self.compile(x)).reduce(Expr::and).unwrap_or_else(|| lit(true)),
            Filter::Or(items) => items.iter().map(|x| self.compile(x)).reduce(Expr::or).unwrap_or_else(|| lit(false)),
            Filter::Not(x) => datafusion::logical_expr::not(self.compile(x)),
            Filter::Leaf { prop, op, value } => {
                let v = self.views(prop);
                match op {
                    Op::IsSet => {
                        if v.nullable {
                            v.binary.clone().unwrap_or(v.text).is_not_null()
                        } else {
                            lit(true)
                        }
                    }
                    Op::Eq => self.eq(&v, value),
                    Op::Ne => datafusion::logical_expr::not(self.eq(&v, value)),
                    Op::In => self.any_of(&v, value.as_array().map(Vec::as_slice).unwrap_or(&[])),
                    Op::NotIn => datafusion::logical_expr::not(self.any_of(&v, value.as_array().map(Vec::as_slice).unwrap_or(&[]))),
                    Op::Gt | Op::Gte | Op::Lt | Op::Lte => {
                        let Some(n) = json::request_value_num(value) else {
                            return lit(false);
                        };
                        let num = v.loose_num;
                        let test = match op {
                            Op::Gt => num.clone().gt(lit(n)),
                            Op::Gte => num.clone().gt_eq(lit(n)),
                            Op::Lt => num.clone().lt(lit(n)),
                            _ => num.clone().lt_eq(lit(n)),
                        };
                        num.is_not_null().and(test)
                    }
                    Op::Contains => {
                        let Some(needle) = json::request_value_str(value) else {
                            return lit(false);
                        };
                        let test = datafusion::functions::string::expr_fn::contains(v.text.clone(), lit(needle));
                        guarded(v.nullable, v.text, test)
                    }
                }
            }
        }
    }

    /// Step/series condition: optional event name plus optional filter.
    pub fn event_condition(&self, event: Option<&str>, filter: Option<&Filter>) -> Expr {
        let by_name = event.map(|e| ident("event").eq(lit(e)));
        let by_filter = filter.map(|f| self.compile(f));
        match (by_name, by_filter) {
            (Some(a), Some(b)) => a.and(b),
            (Some(a), None) => a,
            (None, Some(b)) => b,
            (None, None) => lit(true),
        }
    }
}

/// Parses an optional `filter` field.
pub fn parse_optional(v: Option<&Value>) -> ApiResult<Option<Filter>> {
    match v {
        None | Some(Value::Null) => Ok(None),
        Some(v) => Filter::parse(v).map(Some),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_the_spec_grammar() {
        let f = Filter::parse(&json!({"and": [
            {"prop": "event", "op": "eq", "value": "purchase"},
            {"or": [{"prop": "props.amount", "op": "gte", "value": 10},
                    {"not": {"prop": "person.plan", "op": "in", "value": ["pro", "team"]}}]},
            {"prop": "country", "op": "is_set"}
        ]}))
        .unwrap();
        assert!(f.uses_person());
        let Filter::And(items) = &f else { panic!() };
        assert_eq!(items.len(), 3);
        assert_eq!(items[0], Filter::Leaf { prop: Ref::Column("event"), op: Op::Eq, value: json!("purchase") });
        assert!(!Filter::parse(&json!({"and": []})).unwrap().uses_person());
    }

    #[test]
    fn rejects_malformed_trees() {
        for bad in [
            json!([]),
            json!({"and": {}}),
            json!({"prop": "event"}),
            json!({"prop": "event", "op": "like", "value": "x"}),
            json!({"prop": "evnt", "op": "eq", "value": "x"}),
            json!({"prop": "props.", "op": "eq", "value": "x"}),
            json!({"prop": "event", "op": "in", "value": "x"}),
            json!({"prop": "event", "op": "eq", "value": {"a": 1}}),
            json!({"prop": "event", "op": "eq", "value": "x", "project": 2}),
            json!({"and": [], "or": []}),
            json!({"prop": "recv_ts", "op": "gt", "value": 1}),
            json!({"prop": "props", "op": "is_set"}),
            json!({"prop": "events.event", "op": "eq", "value": "x"}),
            json!({"prop": "event; DROP TABLE events", "op": "eq", "value": "x"}),
        ] {
            assert_eq!(Filter::parse(&bad).unwrap_err().status, 400, "{bad}");
        }
        let mut deep = json!({"prop": "event", "op": "is_set"});
        for _ in 0..40 {
            deep = json!({"not": deep});
        }
        assert!(Filter::parse(&deep).is_err());
    }
}
