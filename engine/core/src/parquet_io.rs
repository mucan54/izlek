//! Parquet read/write helpers with the writer settings from `spec/parquet.md` §1.

use crate::error::{Error, Result};
use crate::events::{rows_to_batch, EventRow};
use crate::manifest::FileEntry;
use crate::schema::PromotedColumn;
use arrow::datatypes::SchemaRef;
use arrow::record_batch::RecordBatch;
use bytes::Bytes;
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ArrowWriter;
use parquet::basic::{Compression, ZstdLevel};
use parquet::file::metadata::{KeyValue, SortingColumn};
use parquet::file::properties::{EnabledStatistics, WriterProperties, WriterPropertiesBuilder};
use parquet::schema::types::ColumnPath;

pub const MAX_ROW_GROUP_ROWS: usize = 100_000;
pub const DATA_PAGE_BYTES: usize = 1 << 20;
pub const WRITE_BATCH_ROWS: usize = 8_192;

pub const META_SCHEMA_VERSION: &str = "la.schema_version";
pub const META_IDENTITY_VERSION: &str = "la.identity_version";
pub const META_LEVEL: &str = "la.level";
pub const META_PROJECT: &str = "la.project";
pub const META_DAY: &str = "la.day";

fn base_props() -> Result<WriterPropertiesBuilder> {
    Ok(WriterProperties::builder()
        .set_compression(Compression::ZSTD(ZstdLevel::try_new(3)?))
        .set_max_row_group_row_count(Some(MAX_ROW_GROUP_ROWS))
        .set_data_page_size_limit(DATA_PAGE_BYTES)
        .set_statistics_enabled(EnabledStatistics::Page)
        .set_dictionary_enabled(true)
        .set_created_by("la-engine".to_owned()))
}

/// Writer properties for an events file: zstd(3), ≤100k-row groups, page
/// statistics + page index, dictionary everywhere except `uuid`, and the
/// declared `(person_key, ts, uuid)` sort order.
pub fn events_writer_props(schema: &SchemaRef, kv: Vec<(String, String)>) -> Result<WriterProperties> {
    let idx = |name: &str| -> Result<i32> {
        schema.index_of(name).map(|i| i as i32).map_err(|_| Error::invalid(format!("schema lacks {name}")))
    };
    let sorting = ["person_key", "ts", "uuid"]
        .iter()
        .map(|c| Ok(SortingColumn { column_idx: idx(c)?, descending: false, nulls_first: false }))
        .collect::<Result<Vec<_>>>()?;
    Ok(base_props()?
        .set_column_dictionary_enabled(ColumnPath::from("uuid"), false)
        .set_sorting_columns(Some(sorting))
        .set_key_value_metadata(Some(kv.into_iter().map(|(k, v)| KeyValue::new(k, v)).collect()))
        .build())
}

/// Writer properties for the small side tables (identity map, person props).
pub fn table_writer_props() -> Result<WriterProperties> {
    Ok(base_props()?.build())
}

/// Serializes `batches` (all in `schema`) into an in-memory Parquet file.
pub fn write_parquet(schema: &SchemaRef, batches: &[RecordBatch], props: WriterProperties) -> Result<Bytes> {
    let mut buf: Vec<u8> = Vec::new();
    let mut w = ArrowWriter::try_new(&mut buf, schema.clone(), Some(props))?;
    for b in batches {
        w.write(b)?;
    }
    w.close()?;
    Ok(Bytes::from(buf))
}

/// Decodes a whole in-memory Parquet file.
pub fn read_parquet(data: Bytes) -> Result<Vec<RecordBatch>> {
    let reader = ParquetRecordBatchReaderBuilder::try_new(data)?.with_batch_size(WRITE_BATCH_ROWS).build()?;
    let mut out = Vec::new();
    for b in reader {
        out.push(b?);
    }
    Ok(out)
}

/// File-level key-value metadata of an in-memory Parquet file.
pub fn read_kv_metadata(data: Bytes) -> Result<Vec<(String, String)>> {
    let b = ParquetRecordBatchReaderBuilder::try_new(data)?;
    Ok(b.metadata()
        .file_metadata()
        .key_value_metadata()
        .map(|kv| kv.iter().filter_map(|k| Some((k.key.clone(), k.value.clone()?))).collect())
        .unwrap_or_default())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EventsFileMeta {
    pub project: u32,
    pub day: String,
    pub level: u8,
    pub schema_version: u64,
    pub identity_version: u64,
}

/// Encodes rows that are **already in file order** into an events file and
/// returns it together with its manifest entry (the `key` is left for the
/// caller to fill in).
pub fn write_events_file(
    rows: &[EventRow],
    promoted: &[PromotedColumn],
    meta: &EventsFileMeta,
) -> Result<(Bytes, FileEntry)> {
    if rows.is_empty() {
        return Err(Error::invalid("refusing to write an empty events file"));
    }
    debug_assert!(rows.windows(2).all(|w| w[0].sort_key_cmp(&w[1]).is_le()), "rows must be sorted");
    let schema = crate::schema::events_schema(promoted);
    let props = events_writer_props(
        &schema,
        vec![
            (META_SCHEMA_VERSION.into(), meta.schema_version.to_string()),
            (META_IDENTITY_VERSION.into(), meta.identity_version.to_string()),
            (META_LEVEL.into(), meta.level.to_string()),
            (META_PROJECT.into(), meta.project.to_string()),
            (META_DAY.into(), meta.day.clone()),
        ],
    )?;
    let mut buf: Vec<u8> = Vec::new();
    let mut w = ArrowWriter::try_new(&mut buf, schema, Some(props))?;
    for chunk in rows.chunks(WRITE_BATCH_ROWS) {
        w.write(&rows_to_batch(chunk, promoted)?)?;
    }
    w.close()?;
    let entry = FileEntry {
        key: String::new(),
        level: meta.level,
        rows: rows.len() as u64,
        bytes: buf.len() as u64,
        min_ts: rows.iter().map(|r| r.ts).min().unwrap_or(0),
        max_ts: rows.iter().map(|r| r.ts).max().unwrap_or(0),
        min_person: rows[0].person_key.clone(),
        max_person: rows[rows.len() - 1].person_key.clone(),
        identity_version: meta.identity_version,
        schema_version: meta.schema_version,
    };
    Ok((Bytes::from(buf), entry))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::events::{batch_to_rows, Cell};
    use crate::schema::{PropType, DIM_PLATFORM};

    fn rows(n: usize) -> Vec<EventRow> {
        let mut v: Vec<EventRow> = (0..n)
            .map(|i| {
                let mut r = EventRow {
                    uuid: (i as u128).to_be_bytes(),
                    ts: 1_795_046_400_000 + (i as i64 % 97) * 1000,
                    recv_ts: 1_795_046_400_000 + i as i64,
                    event: if i % 3 == 0 { "purchase".into() } else { "pageview".into() },
                    person_key: format!("{}:{:04}", if i % 5 == 0 { "a" } else { "u" }, i % 50),
                    anon_id: format!("anon-{}", i % 50),
                    props: format!("{{\"i\":{i}}}"),
                    promoted: vec![Some(Cell::Str(if i % 2 == 0 { "pro".into() } else { "free".into() }))],
                    ..Default::default()
                };
                r.dims[DIM_PLATFORM] = Some("web".into());
                r
            })
            .collect();
        v.sort_by(|a, b| a.sort_key_cmp(b));
        v
    }

    #[test]
    fn events_file_roundtrip_and_metadata() {
        let cols = vec![PromotedColumn { key: "plan".into(), ty: PropType::String, since: 3 }];
        let meta = EventsFileMeta {
            project: 1017,
            day: "2026-11-18".into(),
            level: 0,
            schema_version: 3,
            identity_version: 7,
        };
        let input = rows(20_000);
        let (bytes, entry) = write_events_file(&input, &cols, &meta).unwrap();
        assert_eq!(entry.rows, 20_000);
        assert_eq!(entry.bytes, bytes.len() as u64);
        assert_eq!((entry.min_person.as_str(), entry.max_person.as_str()), ("a:0000", "u:0049"));
        assert_eq!((entry.identity_version, entry.schema_version, entry.level), (7, 3, 0));
        assert!(entry.min_ts <= entry.max_ts);

        let kv = read_kv_metadata(bytes.clone()).unwrap();
        assert!(kv.contains(&(META_IDENTITY_VERSION.to_owned(), "7".to_owned())));
        assert!(kv.contains(&(META_DAY.to_owned(), "2026-11-18".to_owned())));

        let opts = parquet::arrow::arrow_reader::ArrowReaderOptions::new()
            .with_page_index_policy(parquet::file::metadata::PageIndexPolicy::Required);
        let b = ParquetRecordBatchReaderBuilder::try_new_with_options(bytes.clone(), opts).unwrap();
        let rg = b.metadata().row_group(0);
        let sorting = rg.sorting_columns().expect("sorting columns declared");
        assert_eq!(sorting.iter().map(|s| s.column_idx).collect::<Vec<_>>(), [4, 1, 0]);
        assert!(b.metadata().column_index().is_some(), "page index written");
        assert!(rg.column(4).statistics().is_some(), "person_key chunk statistics");

        let mut back = Vec::new();
        for batch in read_parquet(bytes).unwrap() {
            back.extend(batch_to_rows(&batch, &cols).unwrap());
        }
        assert_eq!(back, input);
    }

    #[test]
    fn large_files_split_into_row_groups() {
        let input = rows(MAX_ROW_GROUP_ROWS + 10);
        let meta =
            EventsFileMeta { project: 1, day: "2026-11-18".into(), level: 2, schema_version: 0, identity_version: 0 };
        let cols = vec![PromotedColumn { key: "plan".into(), ty: PropType::String, since: 1 }];
        let (bytes, _) = write_events_file(&input, &cols, &meta).unwrap();
        let b = ParquetRecordBatchReaderBuilder::try_new(bytes).unwrap();
        assert_eq!(b.metadata().num_row_groups(), 2);
        assert_eq!(b.metadata().row_group(0).num_rows() as usize, MAX_ROW_GROUP_ROWS);
    }

    #[test]
    fn empty_file_is_refused() {
        let meta =
            EventsFileMeta { project: 1, day: "2026-11-18".into(), level: 0, schema_version: 0, identity_version: 0 };
        assert!(write_events_file(&[], &[], &meta).is_err());
    }
}
