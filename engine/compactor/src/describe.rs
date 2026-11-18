//! Derives a manifest entry from an events file that already exists.

use arrow::array::{Array, StringArray, TimestampMillisecondArray};
use bytes::Bytes;
use la_core::error::{Error, Result};
use la_core::manifest::FileEntry;
use la_core::parquet_io::{META_IDENTITY_VERSION, META_LEVEL, META_SCHEMA_VERSION};
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ProjectionMask;

/// Reads `data` (a complete events file) and returns the manifest entry that
/// describes it. Used when a deterministic L0 name turns out to be taken: the
/// existing object, not the writer's own attempt, is what gets referenced.
pub fn describe_events_file(key: &str, data: Bytes) -> Result<FileEntry> {
    let bytes = data.len() as u64;
    let builder = ParquetRecordBatchReaderBuilder::try_new(data)?;
    let kv = |name: &str| -> u64 {
        builder
            .metadata()
            .file_metadata()
            .key_value_metadata()
            .and_then(|kv| kv.iter().find(|k| k.key == name))
            .and_then(|k| k.value.as_deref())
            .and_then(|v| v.parse().ok())
            .unwrap_or(0)
    };
    let (schema_version, identity_version, level) =
        (kv(META_SCHEMA_VERSION), kv(META_IDENTITY_VERSION), kv(META_LEVEL) as u8);
    let schema = builder.schema().clone();
    let idx = |n: &str| schema.index_of(n).map_err(|_| Error::invalid(format!("{key}: no column {n}")));
    let mask = ProjectionMask::roots(builder.parquet_schema(), [idx("ts")?, idx("person_key")?]);
    let reader = builder.with_projection(mask).build()?;
    let mut entry = FileEntry {
        key: key.to_owned(),
        level,
        bytes,
        min_ts: i64::MAX,
        max_ts: i64::MIN,
        identity_version,
        schema_version,
        ..Default::default()
    };
    for batch in reader {
        let batch = batch?;
        let n = batch.num_rows();
        if n == 0 {
            continue;
        }
        let bad = || Error::invalid(format!("{key}: unexpected column type"));
        let ts = batch
            .column_by_name("ts")
            .and_then(|c| c.as_any().downcast_ref::<TimestampMillisecondArray>())
            .ok_or_else(bad)?;
        let pk = batch
            .column_by_name("person_key")
            .and_then(|c| c.as_any().downcast_ref::<StringArray>())
            .ok_or_else(bad)?;
        if entry.rows == 0 {
            entry.min_person = pk.value(0).to_owned();
        }
        entry.max_person = pk.value(n - 1).to_owned();
        for v in ts.values().iter() {
            entry.min_ts = entry.min_ts.min(*v);
            entry.max_ts = entry.max_ts.max(*v);
        }
        entry.rows += n as u64;
    }
    if entry.rows == 0 {
        return Err(Error::invalid(format!("{key}: events file has no rows")));
    }
    Ok(entry)
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::events::EventRow;
    use la_core::parquet_io::{write_events_file, EventsFileMeta};

    #[test]
    fn matches_the_entry_produced_by_the_writer() {
        let mut rows: Vec<EventRow> = (0..5000)
            .map(|i| EventRow {
                uuid: (i as u128).to_be_bytes(),
                ts: 1_000 + (i * 37 % 911) as i64,
                recv_ts: 2_000,
                event: "e".into(),
                person_key: format!("u:{:03}", i % 250),
                anon_id: "a".into(),
                props: "{}".into(),
                ..Default::default()
            })
            .collect();
        rows.sort_by(|a, b| a.sort_key_cmp(b));
        let meta =
            EventsFileMeta { project: 3, day: "2026-11-18".into(), level: 0, schema_version: 4, identity_version: 9 };
        let (bytes, mut entry) = write_events_file(&rows, &[], &meta).unwrap();
        entry.key = "data/p=3/d=2026-11-18/L0-1-0-0000000001.parquet".into();
        assert_eq!(describe_events_file(&entry.key, bytes).unwrap(), entry);
        assert!(describe_events_file("k", Bytes::from_static(b"not parquet")).is_err());
    }
}
