//! Person deletion (`docs/design.md` §10.5, `spec/manifest.md` §9).
//!
//! For every day whose `tombstone_ts` lags behind the newest tombstone the
//! files containing matching rows are rewritten (L0/L1 files together into
//! one L1 run, each L2 file on its own), the replay objects referenced by the
//! deleted `session_replay_chunk` rows are removed, and `tombstone_ts` is
//! advanced once **every** listed file is known to be clean. When all days are
//! done the person's rows in `persons/` and `identity/` are removed.
//!
//! Deleting identity links would make later expansions of `u:U` forget the
//! anonymous ids that belonged to `U` (late or rebuilt data must still be
//! filtered for 400 days), so before any link is removed the expansion is
//! frozen into an extra tombstone object `<ts>-<id>-x.json` listing those ids.

use crate::config::Ctx;
use crate::job::{
    commit, fetch_input, stage, CommitPlan, Group, Outcome, ProjectEnv, Replace, Staged, TombstoneAdvance,
};
use crate::merge::InputData;
use arrow::array::{Array, StringArray};
use la_core::error::{Error, Result};
use la_core::manifest::{self, FileEntry};
use la_core::time::DAY_MS;
use la_core::tombstone::{tombstone_key, Tombstone, TombstoneSet};
use la_core::{identity, keys, persons};
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ProjectionMask;
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::sync::Arc;

pub const EXPANSION_SUFFIX: &str = "-x";

/// `tombstones/p=<pid>/_applied.state`
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct TombstoneState {
    /// Newest tombstone whose identity/persons cleanup is complete.
    #[serde(default)]
    pub side_ts: i64,
}

pub async fn load_state(ctx: &Ctx, project: u32) -> Result<TombstoneState> {
    Ok(ctx.store.get_json(&keys::tombstone_state(project)).await?.unwrap_or_default())
}

/// Removes replay objects named by deleted rows. Keys come from event
/// properties, i.e. from clients: only keys inside the project's own replay
/// prefix are honoured.
pub async fn delete_replay_keys(store: &la_core::Store, project: u32, replay_keys: &[String]) -> Result<u64> {
    let prefix = format!("{}/", keys::replay_project_prefix(project));
    let mut deleted = 0;
    for key in replay_keys {
        let safe = key.starts_with(&prefix) && !key.split('/').any(|seg| seg.is_empty() || seg == "." || seg == "..");
        if !safe {
            tracing::warn!(project, "ignoring a replay key outside the project's replay prefix");
            continue;
        }
        store.delete(key).await?;
        deleted += 1;
    }
    Ok(deleted)
}

pub async fn delete_replay_objects(ctx: &Ctx, project: u32, replay_keys: &[String]) -> Result<u64> {
    let deleted = delete_replay_keys(&ctx.store, project, replay_keys).await?;
    if deleted > 0 {
        ctx.metrics.inc("replay_objects_deleted_total", &[], deleted);
    }
    Ok(deleted)
}

fn scan(data: InputData, tombs: &TombstoneSet) -> Result<bool> {
    fn run<T: parquet::file::reader::ChunkReader + 'static>(src: T, tombs: &TombstoneSet) -> Result<bool> {
        let builder = ParquetRecordBatchReaderBuilder::try_new(src)?;
        let schema = builder.schema().clone();
        let idx = |n: &str| schema.index_of(n).map_err(|_| Error::invalid(format!("no column {n}")));
        let mask =
            ProjectionMask::roots(builder.parquet_schema(), [idx("person_key")?, idx("anon_id")?, idx("user_id")?]);
        for batch in builder.with_projection(mask).with_batch_size(8192).build()? {
            let batch = batch?;
            let col = |n: &str| {
                batch
                    .column_by_name(n)
                    .and_then(|c| c.as_any().downcast_ref::<StringArray>())
                    .ok_or_else(|| Error::invalid(format!("mistyped column {n}")))
            };
            let (pk, anon, user) = (col("person_key")?, col("anon_id")?, col("user_id")?);
            for i in 0..batch.num_rows() {
                let u = (!user.is_null(i)).then(|| user.value(i));
                if tombs.matches(pk.value(i), anon.value(i), u) {
                    return Ok(true);
                }
            }
        }
        Ok(false)
    }
    match data {
        InputData::Mem(b) => run(b, tombs),
        InputData::File(f) => run(f, tombs),
        InputData::Batches(_) => Err(Error::invalid("cannot scan decoded batches")),
    }
}

/// Whether `file` contains a tombstoned row. `None` when the object is gone.
async fn file_is_affected(ctx: &Ctx, env: &Arc<ProjectEnv>, file: &FileEntry) -> Result<Option<bool>> {
    let spool = ctx.spool()?;
    let Some(data) = fetch_input(ctx, &file.key, file.bytes, spool.path(), "scan.parquet").await? else {
        return Ok(None);
    };
    let env = env.clone();
    let hit = tokio::task::spawn_blocking(move || scan(data, &env.tombstones))
        .await
        .map_err(|e| Error::invalid(format!("scan task failed: {e}")))??;
    Ok(Some(hit))
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct DayResult {
    /// `tombstone_ts` reached the newest tombstone.
    pub done: bool,
    pub rows_deleted: u64,
    pub replay_deleted: u64,
    pub rewritten: bool,
}

/// Applies all tombstones to one day. Safe to repeat and to interleave with
/// shipper appends: a file that shows up while this runs is checked in the
/// next round before `tombstone_ts` moves.
pub async fn process_day(ctx: &Ctx, env: &Arc<ProjectEnv>, day: &str) -> Result<DayResult> {
    let max_ts = env.tombstones.max_ts;
    let mut verified: HashSet<String> = HashSet::new();
    let mut res = DayResult::default();
    'round: for _ in 0..5 {
        let m = manifest::load_day(&ctx.store, env.project, day).await?;
        if m.version == 0 || m.tombstone_ts >= max_ts {
            res.done = true;
            return Ok(res);
        }
        let mut low: Vec<FileEntry> = Vec::new();
        let mut groups: Vec<Group> = Vec::new();
        for f in &m.files {
            if verified.contains(&f.key) {
                continue;
            }
            match file_is_affected(ctx, env, f).await? {
                None => continue 'round,
                Some(false) => {
                    verified.insert(f.key.clone());
                }
                Some(true) if f.level >= 2 => groups.push(Group { inputs: vec![f.clone()], level: 2 }),
                Some(true) => low.push(f.clone()),
            }
        }
        if !low.is_empty() {
            groups.insert(0, Group { inputs: low, level: 1 });
        }
        let staged = if groups.is_empty() {
            Staged::empty(ctx)?
        } else {
            match stage(ctx, env, day, groups).await? {
                Some(s) => s,
                None => continue 'round,
            }
        };
        // Before the commit: afterwards the rows naming the objects are gone.
        let replay_deleted = delete_replay_objects(ctx, env.project, &staged.replay_keys).await?;
        let plan = CommitPlan {
            replace: Replace::Inputs,
            set_plans: Default::default(),
            tombstone: Some(TombstoneAdvance { ts: max_ts, verified: verified.clone() }),
        };
        match commit(ctx, env.project, day, &staged, &plan).await? {
            Outcome::Committed { manifest, added } => {
                verified.extend(added.into_iter().map(|f| f.key));
                res.rows_deleted += staged.stats.tombstoned;
                res.replay_deleted += replay_deleted;
                res.rewritten |= !staged.removed.is_empty();
                ctx.metrics.inc("tombstoned_rows_total", &[], staged.stats.tombstoned);
                ctx.metrics.inc("bytes_rewritten_total", &[], staged.stats.bytes_out);
                if manifest.tombstone_ts >= max_ts {
                    res.done = true;
                    return Ok(res);
                }
            }
            Outcome::Unchanged(_) | Outcome::InputVanished | Outcome::Busy => {}
        }
    }
    Ok(res)
}

/// Freezes the identity expansion of every tombstone newer than `side_ts`
/// into `-x` tombstone objects. Returns `true` if any object was written (the
/// caller should reload its [`ProjectEnv`]).
pub async fn write_expansions(ctx: &Ctx, env: &ProjectEnv, side_ts: i64) -> Result<bool> {
    let mut wrote = false;
    for t in env.tombstone_list.iter().filter(|t| t.ts_ms > side_ts && !t.id.ends_with(EXPANSION_SUFFIX)) {
        let users: HashSet<String> =
            t.person_keys.iter().filter_map(|k| k.strip_prefix("u:")).map(str::to_owned).collect();
        if users.is_empty() {
            continue;
        }
        let listed: HashSet<&str> = t.anon_ids.iter().map(String::as_str).collect();
        let mut anon_ids: Vec<String> =
            env.identity.anons_of(&users).into_iter().filter(|a| !listed.contains(a.as_str())).collect();
        if anon_ids.is_empty() {
            continue;
        }
        anon_ids.sort();
        let x =
            Tombstone { id: format!("{}{EXPANSION_SUFFIX}", t.id), ts_ms: t.ts_ms, person_keys: Vec::new(), anon_ids };
        wrote |= ctx.store.put_json_if_absent(&tombstone_key(env.project, &x), &x).await?;
    }
    Ok(wrote)
}

/// Removes the tombstoned people from `persons/` and `identity/` and records
/// that in the state object. Call only after every day reported `done`.
pub async fn finish_side_tables(ctx: &Ctx, env: &ProjectEnv) -> Result<usize> {
    let users: HashSet<String> =
        env.tombstones.person_keys.iter().filter_map(|k| k.strip_prefix("u:")).map(str::to_owned).collect();
    persons::compact(&ctx.store, env.project, &env.tombstones.person_keys, true).await?;
    let links = identity::rewrite_dropping(&ctx.store, env.project, &env.tombstones.anon_ids, &users).await?;
    ctx.store.put_json(&keys::tombstone_state(env.project), &TombstoneState { side_ts: env.tombstones.max_ts }).await?;
    if links > 0 {
        ctx.metrics.inc("identity_links_deleted_total", &[], links as u64);
    }
    Ok(links)
}

/// Deletes tombstone objects past their retention (400 days by default).
pub async fn expire_tombstones(ctx: &Ctx, project: u32, now_ms: i64) -> Result<u64> {
    let cutoff = now_ms - ctx.cfg.tombstone_retention_days * DAY_MS;
    let mut deleted = 0;
    for o in ctx.store.list(&keys::tombstone_prefix(project)).await? {
        let name = o.key.rsplit('/').next().unwrap_or_default();
        let ts = name.split('-').next().filter(|t| t.len() == 13).and_then(|t| t.parse::<i64>().ok());
        if name.ends_with(".json") && ts.is_some_and(|ts| ts < cutoff) {
            ctx.store.delete(&o.key).await?;
            deleted += 1;
        }
    }
    Ok(deleted)
}
