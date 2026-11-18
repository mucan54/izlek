//! Scalar functions: the public `prop`, `prop_num`, `prop_bool`, `hex`
//! (`spec/query.md` §7) and the internal helpers the filter compiler uses to
//! express the loose typing rules.

use crate::json;
use arrow::array::{
    Array, ArrayRef, AsArray, BooleanBuilder, Float64Array, Float64Builder, Int32Builder, StringArray, StringBuilder,
};
use arrow::datatypes::{DataType, TimeUnit};
use datafusion::common::{exec_err, plan_err, Result, ScalarValue};
use datafusion::logical_expr::{
    ColumnarValue, ScalarFunctionArgs, ScalarUDF, ScalarUDFImpl, Signature, TypeSignature, Volatility,
};
use la_core::persons::PersonTable;
use std::fmt::Write as _;
use std::hash::{Hash, Hasher};
use std::sync::Arc;

/// How a JSON value is projected into a typed column.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum View {
    /// `str(x)`; null for missing/null.
    Str,
    /// Number, or a numeric-looking string.
    Num,
    /// Only if the value **is** a JSON number.
    JNum,
    /// Only JSON booleans.
    Bool,
}

impl View {
    fn data_type(self) -> DataType {
        match self {
            View::Str => DataType::Utf8,
            View::Num | View::JNum => DataType::Float64,
            View::Bool => DataType::Boolean,
        }
    }
}

/// Builds the output column for `n` rows from a raw-JSON-value accessor.
fn build_view<'a>(n: usize, view: View, mut raw: impl FnMut(usize) -> Option<&'a str>) -> ArrayRef {
    match view {
        View::Str => {
            let mut b = StringBuilder::with_capacity(n, n * 8);
            for i in 0..n {
                match raw(i).and_then(json::value_str) {
                    Some(s) => b.append_value(s),
                    None => b.append_null(),
                }
            }
            Arc::new(b.finish())
        }
        View::Num | View::JNum => {
            let mut b = Float64Builder::with_capacity(n);
            for i in 0..n {
                b.append_option(raw(i).and_then(if view == View::Num { json::loose_num } else { json::strict_num }));
            }
            Arc::new(b.finish())
        }
        View::Bool => {
            let mut b = BooleanBuilder::with_capacity(n);
            for i in 0..n {
                b.append_option(raw(i).and_then(json::as_bool));
            }
            Arc::new(b.finish())
        }
    }
}

fn utf8_arg(v: &ColumnarValue, n: usize) -> Result<StringArray> {
    let arr = match v {
        ColumnarValue::Array(a) => Arc::clone(a),
        ColumnarValue::Scalar(s) => s.to_array_of_size(n)?,
    };
    let arr = if arr.data_type() == &DataType::Utf8 { arr } else { arrow::compute::cast(&arr, &DataType::Utf8)? };
    Ok(arr.as_string::<i32>().clone())
}

fn scalar_str(v: &ColumnarValue) -> Option<Option<&str>> {
    match v {
        ColumnarValue::Scalar(ScalarValue::Utf8(s) | ScalarValue::LargeUtf8(s) | ScalarValue::Utf8View(s)) => {
            Some(s.as_deref())
        }
        ColumnarValue::Scalar(ScalarValue::Null) => Some(None),
        _ => None,
    }
}

/// `f(json_object, key)`.
#[derive(Debug, PartialEq, Eq, Hash)]
pub struct JsonProp {
    name: &'static str,
    view: View,
    signature: Signature,
}

impl JsonProp {
    pub fn new(name: &'static str, view: View) -> Self {
        Self { name, view, signature: Signature::exact(vec![DataType::Utf8, DataType::Utf8], Volatility::Immutable) }
    }
}

impl ScalarUDFImpl for JsonProp {
    fn name(&self) -> &str {
        self.name
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, _: &[DataType]) -> Result<DataType> {
        Ok(self.view.data_type())
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let docs = utf8_arg(&args.args[0], n)?;
        let out = match scalar_str(&args.args[1]) {
            Some(None) => arrow::array::new_null_array(&self.view.data_type(), n),
            Some(Some(key)) => {
                build_view(n, self.view, |i| if docs.is_null(i) { None } else { json::get(docs.value(i), key) })
            }
            None => {
                let keys = utf8_arg(&args.args[1], n)?;
                build_view(n, self.view, |i| {
                    if docs.is_null(i) || keys.is_null(i) {
                        None
                    } else {
                        json::get(docs.value(i), keys.value(i))
                    }
                })
            }
        };
        Ok(ColumnarValue::Array(out))
    }
}

/// `f(person_key, key)` over the query's person-property snapshot
/// (`person.<key>` references; `spec/manifest.md` §4).
#[derive(Debug)]
pub struct PersonProp {
    name: &'static str,
    view: View,
    table: Arc<PersonTable>,
    signature: Signature,
}

impl PersonProp {
    pub fn new(name: &'static str, view: View, table: Arc<PersonTable>) -> Self {
        Self {
            name,
            view,
            table,
            signature: Signature::exact(vec![DataType::Utf8, DataType::Utf8], Volatility::Immutable),
        }
    }
}

impl PartialEq for PersonProp {
    fn eq(&self, other: &Self) -> bool {
        self.name == other.name && self.view == other.view && Arc::ptr_eq(&self.table, &other.table)
    }
}
impl Eq for PersonProp {}
impl Hash for PersonProp {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.name.hash(state);
        self.view.hash(state);
        (Arc::as_ptr(&self.table) as usize).hash(state);
    }
}

impl ScalarUDFImpl for PersonProp {
    fn name(&self) -> &str {
        self.name
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, _: &[DataType]) -> Result<DataType> {
        Ok(self.view.data_type())
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let Some(Some(key)) = scalar_str(&args.args[1]) else {
            return exec_err!("{}: the property name must be a string literal", self.name);
        };
        let persons = utf8_arg(&args.args[0], n)?;
        // Rows arrive grouped by person; remember the previous lookup.
        let mut last: Option<(&str, Option<&str>)> = None;
        let table = self.table.as_ref();
        let out = build_view(n, self.view, |i| {
            if persons.is_null(i) {
                return None;
            }
            let pk = persons.value(i);
            match last {
                Some((p, v)) if p == pk => v,
                _ => {
                    let v = table.get(pk, key);
                    last = Some((pk, v));
                    v
                }
            }
        });
        Ok(ColumnarValue::Array(out))
    }
}

/// Internal one-argument helpers.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Unary {
    /// `la_num(utf8) -> float64`: numeric-looking strings only.
    StrToNum,
    /// `la_fstr(float64) -> utf8`: `str(x)` of a number.
    NumToStr,
    /// `hex(binary) -> utf8`: lowercase hex.
    Hex,
}

#[derive(Debug, PartialEq, Eq, Hash)]
pub struct UnaryFn {
    kind: Unary,
    signature: Signature,
}

impl UnaryFn {
    pub fn new(kind: Unary) -> Self {
        let signature = match kind {
            Unary::StrToNum => Signature::exact(vec![DataType::Utf8], Volatility::Immutable),
            Unary::NumToStr => Signature::exact(vec![DataType::Float64], Volatility::Immutable),
            Unary::Hex => Signature::one_of(
                vec![
                    TypeSignature::Exact(vec![DataType::Binary]),
                    TypeSignature::Exact(vec![DataType::LargeBinary]),
                    TypeSignature::Exact(vec![DataType::BinaryView]),
                    TypeSignature::Any(1),
                ],
                Volatility::Immutable,
            ),
        };
        Self { kind, signature }
    }
}

pub fn hex_array(arr: &ArrayRef) -> Result<ArrayRef> {
    fn push(b: &mut StringBuilder, buf: &mut String, bytes: Option<&[u8]>) {
        match bytes {
            None => b.append_null(),
            Some(v) => {
                buf.clear();
                for x in v {
                    let _ = write!(buf, "{x:02x}");
                }
                b.append_value(&buf);
            }
        }
    }
    let n = arr.len();
    let mut b = StringBuilder::with_capacity(n, n * 32);
    let mut buf = String::with_capacity(64);
    match arr.data_type() {
        DataType::FixedSizeBinary(_) => {
            let a = arr.as_fixed_size_binary();
            for i in 0..n {
                push(&mut b, &mut buf, (!a.is_null(i)).then(|| a.value(i)));
            }
        }
        DataType::Binary => {
            let a = arr.as_binary::<i32>();
            for i in 0..n {
                push(&mut b, &mut buf, (!a.is_null(i)).then(|| a.value(i)));
            }
        }
        DataType::LargeBinary => {
            let a = arr.as_binary::<i64>();
            for i in 0..n {
                push(&mut b, &mut buf, (!a.is_null(i)).then(|| a.value(i)));
            }
        }
        DataType::BinaryView => {
            let a = arr.as_binary_view();
            for i in 0..n {
                push(&mut b, &mut buf, (!a.is_null(i)).then(|| a.value(i)));
            }
        }
        DataType::Null => return Ok(arrow::array::new_null_array(&DataType::Utf8, n)),
        other => return exec_err!("hex() expects a binary argument, got {other}"),
    }
    Ok(Arc::new(b.finish()))
}

impl ScalarUDFImpl for UnaryFn {
    fn name(&self) -> &str {
        match self.kind {
            Unary::StrToNum => "la_num",
            Unary::NumToStr => "la_fstr",
            Unary::Hex => "hex",
        }
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, args: &[DataType]) -> Result<DataType> {
        Ok(match self.kind {
            Unary::StrToNum => DataType::Float64,
            Unary::NumToStr => DataType::Utf8,
            Unary::Hex => {
                match args.first() {
                    Some(
                        DataType::FixedSizeBinary(_)
                        | DataType::Binary
                        | DataType::LargeBinary
                        | DataType::BinaryView
                        | DataType::Null,
                    ) => {}
                    other => return plan_err!("hex() expects a binary argument, got {other:?}"),
                }
                DataType::Utf8
            }
        })
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let input = match &args.args[0] {
            ColumnarValue::Array(a) => Arc::clone(a),
            ColumnarValue::Scalar(s) => s.to_array_of_size(n)?,
        };
        let out: ArrayRef = match self.kind {
            Unary::StrToNum => {
                let a = input.as_string::<i32>();
                let mut b = Float64Builder::with_capacity(n);
                for i in 0..n {
                    b.append_option(if a.is_null(i) { None } else { json::str_to_num(a.value(i)) });
                }
                Arc::new(b.finish())
            }
            Unary::NumToStr => {
                let a: &Float64Array = input.as_primitive();
                let mut b = StringBuilder::with_capacity(n, n * 4);
                for i in 0..n {
                    if a.is_null(i) {
                        b.append_null();
                    } else {
                        b.append_value(json::fmt_f64(a.value(i)));
                    }
                }
                Arc::new(b.finish())
            }
            Unary::Hex => hex_array(&input)?,
        };
        Ok(ColumnarValue::Array(out))
    }
}

/// `la_bucket(ts) -> int32`: index of the time bucket containing `ts`
/// (`None` outside `[starts[0], end)`). Buckets are given by their start
/// instants, ascending.
#[derive(Debug, PartialEq, Eq, Hash)]
pub struct BucketFn {
    starts: Arc<Vec<i64>>,
    end: i64,
    signature: Signature,
}

impl BucketFn {
    pub fn new(starts: Arc<Vec<i64>>, end: i64) -> Self {
        Self {
            starts,
            end,
            signature: Signature::exact(
                vec![DataType::Timestamp(TimeUnit::Millisecond, Some("UTC".into()))],
                Volatility::Immutable,
            ),
        }
    }
}

/// Index of the bucket containing `ts`.
pub fn bucket_of(starts: &[i64], end: i64, ts: i64) -> Option<usize> {
    if starts.is_empty() || ts < starts[0] || ts >= end {
        return None;
    }
    Some(starts.partition_point(|s| *s <= ts) - 1)
}

impl ScalarUDFImpl for BucketFn {
    fn name(&self) -> &str {
        "la_bucket"
    }
    fn signature(&self) -> &Signature {
        &self.signature
    }
    fn return_type(&self, _: &[DataType]) -> Result<DataType> {
        Ok(DataType::Int32)
    }
    fn invoke_with_args(&self, args: ScalarFunctionArgs) -> Result<ColumnarValue> {
        let n = args.number_rows;
        let input = match &args.args[0] {
            ColumnarValue::Array(a) => Arc::clone(a),
            ColumnarValue::Scalar(s) => s.to_array_of_size(n)?,
        };
        let ts = input.as_primitive::<arrow::datatypes::TimestampMillisecondType>();
        let mut b = Int32Builder::with_capacity(n);
        for i in 0..n {
            b.append_option(if ts.is_null(i) {
                None
            } else {
                bucket_of(&self.starts, self.end, ts.value(i)).map(|x| x as i32)
            });
        }
        Ok(ColumnarValue::Array(Arc::new(b.finish())))
    }
}

pub fn prop() -> ScalarUDF {
    ScalarUDF::new_from_impl(JsonProp::new("prop", View::Str))
}
pub fn prop_num() -> ScalarUDF {
    ScalarUDF::new_from_impl(JsonProp::new("prop_num", View::Num))
}
pub fn prop_bool() -> ScalarUDF {
    ScalarUDF::new_from_impl(JsonProp::new("prop_bool", View::Bool))
}
pub fn prop_jnum() -> ScalarUDF {
    ScalarUDF::new_from_impl(JsonProp::new("la_jnum", View::JNum))
}
pub fn hex() -> ScalarUDF {
    ScalarUDF::new_from_impl(UnaryFn::new(Unary::Hex))
}
pub fn str_to_num() -> ScalarUDF {
    ScalarUDF::new_from_impl(UnaryFn::new(Unary::StrToNum))
}
pub fn num_to_str() -> ScalarUDF {
    ScalarUDF::new_from_impl(UnaryFn::new(Unary::NumToStr))
}
pub fn person_prop(view: View, table: Arc<PersonTable>) -> ScalarUDF {
    let name = match view {
        View::Str => "la_person",
        View::Num => "la_person_num",
        View::JNum => "la_person_jnum",
        View::Bool => "la_person_bool",
    };
    ScalarUDF::new_from_impl(PersonProp::new(name, view, table))
}

/// The functions `/q/sql` exposes on top of the built-ins.
pub fn public_udfs() -> Vec<ScalarUDF> {
    vec![prop(), prop_num(), prop_bool(), hex()]
}

#[cfg(test)]
mod tests {
    use super::*;
    use arrow::array::{BooleanArray, FixedSizeBinaryArray, Int32Array};
    use arrow::datatypes::Field;
    use datafusion::config::ConfigOptions;

    fn call(udf: &ScalarUDF, args: Vec<ColumnarValue>, n: usize) -> ArrayRef {
        let types: Vec<DataType> = args.iter().map(|a| a.data_type()).collect();
        let ret = udf.return_type(&types).unwrap();
        let out = udf
            .invoke_with_args(ScalarFunctionArgs {
                arg_fields: types.iter().map(|t| Arc::new(Field::new("a", t.clone(), true))).collect(),
                args,
                number_rows: n,
                return_field: Arc::new(Field::new("r", ret, true)),
                config_options: Arc::new(ConfigOptions::default()),
            })
            .unwrap();
        out.into_array(n).unwrap()
    }

    fn lit(s: &str) -> ColumnarValue {
        ColumnarValue::Scalar(ScalarValue::Utf8(Some(s.into())))
    }

    #[test]
    fn prop_views() {
        let docs: ArrayRef = Arc::new(StringArray::from(vec![
            Some(r#"{"a":"x","n":3.0,"b":true}"#),
            Some(r#"{"a":12,"n":"7.5","b":"true"}"#),
            Some(r#"{"a":null}"#),
            Some("{}"),
            None,
            Some("garbage"),
        ]));
        let d = || ColumnarValue::Array(docs.clone());
        let s = call(&prop(), vec![d(), lit("a")], 6);
        assert_eq!(s.as_string::<i32>().iter().collect::<Vec<_>>(), [Some("x"), Some("12"), None, None, None, None]);
        let n = call(&prop_num(), vec![d(), lit("n")], 6);
        assert_eq!(
            n.as_primitive::<arrow::datatypes::Float64Type>().iter().collect::<Vec<_>>(),
            [Some(3.0), Some(7.5), None, None, None, None]
        );
        let j = call(&prop_jnum(), vec![d(), lit("n")], 6);
        assert_eq!(j.as_primitive::<arrow::datatypes::Float64Type>().iter().collect::<Vec<_>>()[..2], [Some(3.0), None]);
        let b = call(&prop_bool(), vec![d(), lit("b")], 6);
        assert_eq!(b.as_any().downcast_ref::<BooleanArray>().unwrap().iter().collect::<Vec<_>>()[..2], [Some(true), None]);
        // Per-row keys.
        let keys: ArrayRef = Arc::new(StringArray::from(vec![Some("n"), Some("a"), None, Some("a"), Some("a"), Some("a")]));
        let s = call(&prop(), vec![d(), ColumnarValue::Array(keys)], 6);
        assert_eq!(s.as_string::<i32>().iter().collect::<Vec<_>>(), [Some("3"), Some("12"), None, None, None, None]);
    }

    #[test]
    fn hex_and_number_helpers() {
        let ids: ArrayRef = Arc::new(
            FixedSizeBinaryArray::try_from_sparse_iter_with_size(vec![Some(vec![0xabu8, 0x01]), None].into_iter(), 2)
                .unwrap(),
        );
        let h = call(&hex(), vec![ColumnarValue::Array(ids)], 2);
        assert_eq!(h.as_string::<i32>().iter().collect::<Vec<_>>(), [Some("ab01"), None]);
        assert!(hex().return_type(&[DataType::Int64]).is_err());

        let strs: ArrayRef = Arc::new(StringArray::from(vec![Some("12"), Some("1e3"), Some("-0.5"), None]));
        let n = call(&str_to_num(), vec![ColumnarValue::Array(strs)], 4);
        assert_eq!(
            n.as_primitive::<arrow::datatypes::Float64Type>().iter().collect::<Vec<_>>(),
            [Some(12.0), None, Some(-0.5), None]
        );
        let f: ArrayRef = Arc::new(Float64Array::from(vec![Some(3.0), Some(2.5), None]));
        let s = call(&num_to_str(), vec![ColumnarValue::Array(f)], 3);
        assert_eq!(s.as_string::<i32>().iter().collect::<Vec<_>>(), [Some("3"), Some("2.5"), None]);
    }

    #[test]
    fn buckets() {
        let starts = Arc::new(vec![100, 200, 300]);
        assert_eq!(bucket_of(&starts, 400, 99), None);
        assert_eq!(bucket_of(&starts, 400, 100), Some(0));
        assert_eq!(bucket_of(&starts, 400, 199), Some(0));
        assert_eq!(bucket_of(&starts, 400, 200), Some(1));
        assert_eq!(bucket_of(&starts, 400, 399), Some(2));
        assert_eq!(bucket_of(&starts, 400, 400), None);
        let udf = ScalarUDF::new_from_impl(BucketFn::new(starts, 400));
        let ts: ArrayRef = Arc::new(
            arrow::array::TimestampMillisecondArray::from(vec![Some(150), Some(400), None, Some(300)])
                .with_timezone("UTC"),
        );
        let out = call(&udf, vec![ColumnarValue::Array(ts)], 4);
        assert_eq!(out.as_any().downcast_ref::<Int32Array>().unwrap().iter().collect::<Vec<_>>(), [
            Some(0),
            None,
            None,
            Some(2)
        ]);
    }
}
