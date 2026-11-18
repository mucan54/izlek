//! One rewrite of a day: stage merged outputs on local disk, then commit a
//! manifest version that swaps them in for exactly the inputs
//! (`spec/manifest.md` §2).
//!
//! Output names carry the manifest version that adds them, so the names are
//! only known once the base version is: outputs are uploaded with
//! create-if-absent right before the manifest write. Losing the manifest race
//! (typically to a shipper appending an L0) removes the uploads and repeats
//! the upload under the next version's names; files the shipper added in the
//! meantime are preserved because every attempt starts from the latest
//! manifest. If an input is no longer listed, another writer already replaced
//! it and the job gives up without touching anything.

use crate::config::Ctx;
use crate::merge::{merge_inputs, InputData, MergeInput, MergeParams, MergeResult, MergeStats, StagedFile};
use bytes::Bytes;
use futures::StreamExt;
use la_core::error::Result;
use la_core::identity::IdentityMap;
use la_core::manifest::{self, DayManifest, FileEntry};
use la_core::schema::ProjectSchema;
use la_core::tombstone::{self, Tombstone, TombstoneSet};
use la_core::{keys, metrics, Store};
use object_store::ObjectStoreExt;
use std::collections::{BTreeMap, HashSet};
use std::io::Write;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

/// Per-project state every rewrite applies.
#[derive(Debug, Clone)]
pub struct ProjectEnv {
    pub project: u32,
    pub schema: ProjectSchema,
    pub identity: IdentityMap,
    pub tombstone_list: Vec<Tombstone>,
    /// Every known tombstone, expanded (`after_ts = 0`).
    pub tombstones: TombstoneSet,
}

impl ProjectEnv {
    pub async fn load(store: &Store, project: u32) -> Result<Self> {
        let schema = ProjectSchema::load(store, project).await?;
        let identity = IdentityMap::load(store, project).await?;
        let tombstone_list = tombstone::load_all(store, project).await?;
        let tombstones = TombstoneSet::build(&tombstone_list, &identity, 0);
        Ok(Self { project, schema, identity, tombstone_list, tombstones })
    }
}

/// Files merged together into outputs of one level.
#[derive(Debug, Clone)]
pub struct Group {
    pub inputs: Vec<FileEntry>,
    pub level: u8,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Replace {
    /// The new manifest drops exactly the staged inputs.
    Inputs,
    /// The new manifest drops every file it lists (rebuild).
    All,
}

/// Advance `tombstone_ts` to `ts` if every file that stays listed is known to
/// be free of tombstoned rows.
#[derive(Debug, Clone, Default)]
pub struct TombstoneAdvance {
    pub ts: i64,
    pub verified: HashSet<String>,
}

#[derive(Debug, Clone)]
pub struct CommitPlan {
    pub replace: Replace,
    /// `plans[shardkey] = max(current, n)` for each entry.
    pub set_plans: BTreeMap<String, u64>,
    pub tombstone: Option<TombstoneAdvance>,
}

impl CommitPlan {
    pub fn replace_inputs() -> Self {
        Self { replace: Replace::Inputs, set_plans: BTreeMap::new(), tombstone: None }
    }
}

/// Merged outputs waiting on local disk.
#[derive(Debug)]
pub struct Staged {
    pub removed: HashSet<String>,
    pub files: Vec<StagedFile>,
    pub stats: MergeStats,
    pub replay_keys: Vec<String>,
    /// Owns the spool directory the staged files live in.
    pub spool: tempfile::TempDir,
}

impl Staged {
    /// An empty staging area (nothing to merge, e.g. a tombstone-only commit).
    pub fn empty(ctx: &Ctx) -> Result<Self> {
        Ok(Self {
            removed: HashSet::new(),
            files: Vec::new(),
            stats: MergeStats::default(),
            replay_keys: Vec::new(),
            spool: ctx.spool()?,
        })
    }
}

#[derive(Debug)]
pub enum Outcome {
    Committed { manifest: DayManifest, added: Vec<FileEntry> },
    /// Nothing needed to change; the manifest is the one that was current.
    Unchanged(DayManifest),
    /// An input is no longer listed — somebody else already replaced it.
    InputVanished,
    /// Too many lost races; try again in a later cycle.
    Busy,
}

/// Fetches one object for merging: small ones into memory, large ones into
/// the spool directory. `None` when the object does not exist.
pub async fn fetch_input(ctx: &Ctx, key: &str, size: u64, spool: &Path, name: &str) -> Result<Option<InputData>> {
    if size <= ctx.cfg.mem_input_bytes {
        return Ok(ctx.store.get(key).await?.map(InputData::Mem));
    }
    let os = ctx.store.object_store();
    let res = match os.get(&object_store::path::Path::from(key)).await {
        Ok(r) => r,
        Err(object_store::Error::NotFound { .. }) => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    let path = spool.join(name);
    let mut file = std::fs::File::create(&path)?;
    let mut stream = res.into_stream();
    while let Some(chunk) = stream.next().await {
        file.write_all(&chunk?)?;
    }
    file.flush()?;
    drop(file);
    Ok(Some(InputData::File(std::fs::File::open(&path)?)))
}

async fn merge_blocking(
    ctx: &Ctx,
    env: &Arc<ProjectEnv>,
    day: &str,
    level: u8,
    inputs: Vec<MergeInput>,
    dir: std::path::PathBuf,
) -> Result<MergeResult> {
    let (env, cfg, day) = (env.clone(), ctx.cfg.clone(), day.to_owned());
    tokio::task::spawn_blocking(move || {
        let params = MergeParams {
            project: env.project,
            day: &day,
            level,
            promoted: &env.schema.columns,
            schema_version: env.schema.version,
            identity: &env.identity,
            tombstones: &env.tombstones,
            target_file_bytes: cfg.target_file_bytes,
            batch_rows: cfg.batch_rows,
            max_moved_rows: cfg.max_moved_rows,
            max_person_uuids: cfg.max_person_uuids,
            spool: &dir,
        };
        merge_inputs(inputs, &params)
    })
    .await
    .map_err(|e| la_core::Error::invalid(format!("merge task failed: {e}")))?
}

impl Staged {
    fn absorb(&mut self, result: MergeResult) {
        self.stats.rows_in += result.stats.rows_in;
        self.stats.rows_out += result.stats.rows_out;
        self.stats.duplicates += result.stats.duplicates;
        self.stats.tombstoned += result.stats.tombstoned;
        self.stats.moved += result.stats.moved;
        self.stats.bytes_out += result.stats.bytes_out;
        self.files.extend(result.files);
        self.replay_keys.extend(result.replay_keys);
    }
}

/// Downloads and merges every group. `None` when an input object is gone.
pub async fn stage(ctx: &Ctx, env: &Arc<ProjectEnv>, day: &str, groups: Vec<Group>) -> Result<Option<Staged>> {
    let mut staged = Staged::empty(ctx)?;
    for (gi, group) in groups.into_iter().enumerate() {
        let dir = staged.spool.path().join(format!("g{gi}"));
        std::fs::create_dir_all(&dir)?;
        let mut inputs = Vec::with_capacity(group.inputs.len());
        for (i, f) in group.inputs.iter().enumerate() {
            match fetch_input(ctx, &f.key, f.bytes, &dir, &format!("in-{i:04}.parquet")).await? {
                Some(data) => inputs.push(MergeInput { name: f.key.clone(), data }),
                None => return Ok(None),
            }
            staged.removed.insert(f.key.clone());
        }
        let result = merge_blocking(ctx, env, day, group.level, inputs, dir).await?;
        staged.absorb(result);
    }
    Ok(Some(staged))
}

/// Merges sorted run files that live on local disk (rebuild) into staged
/// outputs of `level`. More runs than the fan-in limit are merged in passes.
pub async fn stage_local_runs(
    ctx: &Ctx,
    env: &Arc<ProjectEnv>,
    day: &str,
    level: u8,
    mut runs: Vec<std::path::PathBuf>,
) -> Result<Staged> {
    let mut staged = Staged::empty(ctx)?;
    let mut pass = 0;
    loop {
        let last = runs.len() <= ctx.cfg.max_inputs;
        let take = runs.len().min(ctx.cfg.max_inputs);
        let batch: Vec<std::path::PathBuf> = runs.drain(..take).collect();
        let dir = staged.spool.path().join(format!("pass{pass}"));
        pass += 1;
        std::fs::create_dir_all(&dir)?;
        let mut inputs = Vec::with_capacity(batch.len());
        for path in &batch {
            inputs.push(MergeInput {
                name: path.display().to_string(),
                data: InputData::File(std::fs::File::open(path)?),
            });
        }
        let result = merge_blocking(ctx, env, day, level, inputs, dir).await?;
        if last {
            staged.absorb(result);
            return Ok(staged);
        }
        // Intermediate outputs go to the back of the queue as ordinary runs.
        runs.extend(result.files.into_iter().map(|f| f.path));
    }
}

const MAX_ATTEMPTS: u32 = 24;

async fn delete_all(store: &Store, keys: &[String]) {
    for k in keys {
        if let Err(e) = store.delete(k).await {
            tracing::warn!(key = %k, error = %e, "could not remove an uncommitted output");
        }
    }
}

/// Publishes staged outputs (see the module docs for the protocol).
pub async fn commit(ctx: &Ctx, project: u32, day: &str, staged: &Staged, plan: &CommitPlan) -> Result<Outcome> {
    let store = &ctx.store;
    let dir = keys::data_manifest_dir(project, day);
    for attempt in 0..MAX_ATTEMPTS {
        let m = manifest::load_day(store, project, day).await?;
        if plan.replace == Replace::Inputs && !staged.removed.iter().all(|k| m.files.iter().any(|f| &f.key == k)) {
            return Ok(Outcome::InputVanished);
        }
        let mut next = m.clone();
        match plan.replace {
            Replace::Inputs => next.files.retain(|f| !staged.removed.contains(&f.key)),
            Replace::All => next.files.clear(),
        }
        if let Some(t) = &plan.tombstone {
            // Outputs are clean by construction; everything else must have been checked.
            if next.files.iter().all(|f| t.verified.contains(&f.key)) {
                next.tombstone_ts = next.tombstone_ts.max(t.ts);
            }
        }
        for (shardkey, n) in &plan.set_plans {
            let e = next.plans.entry(shardkey.clone()).or_default();
            *e = (*e).max(*n);
        }
        let removed_any = next.files.len() != m.files.len();
        if staged.files.is_empty() && !removed_any && next.tombstone_ts == m.tombstone_ts && next.plans == m.plans {
            return Ok(Outcome::Unchanged(m));
        }

        let target = m.version + 1;
        let mut created: Vec<String> = Vec::new();
        let mut added: Vec<FileEntry> = Vec::new();
        let mut parts = [0u32; 3];
        let mut blocked = false;
        for f in &staged.files {
            let level = f.entry.level.min(2);
            let key = keys::level_file(project, day, level, target, parts[level as usize]);
            parts[level as usize] += 1;
            let data = Bytes::from(tokio::fs::read(&f.path).await?);
            if store.put_if_absent(&key, data).await? {
                created.push(key.clone());
                added.push(FileEntry { key, ..f.entry.clone() });
            } else {
                blocked = true;
                break;
            }
        }
        if blocked {
            // The names of version `target` are taken by a writer that crashed
            // before committing (or is racing us). They can never be reused,
            // so step over them with a content-neutral version.
            delete_all(store, &created).await;
            let mut bump = m.clone();
            bump.version = target;
            bump.created_at = la_core::time::now_ms();
            if store.put_json_if_absent(&keys::manifest_file(&dir, target), &bump).await? {
                tracing::warn!(project, day, version = target, "stepped over output names left by another writer");
            }
            tokio::time::sleep(Duration::from_millis(rand::random_range(0..=20u64 << attempt.min(5)))).await;
            continue;
        }
        next.files.extend(added.iter().cloned());
        next.version = target;
        next.project = project;
        next.day = day.to_owned();
        next.created_at = la_core::time::now_ms();
        if store.put_json_if_absent(&keys::manifest_file(&dir, target), &next).await? {
            return Ok(Outcome::Committed { manifest: next, added });
        }
        delete_all(store, &created).await;
        metrics::MANIFEST_CONFLICTS.inc();
        ctx.metrics.inc("commit_conflicts_total", &[], 1);
        tokio::time::sleep(Duration::from_millis(rand::random_range(0..=(10u64 << attempt.min(6))))).await;
    }
    Ok(Outcome::Busy)
}

/// Stages and commits one group, updating the compaction metrics.
///
/// Every known tombstone is applied to the rewritten rows; when the job
/// happens to replace every file of the day, `tombstone_ts` advances with it.
pub async fn compact(ctx: &Ctx, env: &Arc<ProjectEnv>, day: &str, group: Group) -> Result<Outcome> {
    let started = std::time::Instant::now();
    let level = group.level.to_string();
    let Some(staged) = stage(ctx, env, day, vec![group]).await? else {
        return Ok(Outcome::InputVanished);
    };
    crate::tombstones::delete_replay_objects(ctx, env.project, &staged.replay_keys).await?;
    let mut plan = CommitPlan::replace_inputs();
    if env.tombstones.max_ts > 0 {
        plan.tombstone = Some(TombstoneAdvance { ts: env.tombstones.max_ts, verified: HashSet::new() });
    }
    let outcome = commit(ctx, env.project, day, &staged, &plan).await?;
    if let Outcome::Committed { manifest, added } = &outcome {
        ctx.metrics.observe("compaction_seconds", &[("level", &level)], started.elapsed().as_secs_f64());
        ctx.metrics.inc("bytes_rewritten_total", &[], staged.stats.bytes_out);
        ctx.metrics.inc("rows_rewritten_total", &[], staged.stats.rows_out);
        ctx.metrics.inc("duplicates_dropped_total", &[], staged.stats.duplicates);
        ctx.metrics.inc("tombstoned_rows_total", &[], staged.stats.tombstoned);
        ctx.metrics.inc("compactions_total", &[("level", &level)], 1);
        tracing::info!(
            project = env.project,
            day,
            level = %level,
            inputs = staged.removed.len(),
            outputs = added.len(),
            rows_in = staged.stats.rows_in,
            rows_out = staged.stats.rows_out,
            duplicates = staged.stats.duplicates,
            tombstoned = staged.stats.tombstoned,
            rekeyed = staged.stats.moved,
            version = manifest.version,
            seconds = started.elapsed().as_secs_f64(),
            "compacted"
        );
    }
    Ok(outcome)
}
