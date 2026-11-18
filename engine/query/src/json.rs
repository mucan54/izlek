//! Minimal top-level JSON object scanner and the loose value conversions of
//! `spec/flags.md` §4 (shared by filter expressions and the `prop*` UDFs).
//!
//! `props` cells are JSON objects written by the shipper (compact, keys
//! byte-sorted), but nothing here relies on that: any valid JSON object works.

use std::borrow::Cow;

/// Kind of a raw JSON value slice.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Null,
    Bool,
    Number,
    String,
    Composite,
}

fn skip_ws(b: &[u8], mut i: usize) -> usize {
    while i < b.len() && matches!(b[i], b' ' | b'\n' | b'\r' | b'\t') {
        i += 1;
    }
    i
}

/// `i` points at the opening quote; returns the index just past the closing one.
fn skip_string(b: &[u8], mut i: usize) -> Option<usize> {
    i += 1;
    while i < b.len() {
        match b[i] {
            b'"' => return Some(i + 1),
            b'\\' => i += 2,
            _ => i += 1,
        }
    }
    None
}

/// Returns the index just past the value starting at `i`.
fn skip_value(b: &[u8], i: usize) -> Option<usize> {
    match *b.get(i)? {
        b'"' => skip_string(b, i),
        b'{' | b'[' => {
            let mut depth = 0usize;
            let mut j = i;
            while j < b.len() {
                match b[j] {
                    b'"' => {
                        j = skip_string(b, j)?;
                        continue;
                    }
                    b'{' | b'[' => depth += 1,
                    b'}' | b']' => {
                        depth -= 1;
                        if depth == 0 {
                            return Some(j + 1);
                        }
                    }
                    _ => {}
                }
                j += 1;
            }
            None
        }
        _ => {
            let mut j = i;
            while j < b.len() && !matches!(b[j], b',' | b'}' | b']' | b' ' | b'\n' | b'\r' | b'\t') {
                j += 1;
            }
            (j > i).then_some(j)
        }
    }
}

/// Calls `f(raw_key_including_quotes, raw_value)` for every top-level member.
/// Returns `false` if the text is not a well-formed object envelope.
pub fn for_each_member<'a>(json: &'a str, mut f: impl FnMut(&'a str, &'a str) -> bool) -> bool {
    let b = json.as_bytes();
    let mut i = skip_ws(b, 0);
    if b.get(i) != Some(&b'{') {
        return false;
    }
    i = skip_ws(b, i + 1);
    if b.get(i) == Some(&b'}') {
        return true;
    }
    loop {
        if b.get(i) != Some(&b'"') {
            return false;
        }
        let Some(kend) = skip_string(b, i) else { return false };
        let key = &json[i..kend];
        i = skip_ws(b, kend);
        if b.get(i) != Some(&b':') {
            return false;
        }
        i = skip_ws(b, i + 1);
        let Some(vend) = skip_value(b, i) else { return false };
        if !f(key, &json[i..vend]) {
            return true;
        }
        i = skip_ws(b, vend);
        match b.get(i) {
            Some(b',') => i = skip_ws(b, i + 1),
            Some(b'}') => return true,
            _ => return false,
        }
    }
}

/// Decodes a raw JSON string literal (with quotes).
pub fn unquote(raw: &str) -> Option<Cow<'_, str>> {
    let inner = raw.strip_prefix('"')?.strip_suffix('"')?;
    if !inner.as_bytes().contains(&b'\\') {
        return Some(Cow::Borrowed(inner));
    }
    serde_json::from_str::<String>(raw).ok().map(Cow::Owned)
}

/// Raw text of the top-level member `key` (`None` when absent or malformed).
pub fn get<'a>(json: &'a str, key: &str) -> Option<&'a str> {
    let mut out = None;
    for_each_member(json, |k, v| {
        let inner = &k[1..k.len() - 1];
        let hit = if inner.as_bytes().contains(&b'\\') { unquote(k).is_some_and(|d| d == key) } else { inner == key };
        if hit {
            out = Some(v);
        }
        !hit
    });
    out
}

pub fn kind(raw: &str) -> Kind {
    match raw.as_bytes().first() {
        Some(b'"') => Kind::String,
        Some(b't' | b'f') => Kind::Bool,
        Some(b'n') | None => Kind::Null,
        Some(b'{' | b'[') => Kind::Composite,
        _ => Kind::Number,
    }
}

/// `str(x)` for a number: integer-valued → plain decimal without exponent or
/// fraction; anything else → shortest round-trip decimal.
pub fn fmt_f64(x: f64) -> String {
    if !x.is_finite() {
        return if x.is_nan() {
            "NaN".into()
        } else if x > 0.0 {
            "Infinity".into()
        } else {
            "-Infinity".into()
        };
    }
    if x == 0.0 {
        return "0".into();
    }
    if x.fract() == 0.0 {
        return format!("{x:.0}");
    }
    format!("{x}")
}

fn is_plain_int(s: &str) -> bool {
    let d = s.strip_prefix('-').unwrap_or(s);
    !d.is_empty() && d.bytes().all(|c| c.is_ascii_digit())
}

/// `str(x)` of a raw JSON number literal.
pub fn number_str(raw: &str) -> Cow<'_, str> {
    if is_plain_int(raw) {
        return if raw == "-0" { Cow::Borrowed("0") } else { Cow::Borrowed(raw) };
    }
    match raw.parse::<f64>() {
        Ok(x) => Cow::Owned(fmt_f64(x)),
        Err(_) => Cow::Borrowed(raw),
    }
}

/// `str(x)` of a raw JSON value; `None` for null. Objects and arrays keep
/// their JSON text.
pub fn value_str(raw: &str) -> Option<Cow<'_, str>> {
    match kind(raw) {
        Kind::Null => None,
        Kind::String => unquote(raw),
        Kind::Number => Some(number_str(raw)),
        Kind::Bool | Kind::Composite => Some(Cow::Borrowed(raw)),
    }
}

/// `^-?[0-9]+(\.[0-9]+)?$`
pub fn looks_numeric(s: &str) -> bool {
    let d = s.strip_prefix('-').unwrap_or(s);
    let (int, frac) = match d.split_once('.') {
        Some((i, f)) => (i, Some(f)),
        None => (d, None),
    };
    !int.is_empty()
        && int.bytes().all(|c| c.is_ascii_digit())
        && frac.is_none_or(|f| !f.is_empty() && f.bytes().all(|c| c.is_ascii_digit()))
}

/// Loose numeric conversion of a string (`flags.md` §4).
pub fn str_to_num(s: &str) -> Option<f64> {
    if looks_numeric(s) {
        s.parse().ok()
    } else {
        None
    }
}

/// The value as a number only if it **is** a JSON number.
pub fn strict_num(raw: &str) -> Option<f64> {
    match kind(raw) {
        Kind::Number => raw.parse::<f64>().ok().filter(|x| x.is_finite()),
        _ => None,
    }
}

/// Number, or a string that looks like one; booleans never convert.
pub fn loose_num(raw: &str) -> Option<f64> {
    match kind(raw) {
        Kind::Number => raw.parse::<f64>().ok().filter(|x| x.is_finite()),
        Kind::String => unquote(raw).and_then(|s| str_to_num(&s)),
        _ => None,
    }
}

pub fn as_bool(raw: &str) -> Option<bool> {
    match raw {
        "true" => Some(true),
        "false" => Some(false),
        _ => None,
    }
}

/// `str(v)` of a request-side JSON value (`None` for null).
pub fn request_value_str(v: &serde_json::Value) -> Option<String> {
    match v {
        serde_json::Value::Null => None,
        serde_json::Value::String(s) => Some(s.clone()),
        serde_json::Value::Bool(b) => Some(b.to_string()),
        serde_json::Value::Number(n) => Some(number_str(&n.to_string()).into_owned()),
        other => Some(other.to_string()),
    }
}

/// Loose numeric conversion of a request-side value.
pub fn request_value_num(v: &serde_json::Value) -> Option<f64> {
    match v {
        serde_json::Value::Number(n) => n.as_f64().filter(|x| x.is_finite()),
        serde_json::Value::String(s) => str_to_num(s),
        _ => None,
    }
}

/// Recursively key-sorted, whitespace-free rendering (result-cache keys).
pub fn canonical(v: &serde_json::Value) -> String {
    fn go(v: &serde_json::Value, out: &mut String) {
        match v {
            serde_json::Value::Object(m) => {
                let mut keys: Vec<&String> = m.keys().collect();
                keys.sort();
                out.push('{');
                for (i, k) in keys.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    out.push_str(&serde_json::Value::String((*k).clone()).to_string());
                    out.push(':');
                    go(&m[*k], out);
                }
                out.push('}');
            }
            serde_json::Value::Array(a) => {
                out.push('[');
                for (i, x) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    go(x, out);
                }
                out.push(']');
            }
            other => out.push_str(&other.to_string()),
        }
    }
    let mut s = String::new();
    go(v, &mut s);
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_top_level_members_only() {
        let j = r#"{"a":1,"b":{"a":2,"x":[1,"}",{"q":"\""}]},"c\"d":"v","e":"x\\","f":null,"g":true,"h":-1.50}"#;
        assert_eq!(get(j, "a"), Some("1"));
        assert_eq!(get(j, "b"), Some(r#"{"a":2,"x":[1,"}",{"q":"\""}]}"#));
        assert_eq!(get(j, "c\"d"), Some(r#""v""#));
        assert_eq!(get(j, "e"), Some(r#""x\\""#));
        assert_eq!(get(j, "f"), Some("null"));
        assert_eq!(get(j, "g"), Some("true"));
        assert_eq!(get(j, "h"), Some("-1.50"));
        assert_eq!(get(j, "x"), None);
        assert_eq!(get(j, "q"), None);
        assert_eq!(get("{}", "a"), None);
        assert_eq!(get(" { \"a\" : [1, 2] , \"b\" : \"z\" } ", "b"), Some("\"z\""));
        for bad in ["", "[]", "{", "{\"a\"", "{\"a\":", "{\"a\":1,", "nope", "{\"a\":\"x}"] {
            assert_eq!(get(bad, "zz"), None, "{bad}");
        }
    }

    #[test]
    fn string_views_follow_flags_spec() {
        assert_eq!(value_str("3.0").unwrap(), "3");
        assert_eq!(value_str("3").unwrap(), "3");
        assert_eq!(value_str("-0").unwrap(), "0");
        assert_eq!(value_str("1e3").unwrap(), "1000");
        assert_eq!(value_str("2.50").unwrap(), "2.5");
        assert_eq!(value_str("9007199254740993").unwrap(), "9007199254740993");
        assert_eq!(value_str("true").unwrap(), "true");
        assert_eq!(value_str("\"a\\nb\"").unwrap(), "a\nb");
        assert_eq!(value_str("[1,2]").unwrap(), "[1,2]");
        assert!(value_str("null").is_none());
        assert_eq!(fmt_f64(1e21), "1000000000000000000000");
        assert_eq!(fmt_f64(0.1), "0.1");
        assert_eq!(fmt_f64(-0.0), "0");
    }

    #[test]
    fn numeric_views() {
        assert_eq!(loose_num("12"), Some(12.0));
        assert_eq!(loose_num("\"12.5\""), Some(12.5));
        assert_eq!(loose_num("\"-3\""), Some(-3.0));
        for no in ["\"1e3\"", "\" 1\"", "\"1.\"", "\".5\"", "\"+1\"", "true", "null", "\"\"", "[1]", "\"abc\""] {
            assert_eq!(loose_num(no), None, "{no}");
        }
        assert_eq!(strict_num("\"12\""), None);
        assert_eq!(strict_num("1e2"), Some(100.0));
        assert_eq!(as_bool("true"), Some(true));
        assert_eq!(as_bool("\"true\""), None);
    }

    #[test]
    fn canonical_sorts_keys() {
        let a: serde_json::Value = serde_json::from_str(r#"{"b":[{"z":1,"a":2}],"a":"x"}"#).unwrap();
        let b: serde_json::Value = serde_json::from_str(r#"{ "a":"x", "b":[{"a":2,"z":1}] }"#).unwrap();
        assert_eq!(canonical(&a), canonical(&b));
        assert_eq!(canonical(&a), r#"{"a":"x","b":[{"a":2,"z":1}]}"#);
    }
}
