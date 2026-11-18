//! Streaming k-way merge of sorted events files at the Arrow level.
//!
//! Pipeline (design §10.1): inputs are read batch by batch and rewritten to
//! the current promoted schema → rows whose `person_key` changes under the
//! current identity map are diverted, re-sorted and merged back in → rows
//! repeating a `uuid` of the same person are dropped → tombstoned rows are
//! dropped → output files are written, split at person boundaries once they
//! reach the target size.
//!
//! Memory is bounded by `inputs × batch` plus the diverted rows (which spill
//! to sorted runs on disk beyond `max_moved_rows`) plus the per-person uuid
//! set; a whole day is never materialized.
//!
//! Why diverting works without a second pass: unresolved rows carry keys
//! `a:…`, resolved ones `u:…`, and `"a:" < "u:"`. Every input therefore
//! yields all of its movable rows before its first `u:` row, so by the time
//! the merge frontier leaves the `a:` range every diverted row is known and
//! the re-sorted rows can join the merge as one more input.

use crate::normalize::Normalizer;
use arrow::array::{
    Array, ArrayRef, BooleanArray, FixedSizeBinaryArray, RecordBatch, StringArray, StringBuilder,
    TimestampMillisecondArray,
};
use arrow::compute::{concat_batches, filter_record_batch, interleave_record_batch, lexsort_to_indices, take};
use arrow::compute::{SortColumn, SortOptions};
use arrow::datatypes::{DataType, SchemaRef, TimeUnit};
use arrow::error::ArrowError;
use arrow::row::{RowConverter, Rows, SortField};
use bytes::Bytes;
use la_core::error::{Error, Result};
use la_core::identity::IdentityMap;
use la_core::manifest::FileEntry;
use la_core::parquet_io::{
    events_writer_props, table_writer_props, META_DAY, META_IDENTITY_VERSION, META_LEVEL, META_PROJECT,
    META_SCHEMA_VERSION,
};
use la_core::schema::PromotedColumn;
use la_core::tombstone::TombstoneSet;
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ArrowWriter;
use std::collections::HashSet;
use std::fs::File;
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub const REPLAY_CHUNK_EVENT: &str = "session_replay_chunk";

/// Keys below this bound are in the "may still be re-keyed" range (`a:…`).
const ANON_RANGE_END: &[u8] = b"a;";

pub enum InputData {
    Mem(Bytes),
    File(File),
    /// Already decoded, sorted batches in the target schema.
    Batches(Vec<RecordBatch>),
}

pub struct MergeInput {
    pub name: String,
    pub data: InputData,
}

pub struct MergeParams<'a> {
    pub project: u32,
    pub day: &'a str,
    pub level: u8,
    pub promoted: &'a [PromotedColumn],
    pub schema_version: u64,
    pub identity: &'a IdentityMap,
    pub tombstones: &'a TombstoneSet,
    /// A file is closed at the next person boundary once it reaches this size.
    pub target_file_bytes: u64,
    pub batch_rows: usize,
    /// Diverted (re-keyed) rows held in memory before spilling a sorted run.
    pub max_moved_rows: usize,
    /// Distinct uuids remembered per person; beyond it only adjacent
    /// duplicates are detected for that person.
    pub max_person_uuids: usize,
    /// Directory for output files and spilled runs.
    pub spool: &'a Path,
}

#[derive(Debug)]
pub struct StagedFile {
    pub path: PathBuf,
    /// Manifest entry with an empty `key` (assigned at commit time).
    pub entry: FileEntry,
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct MergeStats {
    pub rows_in: u64,
    pub rows_out: u64,
    pub duplicates: u64,
    pub tombstoned: u64,
    pub moved: u64,
    pub bytes_out: u64,
}

#[derive(Debug, Default)]
pub struct MergeResult {
    pub files: Vec<StagedFile>,
    pub stats: MergeStats,
    /// `props.key` of every dropped (tombstoned) replay chunk row.
    pub replay_keys: Vec<String>,
}

type BatchIter = Box<dyn Iterator<Item = std::result::Result<RecordBatch, ArrowError>> + Send>;

fn open(data: InputData, batch_rows: usize) -> Result<BatchIter> {
    Ok(match data {
        InputData::Mem(b) => {
            Box::new(ParquetRecordBatchReaderBuilder::try_new(b)?.with_batch_size(batch_rows).build()?)
        }
        InputData::File(f) => {
            Box::new(ParquetRecordBatchReaderBuilder::try_new(f)?.with_batch_size(batch_rows).build()?)
        }
        InputData::Batches(v) => Box::new(v.into_iter().map(Ok)),
    })
}

fn typed<'a, T: 'static>(batch: &'a RecordBatch, name: &str) -> Result<&'a T> {
    batch
        .column_by_name(name)
        .and_then(|c| c.as_any().downcast_ref::<T>())
        .ok_or_else(|| Error::invalid(format!("missing or mistyped column {name}")))
}

/// Typed handles on the columns the merge loop looks at.
struct Cols {
    pk: StringArray,
    ts: TimestampMillisecondArray,
    uuid: FixedSizeBinaryArray,
    anon: StringArray,
    user: StringArray,
    event: StringArray,
    props: StringArray,
}

impl Cols {
    fn of(batch: &RecordBatch) -> Result<Self> {
        Ok(Self {
            pk: typed::<StringArray>(batch, "person_key")?.clone(),
            ts: typed::<TimestampMillisecondArray>(batch, "ts")?.clone(),
            uuid: typed::<FixedSizeBinaryArray>(batch, "uuid")?.clone(),
            anon: typed::<StringArray>(batch, "anon_id")?.clone(),
            user: typed::<StringArray>(batch, "user_id")?.clone(),
            event: typed::<StringArray>(batch, "event")?.clone(),
            props: typed::<StringArray>(batch, "props")?.clone(),
        })
    }
}

struct Cursor {
    iter: BatchIter,
    /// Whether rows of this input may still be re-keyed by the identity map.
    resolve: bool,
    /// Index of the current batch in `Merger::live`.
    batch: usize,
    keys: Rows,
    cols: Cols,
    row: usize,
    len: usize,
}

fn sort_fields() -> Vec<SortField> {
    let ts = DataType::Timestamp(TimeUnit::Millisecond, Some("UTC".into()));
    vec![
        SortField::new(DataType::Utf8),
        SortField::new(ts.clone()),
        SortField::new(DataType::FixedSizeBinary(16)),
        SortField::new(ts),
    ]
}

fn key_columns(batch: &RecordBatch) -> Result<Vec<ArrayRef>> {
    ["person_key", "ts", "uuid", "recv_ts"]
        .iter()
        .map(|n| batch.column_by_name(n).cloned().ok_or_else(|| Error::invalid(format!("missing column {n}"))))
        .collect()
}

/// Sorts batches (same schema) by `(person_key, ts, uuid, recv_ts)` and
/// re-chunks them.
pub fn sort_batches(schema: &SchemaRef, batches: &[RecordBatch], batch_rows: usize) -> Result<Vec<RecordBatch>> {
    if batches.is_empty() {
        return Ok(Vec::new());
    }
    let all = concat_batches(schema, batches)?;
    let opts = Some(SortOptions { descending: false, nulls_first: true });
    let cols: Vec<SortColumn> = key_columns(&all)?.into_iter().map(|values| SortColumn { values, options: opts }).collect();
    let idx = lexsort_to_indices(&cols, None)?;
    let sorted: Vec<ArrayRef> = all.columns().iter().map(|c| take(c, &idx, None)).collect::<std::result::Result<_, _>>()?;
    let sorted = RecordBatch::try_new(schema.clone(), sorted)?;
    let mut out = Vec::new();
    let mut at = 0;
    while at < sorted.num_rows() {
        let n = batch_rows.min(sorted.num_rows() - at);
        out.push(sorted.slice(at, n));
        at += n;
    }
    Ok(out)
}

struct OutFile {
    writer: ArrowWriter<File>,
    path: PathBuf,
    rows: u64,
    min_ts: i64,
    max_ts: i64,
    min_person: String,
    max_person: String,
}

struct Merger<'a> {
    p: &'a MergeParams<'a>,
    norm: Normalizer,
    conv: RowConverter,
    cursors: Vec<Cursor>,
    /// Min-heap of cursor indices ordered by their current row.
    heap: Vec<usize>,
    live: Vec<RecordBatch>,
    pending: Vec<(usize, usize)>,
    moved: Vec<RecordBatch>,
    moved_rows: usize,
    moved_runs: Vec<PathBuf>,
    moved_done: bool,
    out: Option<OutFile>,
    out_seq: usize,
    over_target: bool,
    files: Vec<StagedFile>,
    stats: MergeStats,
    replay_keys: Vec<String>,
    // Per-person dedupe state.
    person: String,
    seen: HashSet<[u8; 16]>,
    last: Option<(i64, [u8; 16])>,
}

fn id16(a: &FixedSizeBinaryArray, i: usize) -> [u8; 16] {
    let mut out = [0u8; 16];
    out.copy_from_slice(a.value(i));
    out
}

impl<'a> Merger<'a> {
    fn new(p: &'a MergeParams<'a>) -> Result<Self> {
        Ok(Self {
            p,
            norm: Normalizer::new(p.promoted),
            conv: RowConverter::new(sort_fields())?,
            cursors: Vec::new(),
            heap: Vec::new(),
            live: Vec::new(),
            pending: Vec::with_capacity(p.batch_rows),
            moved: Vec::new(),
            moved_rows: 0,
            moved_runs: Vec::new(),
            moved_done: false,
            out: None,
            out_seq: 0,
            over_target: false,
            files: Vec::new(),
            stats: MergeStats::default(),
            replay_keys: Vec::new(),
            person: String::new(),
            seen: HashSet::new(),
            last: None,
        })
    }

    /// Splits off the rows the identity map re-keys; returns what stays.
    fn divert_moved(&mut self, batch: RecordBatch) -> Result<Option<RecordBatch>> {
        let pk = typed::<StringArray>(&batch, "person_key")?;
        // Sorted input: once the batch starts beyond the `a:` range nothing can move.
        if pk.is_empty() || pk.value(0).as_bytes() >= ANON_RANGE_END || self.p.identity.is_empty() {
            return Ok(Some(batch));
        }
        let n = batch.num_rows();
        let mut new_keys: Vec<Option<String>> = Vec::with_capacity(n);
        let mut any = false;
        for i in 0..n {
            let nk = pk.value(i).strip_prefix("a:").and_then(|a| self.p.identity.resolve(a)).map(|u| format!("u:{u}"));
            any |= nk.is_some();
            new_keys.push(nk);
        }
        if !any {
            return Ok(Some(batch));
        }
        let mask = BooleanArray::from(new_keys.iter().map(|k| k.is_some()).collect::<Vec<bool>>());
        let moved = filter_record_batch(&batch, &mask)?;
        let mut b = StringBuilder::new();
        for k in new_keys.iter().flatten() {
            b.append_value(k);
        }
        let pk_idx = batch.schema().index_of("person_key")?;
        let mut cols = moved.columns().to_vec();
        cols[pk_idx] = Arc::new(b.finish());
        let moved = RecordBatch::try_new(batch.schema(), cols)?;
        self.moved_rows += moved.num_rows();
        self.stats.moved += moved.num_rows() as u64;
        self.moved.push(moved);
        if self.moved_rows >= self.p.max_moved_rows {
            self.spill_moved()?;
        }
        let keep = BooleanArray::from(new_keys.iter().map(|k| k.is_none()).collect::<Vec<bool>>());
        let rest = filter_record_batch(&batch, &keep)?;
        Ok((rest.num_rows() > 0).then_some(rest))
    }

    fn spill_moved(&mut self) -> Result<()> {
        let schema = self.norm.schema().clone();
        let sorted = sort_batches(&schema, &std::mem::take(&mut self.moved), self.p.batch_rows)?;
        self.moved_rows = 0;
        if sorted.is_empty() {
            return Ok(());
        }
        let path = self.p.spool.join(format!("moved-{:04}.parquet", self.moved_runs.len()));
        let mut w = ArrowWriter::try_new(File::create(&path)?, schema, Some(table_writer_props()?))?;
        for b in &sorted {
            w.write(b)?;
        }
        w.close()?;
        self.moved_runs.push(path);
        Ok(())
    }

    /// Loads the next non-empty batch of cursor `ci`. `false` = exhausted.
    fn load(&mut self, ci: usize) -> Result<bool> {
        loop {
            let Some(next) = self.cursors[ci].iter.next() else {
                return Ok(false);
            };
            let raw = next?;
            if raw.num_rows() == 0 {
                continue;
            }
            let batch = if self.cursors[ci].resolve {
                self.stats.rows_in += raw.num_rows() as u64;
                let b = self.norm.normalize(&raw)?;
                match self.divert_moved(b)? {
                    Some(b) => b,
                    None => continue,
                }
            } else {
                raw
            };
            let keys = self.conv.convert_columns(&key_columns(&batch)?)?;
            let c = &mut self.cursors[ci];
            c.cols = Cols::of(&batch)?;
            c.keys = keys;
            c.row = 0;
            c.len = batch.num_rows();
            c.batch = self.live.len();
            self.live.push(batch);
            return Ok(true);
        }
    }

    fn add_input(&mut self, data: InputData, resolve: bool) -> Result<()> {
        let iter = open(data, self.p.batch_rows)?;
        let empty = RecordBatch::new_empty(self.norm.schema().clone());
        let ci = self.cursors.len();
        self.cursors.push(Cursor {
            iter,
            resolve,
            batch: 0,
            keys: self.conv.empty_rows(0, 0),
            cols: Cols::of(&empty)?,
            row: 0,
            len: 0,
        });
        if self.load(ci)? {
            self.heap.push(ci);
            self.sift_up(self.heap.len() - 1);
        }
        Ok(())
    }

    fn less(&self, a: usize, b: usize) -> bool {
        let (ca, cb) = (&self.cursors[a], &self.cursors[b]);
        match ca.keys.row(ca.row).cmp(&cb.keys.row(cb.row)) {
            std::cmp::Ordering::Less => true,
            std::cmp::Ordering::Greater => false,
            std::cmp::Ordering::Equal => a < b,
        }
    }

    fn sift_up(&mut self, mut i: usize) {
        while i > 0 {
            let parent = (i - 1) / 2;
            if self.less(self.heap[i], self.heap[parent]) {
                self.heap.swap(i, parent);
                i = parent;
            } else {
                break;
            }
        }
    }

    fn sift_down(&mut self, mut i: usize) {
        let n = self.heap.len();
        loop {
            let (l, r) = (2 * i + 1, 2 * i + 2);
            let mut m = i;
            if l < n && self.less(self.heap[l], self.heap[m]) {
                m = l;
            }
            if r < n && self.less(self.heap[r], self.heap[m]) {
                m = r;
            }
            if m == i {
                return;
            }
            self.heap.swap(i, m);
            i = m;
        }
    }

    /// Adds the re-sorted diverted rows as new merge inputs.
    fn finalize_moved(&mut self) -> Result<()> {
        self.moved_done = true;
        let schema = self.norm.schema().clone();
        let sorted = sort_batches(&schema, &std::mem::take(&mut self.moved), self.p.batch_rows)?;
        self.moved_rows = 0;
        if !sorted.is_empty() {
            self.add_input(InputData::Batches(sorted), false)?;
        }
        for path in std::mem::take(&mut self.moved_runs) {
            self.add_input(InputData::File(File::open(path)?), false)?;
        }
        Ok(())
    }

    fn open_out(&mut self) -> Result<()> {
        let path = self.p.spool.join(format!("out-{:04}.parquet", self.out_seq));
        self.out_seq += 1;
        let schema = self.norm.schema().clone();
        let props = events_writer_props(
            &schema,
            vec![
                (META_SCHEMA_VERSION.into(), self.p.schema_version.to_string()),
                (META_IDENTITY_VERSION.into(), self.p.identity.version().to_string()),
                (META_LEVEL.into(), self.p.level.to_string()),
                (META_PROJECT.into(), self.p.project.to_string()),
                (META_DAY.into(), self.p.day.to_owned()),
            ],
        )?;
        let writer = ArrowWriter::try_new(File::create(&path)?, schema, Some(props))?;
        self.out = Some(OutFile {
            writer,
            path,
            rows: 0,
            min_ts: i64::MAX,
            max_ts: i64::MIN,
            min_person: String::new(),
            max_person: String::new(),
        });
        self.over_target = false;
        Ok(())
    }

    /// Writes the pending rows and forgets batches no cursor needs any more.
    fn flush(&mut self) -> Result<()> {
        if !self.pending.is_empty() {
            let refs: Vec<&RecordBatch> = self.live.iter().collect();
            let batch = interleave_record_batch(&refs, &self.pending)?;
            self.pending.clear();
            let out = self.out.as_mut().ok_or_else(|| Error::invalid("flush without an open output"))?;
            out.writer.write(&batch)?;
            let size = out.writer.bytes_written() + out.writer.in_progress_size();
            self.over_target = size as u64 >= self.p.target_file_bytes;
        }
        let mut live = Vec::with_capacity(self.heap.len());
        for &ci in &self.heap {
            let c = &mut self.cursors[ci];
            live.push(self.live[c.batch].clone());
            c.batch = live.len() - 1;
        }
        self.live = live;
        Ok(())
    }

    fn close_out(&mut self) -> Result<()> {
        self.flush()?;
        let Some(out) = self.out.take() else {
            return Ok(());
        };
        out.writer.close()?;
        let bytes = std::fs::metadata(&out.path)?.len();
        self.stats.bytes_out += bytes;
        self.files.push(StagedFile {
            path: out.path,
            entry: FileEntry {
                key: String::new(),
                level: self.p.level,
                rows: out.rows,
                bytes,
                min_ts: out.min_ts,
                max_ts: out.max_ts,
                min_person: out.min_person,
                max_person: out.max_person,
                identity_version: self.p.identity.version(),
                schema_version: self.p.schema_version,
            },
        });
        Ok(())
    }

    fn run(mut self, inputs: Vec<MergeInput>) -> Result<MergeResult> {
        for input in inputs {
            self.add_input(input.data, true).map_err(|e| Error::invalid(format!("{}: {e}", input.name)))?;
        }
        loop {
            let Some(&top) = self.heap.first() else {
                if self.moved_done {
                    break;
                }
                self.finalize_moved()?;
                continue;
            };
            let row = self.cursors[top].row;
            if !self.moved_done && self.cursors[top].cols.pk.value(row).as_bytes() >= ANON_RANGE_END {
                self.finalize_moved()?;
                continue;
            }
            let c = &self.cursors[top];
            let pk = c.cols.pk.value(row);
            let ts = c.cols.ts.value(row);
            let uuid = id16(&c.cols.uuid, row);
            let new_person = pk != self.person;
            if new_person {
                // File boundaries only ever fall between two people.
                if self.over_target && self.out.is_some() {
                    self.close_out()?;
                }
                let c = &self.cursors[top];
                self.person.clear();
                self.person.push_str(c.cols.pk.value(row));
                if self.seen.capacity() > 4096 {
                    self.seen = HashSet::new();
                } else {
                    self.seen.clear();
                }
                self.last = None;
            }
            let c = &self.cursors[top];
            let user = (!c.cols.user.is_null(row)).then(|| c.cols.user.value(row));
            let duplicate = self.last == Some((ts, uuid))
                || if self.seen.len() < self.p.max_person_uuids {
                    !self.seen.insert(uuid)
                } else {
                    self.seen.contains(&uuid)
                };
            if duplicate {
                self.stats.duplicates += 1;
            } else if self.p.tombstones.matches(&self.person, c.cols.anon.value(row), user) {
                self.stats.tombstoned += 1;
                if c.cols.event.value(row) == REPLAY_CHUNK_EVENT {
                    if let Some(serde_json::Value::String(k)) = crate::json::object_member(c.cols.props.value(row), "key")
                    {
                        self.replay_keys.push(k);
                    }
                }
            } else {
                if self.out.is_none() {
                    self.open_out()?;
                }
                let out = self.out.as_mut().ok_or_else(|| Error::invalid("no open output"))?;
                if out.rows == 0 {
                    out.min_person.clone_from(&self.person);
                }
                if new_person || out.rows == 0 {
                    out.max_person.clone_from(&self.person);
                }
                out.rows += 1;
                out.min_ts = out.min_ts.min(ts);
                out.max_ts = out.max_ts.max(ts);
                self.stats.rows_out += 1;
                // Read the batch slot only now: closing a file above re-numbers the live batches.
                self.pending.push((self.cursors[top].batch, row));
            }
            self.last = Some((ts, uuid));

            // Advance the cursor that supplied the row.
            self.cursors[top].row += 1;
            if self.cursors[top].row == self.cursors[top].len {
                if self.load(top)? {
                    self.sift_down(0);
                } else {
                    let lastpos = self.heap.len() - 1;
                    self.heap.swap(0, lastpos);
                    self.heap.pop();
                    if !self.heap.is_empty() {
                        self.sift_down(0);
                    }
                }
            } else {
                self.sift_down(0);
            }
            if self.pending.len() >= self.p.batch_rows || self.live.len() > 4 * self.cursors.len() + 16 {
                self.flush()?;
            }
        }
        self.close_out()?;
        Ok(MergeResult { files: self.files, stats: self.stats, replay_keys: self.replay_keys })
    }
}

/// Merges `inputs` (each sorted by `(person_key, ts, uuid)`) into staged
/// output files under `params.spool`. CPU-bound and synchronous: call it from
/// `spawn_blocking`.
pub fn merge_inputs(inputs: Vec<MergeInput>, params: &MergeParams<'_>) -> Result<MergeResult> {
    Merger::new(params)?.run(inputs)
}

#[cfg(test)]
mod tests {
    use super::*;
    use la_core::events::{batch_to_rows, Cell, EventRow};
    use la_core::identity::{append_delta, Candidate};
    use la_core::parquet_io::{read_parquet, write_events_file, EventsFileMeta};
    use la_core::schema::PropType;
    use la_core::tombstone::Tombstone;
    use la_core::Store;

    fn row(person: &str, anon: &str, ts: i64, id: u16) -> EventRow {
        let mut uuid = [0u8; 16];
        uuid[..2].copy_from_slice(&id.to_be_bytes());
        EventRow {
            uuid,
            ts,
            recv_ts: ts + 1,
            event: "e".into(),
            person_key: person.into(),
            anon_id: anon.into(),
            user_id: person.strip_prefix("u:").map(str::to_owned),
            props: format!("{{\"id\":{id}}}"),
            ..Default::default()
        }
    }

    fn file(rows: &mut Vec<EventRow>, cols: &[PromotedColumn]) -> MergeInput {
        la_core::events::sort_and_dedupe(rows);
        let meta =
            EventsFileMeta { project: 1, day: "2026-11-18".into(), level: 0, schema_version: 0, identity_version: 0 };
        let (bytes, _) = write_events_file(rows, cols, &meta).unwrap();
        MergeInput { name: "test".into(), data: InputData::Mem(bytes) }
    }

    fn params<'a>(
        dir: &'a Path,
        identity: &'a IdentityMap,
        tombs: &'a TombstoneSet,
        cols: &'a [PromotedColumn],
    ) -> MergeParams<'a> {
        MergeParams {
            project: 1,
            day: "2026-11-18",
            level: 1,
            promoted: cols,
            schema_version: 3,
            identity,
            tombstones: tombs,
            target_file_bytes: u64::MAX,
            batch_rows: 7,
            max_moved_rows: 1_000_000,
            max_person_uuids: 1_000_000,
            spool: dir,
        }
    }

    fn read_all(res: &MergeResult, cols: &[PromotedColumn]) -> Vec<Vec<EventRow>> {
        res.files
            .iter()
            .map(|f| {
                let data = Bytes::from(std::fs::read(&f.path).unwrap());
                let mut rows = Vec::new();
                for b in read_parquet(data).unwrap() {
                    rows.extend(batch_to_rows(&b, cols).unwrap());
                }
                rows
            })
            .collect()
    }

    fn keys(rows: &[EventRow]) -> Vec<(String, i64, u16)> {
        rows.iter().map(|r| (r.person_key.clone(), r.ts, u16::from_be_bytes([r.uuid[0], r.uuid[1]]))).collect()
    }

    #[test]
    fn merges_sorted_inputs_and_drops_duplicate_uuids() {
        let dir = tempfile::tempdir().unwrap();
        let mut a: Vec<EventRow> = (0..40).map(|i| row(&format!("u:{:02}", i % 10), "x", 100 + i, i as u16)).collect();
        let mut b: Vec<EventRow> = (20..70).map(|i| row(&format!("u:{:02}", i % 10), "x", 100 + i, i as u16)).collect();
        // Same uuid, later timestamp, other file: still a duplicate of the same person.
        b.push(row("u:03", "x", 999, 3));
        // Same uuid under another person is a different event.
        b.push(row("u:zz", "x", 5, 3));
        let (id, tombs) = (IdentityMap::default(), TombstoneSet::default());
        let p = params(dir.path(), &id, &tombs, &[]);
        let res = merge_inputs(vec![file(&mut a, &[]), file(&mut b, &[])], &p).unwrap();
        assert_eq!(res.files.len(), 1);
        let rows = read_all(&res, &[]).remove(0);
        assert_eq!(rows.len(), 71);
        assert!(rows.windows(2).all(|w| w[0].sort_key_cmp(&w[1]).is_lt()));
        assert_eq!(res.stats, MergeStats { rows_in: 92, rows_out: 71, duplicates: 21, bytes_out: res.stats.bytes_out, ..Default::default() });
        let e = &res.files[0].entry;
        assert_eq!((e.rows, e.level, e.schema_version), (71, 1, 3));
        assert_eq!((e.min_person.as_str(), e.max_person.as_str()), ("u:00", "u:zz"));
        assert_eq!((e.min_ts, e.max_ts), (5, 169));
        assert_eq!(e.bytes, std::fs::metadata(&res.files[0].path).unwrap().len());
    }

    #[tokio::test]
    async fn rekeyed_rows_are_resorted_into_place() {
        let s = Store::memory();
        let mut id = IdentityMap::default();
        append_delta(
            &s,
            1,
            "1-0",
            1,
            vec![
                Candidate { anon_id: "d1".into(), user_id: "m".into(), ts: 1, uuid: [1; 16] },
                Candidate { anon_id: "d3".into(), user_id: "b".into(), ts: 1, uuid: [2; 16] },
            ],
            &mut id,
        )
        .await
        .unwrap();
        let dir = tempfile::tempdir().unwrap();
        let mut a = vec![
            row("a:d1", "d1", 10, 1),
            row("a:d1", "d1", 30, 2),
            row("a:d2", "d2", 5, 3),
            row("u:a", "x", 1, 4),
            row("u:m", "d1", 20, 5),
            row("u:z", "x", 1, 6),
        ];
        let mut b = vec![row("a:d3", "d3", 7, 7), row("a:d1", "d1", 30, 2), row("u:m", "d1", 40, 8)];
        let tombs = TombstoneSet::default();
        for max_moved in [1_000_000, 1] {
            let sub = tempfile::tempdir_in(dir.path()).unwrap();
            let mut p = params(sub.path(), &id, &tombs, &[]);
            p.max_moved_rows = max_moved; // 1 = every diverted batch spills a run to disk
            let res = merge_inputs(vec![file(&mut a, &[]), file(&mut b, &[])], &p).unwrap();
            let rows = read_all(&res, &[]).remove(0);
            assert_eq!(
                keys(&rows),
                [
                    ("a:d2".to_owned(), 5, 3),
                    ("u:a".to_owned(), 1, 4),
                    ("u:b".to_owned(), 7, 7),
                    ("u:m".to_owned(), 10, 1),
                    ("u:m".to_owned(), 20, 5),
                    ("u:m".to_owned(), 30, 2),
                    ("u:m".to_owned(), 40, 8),
                    ("u:z".to_owned(), 1, 6),
                ],
                "max_moved={max_moved}"
            );
            assert_eq!((res.stats.moved, res.stats.duplicates), (4, 1));
            assert_eq!(res.files[0].entry.identity_version, 1);
            // The original identifiers of a re-keyed row are untouched.
            assert_eq!((rows[3].anon_id.as_str(), rows[3].user_id.as_deref()), ("d1", None));
        }
    }

    #[test]
    fn only_anonymous_rows_still_get_rekeyed() {
        // No `u:` row ever reaches the top of the heap: the diverted rows must
        // be merged when the regular inputs run dry.
        let rt = tokio::runtime::Builder::new_current_thread().build().unwrap();
        let s = Store::memory();
        let mut id = IdentityMap::default();
        rt.block_on(append_delta(
            &s,
            1,
            "1-0",
            1,
            vec![Candidate { anon_id: "d1".into(), user_id: "m".into(), ts: 1, uuid: [1; 16] }],
            &mut id,
        ))
        .unwrap();
        let dir = tempfile::tempdir().unwrap();
        let mut a = vec![row("a:d1", "d1", 10, 1), row("a:d0", "d0", 3, 2)];
        let tombs = TombstoneSet::default();
        let p = params(dir.path(), &id, &tombs, &[]);
        let res = merge_inputs(vec![file(&mut a, &[])], &p).unwrap();
        assert_eq!(keys(&read_all(&res, &[])[0]), [("a:d0".to_owned(), 3, 2), ("u:m".to_owned(), 10, 1)]);
    }

    #[test]
    fn tombstones_drop_rows_and_report_replay_objects() {
        let dir = tempfile::tempdir().unwrap();
        let mut chunk = row("u:gone", "dev", 50, 9);
        chunk.event = REPLAY_CHUNK_EVENT.into();
        chunk.props = r#"{"bytes":10,"key":"replay/p=1/d=2026-11-18/aa/000000.bin.zst","seq":0}"#.into();
        let mut a = vec![
            row("u:gone", "dev", 1, 1),
            chunk,
            row("a:dev", "dev", 2, 2),
            row("u:other", "dev", 3, 3), // shared device: caught through anon_id
            row("u:keep", "k", 4, 4),
        ];
        let id = IdentityMap::default();
        let tombs = TombstoneSet::build(
            &[Tombstone { id: "t".into(), ts_ms: 5, person_keys: vec!["u:gone".into()], anon_ids: vec!["dev".into()] }],
            &id,
            0,
        );
        let p = params(dir.path(), &id, &tombs, &[]);
        let res = merge_inputs(vec![file(&mut a, &[])], &p).unwrap();
        assert_eq!(keys(&read_all(&res, &[])[0]), [("u:keep".to_owned(), 4, 4)]);
        assert_eq!(res.stats.tombstoned, 4);
        assert_eq!(res.replay_keys, ["replay/p=1/d=2026-11-18/aa/000000.bin.zst"]);
        // Everything deleted → no output file at all.
        let mut only = vec![row("u:gone", "dev", 1, 1)];
        let sub = tempfile::tempdir().unwrap();
        let p = params(sub.path(), &id, &tombs, &[]);
        assert!(merge_inputs(vec![file(&mut only, &[])], &p).unwrap().files.is_empty());
    }

    #[test]
    fn output_splits_at_person_boundaries_and_promotes() {
        let dir = tempfile::tempdir().unwrap();
        let cols = vec![PromotedColumn { key: "id".into(), ty: PropType::Int, since: 3 }];
        let mut a: Vec<EventRow> =
            (0..3000u16).map(|i| row(&format!("u:{:03}", i % 300), "x", 1000 + i as i64, i)).collect();
        let (id, tombs) = (IdentityMap::default(), TombstoneSet::default());
        let mut p = params(dir.path(), &id, &tombs, &cols);
        p.target_file_bytes = 1; // rotate at every person boundary after a flush
        p.batch_rows = 256;
        let res = merge_inputs(vec![file(&mut a, &[])], &p).unwrap();
        assert!(res.files.len() > 3, "{} files", res.files.len());
        let parts = read_all(&res, &cols);
        let mut total = 0;
        for (i, (rows, f)) in parts.iter().zip(&res.files).enumerate() {
            total += rows.len();
            assert_eq!(f.entry.rows as usize, rows.len());
            assert_eq!(f.entry.min_person, rows[0].person_key);
            assert_eq!(f.entry.max_person, rows[rows.len() - 1].person_key);
            if i > 0 {
                assert!(res.files[i - 1].entry.max_person < f.entry.min_person, "person ranges are disjoint");
            }
            // The value moved from props into the promoted column.
            assert!(rows.iter().all(|r| matches!(r.promoted[0], Some(Cell::Int(_))) && r.props == "{}"));
        }
        assert_eq!(total, 3000);
        // Rotating files must not disturb which rows are written.
        let mut seen: Vec<u16> = parts.iter().flatten().map(|r| u16::from_be_bytes([r.uuid[0], r.uuid[1]])).collect();
        seen.sort_unstable();
        assert_eq!(seen, (0..3000u16).collect::<Vec<_>>());
        for rows in &parts {
            assert!(rows.windows(2).all(|w| w[0].sort_key_cmp(&w[1]).is_lt()));
        }
    }
}
