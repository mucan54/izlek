//! Property values on their way from a WAL entry into `props` JSON, a
//! promoted column or the persons table.

use la_codec::wire;
use la_compactor::json::{json_f64, push_json_string};

/// A decoded property value. `Json` holds the compact text of an array or
/// object (nested keys sorted, no whitespace).
#[derive(Debug, Clone, PartialEq)]
pub enum PropVal {
    Null,
    Bool(bool),
    Int(i64),
    Float(f64),
    Str(String),
    Json(String),
}

impl PropVal {
    /// Type name used in the schema statistics (`spec/manifest.md` §5).
    pub fn type_name(&self) -> &'static str {
        match self {
            PropVal::Null => "null",
            PropVal::Bool(_) => "bool",
            PropVal::Int(_) => "int",
            PropVal::Float(_) => "float",
            PropVal::Str(_) => "string",
            PropVal::Json(_) => "json",
        }
    }

    /// Bytes counted against the per-value limit (strings and JSON only).
    pub fn limited_len(&self) -> usize {
        match self {
            PropVal::Str(s) | PropVal::Json(s) => s.len(),
            _ => 0,
        }
    }

    pub fn write_json(&self, out: &mut String) {
        match self {
            PropVal::Null => out.push_str("null"),
            PropVal::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            PropVal::Int(i) => out.push_str(&i.to_string()),
            PropVal::Float(f) => out.push_str(&json_f64(*f)),
            PropVal::Str(s) => push_json_string(out, s),
            PropVal::Json(j) => out.push_str(j),
        }
    }

    pub fn to_json(&self) -> String {
        let mut s = String::new();
        self.write_json(&mut s);
        s
    }

    /// `spec/parquet.md` §2.5: a `json` tag is parsed and embedded (kept as a
    /// string when it is not valid JSON); `NaN/±Inf` become `null`.
    pub fn from_wire(v: &wire::Value) -> Self {
        match v {
            wire::Value::Null => PropVal::Null,
            wire::Value::Bool(b) => PropVal::Bool(*b),
            wire::Value::Int(i) => PropVal::Int(*i),
            wire::Value::Float(f) if f.is_finite() => PropVal::Float(*f),
            wire::Value::Float(_) => PropVal::Null,
            wire::Value::Str(s) => PropVal::Str(s.clone()),
            wire::Value::Json(text) => match serde_json::from_str::<serde_json::Value>(text) {
                Ok(v) => PropVal::from_json(&v),
                Err(_) => PropVal::Str(text.clone()),
            },
        }
    }

    /// Scalars map directly, arrays and objects are kept as compact JSON.
    pub fn from_json(v: &serde_json::Value) -> Self {
        use serde_json::Value;
        match v {
            Value::Null => PropVal::Null,
            Value::Bool(b) => PropVal::Bool(*b),
            Value::Number(n) => match (n.as_i64(), n.as_f64()) {
                (Some(i), _) => PropVal::Int(i),
                (None, Some(f)) if f.is_finite() => PropVal::Float(f),
                _ => PropVal::Null,
            },
            Value::String(s) => PropVal::Str(s.clone()),
            Value::Array(_) | Value::Object(_) => PropVal::Json(v.to_string()),
        }
    }
}

/// Sorted, de-duplicated property list (later entries win), as stored in a
/// decoded event.
pub type PropList = Vec<(String, PropVal)>;

/// Sorts by key (byte order) keeping the **last** value of a repeated key.
pub fn sort_last_wins(mut props: PropList) -> PropList {
    // Stable sort keeps wire order among equal keys; the last one survives.
    props.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    let mut out: PropList = Vec::with_capacity(props.len());
    for p in props {
        match out.last_mut() {
            Some(last) if last.0 == p.0 => *last = p,
            _ => out.push(p),
        }
    }
    out
}

pub fn find(props: &PropList, key: &str) -> Option<usize> {
    props.binary_search_by(|p| p.0.as_bytes().cmp(key.as_bytes())).ok()
}

/// Canonical `props` JSON of a sorted list.
pub fn to_props_json(props: &PropList) -> String {
    let mut out = String::with_capacity(2 + props.len() * 24);
    out.push('{');
    for (i, (k, v)) in props.iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        push_json_string(&mut out, k);
        out.push(':');
        v.write_json(&mut out);
    }
    out.push('}');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wire_values_follow_the_row_production_rules() {
        assert_eq!(PropVal::from_wire(&wire::Value::Float(f64::NAN)), PropVal::Null);
        assert_eq!(PropVal::from_wire(&wire::Value::Float(f64::NEG_INFINITY)), PropVal::Null);
        assert_eq!(PropVal::from_wire(&wire::Value::Float(1.0)).to_json(), "1.0");
        assert_eq!(
            PropVal::from_wire(&wire::Value::Json("{ \"b\": [1, 2.50, {\"z\":null}], \"a\": \"x\" }".into())),
            PropVal::Json(r#"{"a":"x","b":[1,2.5,{"z":null}]}"#.into())
        );
        assert_eq!(PropVal::from_wire(&wire::Value::Json("{oops".into())), PropVal::Str("{oops".into()));
        // A json tag holding a scalar is embedded as that scalar.
        assert_eq!(PropVal::from_wire(&wire::Value::Json("12".into())), PropVal::Int(12));
        assert_eq!(PropVal::from_json(&serde_json::json!(18446744073709551615u64)).type_name(), "float");
    }

    #[test]
    fn canonical_props_json() {
        let props = sort_last_wins(vec![
            ("b".into(), PropVal::Int(1)),
            ("a\"q".into(), PropVal::Str("x\ny".into())),
            ("b".into(), PropVal::Bool(true)),
            ("Z".into(), PropVal::Null),
            ("é".into(), PropVal::Float(0.5)),
            ("j".into(), PropVal::Json("[1,2]".into())),
        ]);
        assert_eq!(to_props_json(&props), r#"{"Z":null,"a\"q":"x\ny","b":true,"j":[1,2],"é":0.5}"#);
        assert_eq!(find(&props, "b"), Some(2));
        assert_eq!(find(&props, "nope"), None);
        assert_eq!(to_props_json(&Vec::new()), "{}");
    }
}
