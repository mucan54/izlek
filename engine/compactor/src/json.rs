//! Canonical `props` JSON helpers shared by the shipper and the compactor:
//! one object, keys byte-sorted, no whitespace (`spec/parquet.md` §1).

use serde_json::value::RawValue;
use std::borrow::Cow;
use std::collections::BTreeMap;

/// Appends `s` as a JSON string literal.
pub fn push_json_string(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0c}' => out.push_str("\\f"),
            c if (c as u32) < 0x20 => {
                out.push_str("\\u00");
                const HEX: &[u8; 16] = b"0123456789abcdef";
                out.push(HEX[(c as usize) >> 4] as char);
                out.push(HEX[(c as usize) & 15] as char);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

pub fn json_string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    push_json_string(&mut out, s);
    out
}

/// JSON literal of a finite float (`1.0`, never `1`), `null` otherwise.
pub fn json_f64(v: f64) -> String {
    if v.is_finite() {
        serde_json::to_string(&v).unwrap_or_else(|_| "null".to_owned())
    } else {
        "null".to_owned()
    }
}

/// Top-level members of a JSON object as raw value text, keys byte-sorted.
/// `None` when `text` is not a JSON object.
pub fn parse_object(text: &str) -> Option<BTreeMap<String, Cow<'_, str>>> {
    let m: BTreeMap<String, &RawValue> = serde_json::from_str(text).ok()?;
    Some(m.into_iter().map(|(k, v)| (k, Cow::Borrowed(v.get()))).collect())
}

/// Serializes members (already raw JSON text) back into canonical form.
pub fn write_object<'a, I, V>(members: I) -> String
where
    I: IntoIterator<Item = (&'a String, V)>,
    V: AsRef<str>,
{
    let mut out = String::from("{");
    let mut first = true;
    for (k, v) in members {
        if !first {
            out.push(',');
        }
        first = false;
        push_json_string(&mut out, k);
        out.push(':');
        out.push_str(v.as_ref());
    }
    out.push('}');
    out
}

/// One top-level member of an object as a parsed value.
pub fn object_member(text: &str, key: &str) -> Option<serde_json::Value> {
    let m = parse_object(text)?;
    serde_json::from_str(m.get(key)?.as_ref()).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn string_escaping_matches_serde() {
        for s in ["", "plain", "q\"uote", "back\\slash", "nl\n\r\t", "\u{1}\u{1f}", "ünï©ödé 🎉", "\u{08}\u{0c}"]
        {
            assert_eq!(json_string(s), serde_json::to_string(s).unwrap(), "{s:?}");
        }
    }

    #[test]
    fn object_roundtrip_keeps_raw_values() {
        let text = r#"{"a":1.0,"b":{"z":[1,2,{"q":null}]},"c":"x\"y","d":12345678901234567890}"#;
        let m = parse_object(text).unwrap();
        assert_eq!(m.get("a").unwrap(), "1.0");
        assert_eq!(m.get("d").unwrap(), "12345678901234567890");
        assert_eq!(write_object(m.iter()), text);
        assert!(parse_object("[1]").is_none());
        assert!(parse_object("{bad").is_none());
        assert_eq!(object_member(text, "c"), Some(serde_json::Value::String("x\"y".into())));
        assert_eq!(object_member(text, "nope"), None);
    }

    #[test]
    fn floats_always_look_like_floats() {
        assert_eq!(json_f64(1.0), "1.0");
        assert_eq!(json_f64(-0.5), "-0.5");
        assert_eq!(json_f64(f64::NAN), "null");
        assert_eq!(json_f64(f64::INFINITY), "null");
    }
}
