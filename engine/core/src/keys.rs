//! Object key layout (`spec/manifest.md` §1). Every key is built here so the
//! layout has exactly one definition.

pub const TOKENS: &str = "control/tokens.json";
pub const KEYS: &str = "control/keys.json";
pub const PROJECTS: &str = "control/projects.json";
pub const SHARDS: &str = "control/shards.json";

pub fn wal_hour_prefix(dt: &str, hh: &str) -> String {
    format!("wal/dt={dt}/hh={hh}")
}

pub fn wal_day_prefix(dt: &str) -> String {
    format!("wal/dt={dt}")
}

/// `<node>-<seq>` from a segment key (`wal/dt=…/hh=…/<node>-<seq>.wal.zst`).
pub fn segment_name(key: &str) -> Option<&str> {
    key.rsplit('/').next()?.strip_suffix(".wal.zst")
}

/// `(dt, hh)` parsed from a segment key.
pub fn segment_dt_hh(key: &str) -> Option<(&str, &str)> {
    let mut it = key.split('/');
    if it.next()? != "wal" {
        return None;
    }
    let dt = it.next()?.strip_prefix("dt=")?;
    let hh = it.next()?.strip_prefix("hh=")?;
    Some((dt, hh))
}

pub fn wal_plan_dir(shardkey: &str) -> String {
    format!("wal/_plan/{shardkey}")
}

pub fn wal_plan(shardkey: &str, n: u64) -> String {
    format!("wal/_plan/{shardkey}/{n:010}.json")
}

pub fn wal_done_dir(shardkey: &str) -> String {
    format!("wal/_done/{shardkey}")
}

pub fn wal_done(shardkey: &str, segment_name: &str) -> String {
    format!("wal/_done/{shardkey}/{segment_name}.ok")
}

/// Projects a shipper plan wrote to, so the compactor's frequent cycle can
/// look at those instead of listing every project (`spec/manifest.md` §6).
pub fn wal_touched_dir(shardkey: &str) -> String {
    format!("wal/_touched/{shardkey}")
}

pub fn wal_touched(shardkey: &str, plan: u64) -> String {
    format!("wal/_touched/{shardkey}/{plan:010}.json")
}

/// Record of a segment the shipper skipped because it could not be decoded
/// (extension of `spec/manifest.md` §1; shares the `wal/` lifecycle).
pub fn wal_quarantine_dir(shardkey: &str) -> String {
    format!("wal/_quarantine/{shardkey}")
}

pub fn wal_quarantine(shardkey: &str, segment_name: &str) -> String {
    format!("wal/_quarantine/{shardkey}/{segment_name}.json")
}

/// Completion marker of a rebuild plan. The leading underscore keeps it out
/// of the `<node>-<seq>` segment name space.
pub fn wal_rebuild_done(shardkey: &str, plan: u64) -> String {
    format!("wal/_done/{shardkey}/_rebuild-{plan:010}.ok")
}

/// Plan number from a plan object key (`wal/_plan/<shardkey>/<n:010>.json`).
pub fn plan_number(key: &str) -> Option<u64> {
    let name = key.rsplit('/').next()?.strip_suffix(".json")?;
    if name.len() != 10 || !name.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    name.parse().ok()
}

/// Compactor bookkeeping for a project's tombstones. Deliberately not a
/// `.json` object so tombstone readers never mistake it for a tombstone.
pub fn tombstone_state(project: u32) -> String {
    format!("tombstones/p={project}/_applied.state")
}

pub fn replay_day_prefix(project: u32, day: &str) -> String {
    format!("replay/p={project}/d={day}")
}

pub fn data_project_prefix(project: u32) -> String {
    format!("data/p={project}")
}

pub fn data_day_prefix(project: u32, day: &str) -> String {
    format!("data/p={project}/d={day}")
}

pub fn data_manifest_dir(project: u32, day: &str) -> String {
    format!("data/p={project}/d={day}/_m")
}

pub fn l0_file(project: u32, day: &str, shardkey: &str, plan: u64) -> String {
    format!("data/p={project}/d={day}/L0-{shardkey}-{plan:010}.parquet")
}

/// L1/L2 output: `version` is the manifest version that adds the file.
pub fn level_file(project: u32, day: &str, level: u8, version: u64, part: u32) -> String {
    format!("data/p={project}/d={day}/L{level}-{version:08}-{part:03}.parquet")
}

pub fn manifest_file(dir: &str, version: u64) -> String {
    format!("{dir}/{version:08}.json")
}

/// Version number from a manifest object key, `None` for anything else.
pub fn manifest_version(key: &str) -> Option<u64> {
    let name = key.rsplit('/').next()?.strip_suffix(".json")?;
    if name.len() != 8 || !name.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    name.parse().ok()
}

pub fn identity_manifest_dir(project: u32) -> String {
    format!("identity/p={project}/_m")
}

pub fn identity_delta(project: u32, shardkey: &str, plan: u64) -> String {
    format!("identity/p={project}/map-{shardkey}-{plan:010}.parquet")
}

pub fn identity_compacted(project: u32, version: u64) -> String {
    format!("identity/p={project}/map-c{version:08}.parquet")
}

pub fn persons_manifest_dir(project: u32) -> String {
    format!("persons/p={project}/_m")
}

pub fn persons_delta(project: u32, shardkey: &str, plan: u64) -> String {
    format!("persons/p={project}/props-{shardkey}-{plan:010}.parquet")
}

pub fn persons_compacted(project: u32, version: u64) -> String {
    format!("persons/p={project}/props-c{version:08}.parquet")
}

pub fn tombstone_prefix(project: u32) -> String {
    format!("tombstones/p={project}")
}

pub fn replay_project_prefix(project: u32) -> String {
    format!("replay/p={project}")
}

pub fn project_schema(project: u32) -> String {
    format!("control/schema/p={project}.json")
}

pub fn lease_dir(name: &str) -> String {
    format!("control/lease/{name}")
}

pub fn lease_epoch(name: &str, epoch: u64) -> String {
    format!("control/lease/{name}/{epoch:012}.json")
}

/// `<pid>` from a `…/p=<pid>/…` style directory name.
pub fn parse_project_dir(name: &str) -> Option<u32> {
    name.strip_prefix("p=")?.parse().ok().filter(|p| *p >= 1)
}

/// `<day>` from a `d=<day>` directory name (validated).
pub fn parse_day_dir(name: &str) -> Option<&str> {
    let d = name.strip_prefix("d=")?;
    crate::time::parse_day(d).map(|_| d)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn layout_matches_spec() {
        assert_eq!(wal_plan("1-0", 1234), "wal/_plan/1-0/0000001234.json");
        assert_eq!(wal_done("1-0", "ing-01-000001795046"), "wal/_done/1-0/ing-01-000001795046.ok");
        assert_eq!(l0_file(1017, "2026-11-18", "1-0", 7), "data/p=1017/d=2026-11-18/L0-1-0-0000000007.parquet");
        assert_eq!(level_file(1017, "2026-11-18", 2, 42, 0), "data/p=1017/d=2026-11-18/L2-00000042-000.parquet");
        assert_eq!(
            manifest_file(&data_manifest_dir(1017, "2026-11-18"), 42),
            "data/p=1017/d=2026-11-18/_m/00000042.json"
        );
        assert_eq!(identity_delta(1017, "1-0", 1234), "identity/p=1017/map-1-0-0000001234.parquet");
        assert_eq!(lease_epoch("shipper-1-0", 57), "control/lease/shipper-1-0/000000000057.json");
        assert_eq!(wal_quarantine("1-0", "ing-01-000000000007"), "wal/_quarantine/1-0/ing-01-000000000007.json");
        assert_eq!(wal_rebuild_done("1-0", 12), "wal/_done/1-0/_rebuild-0000000012.ok");
        assert_eq!(tombstone_state(7), "tombstones/p=7/_applied.state");
        assert_eq!(replay_day_prefix(7, "2026-11-18"), "replay/p=7/d=2026-11-18");
    }

    #[test]
    fn parsers() {
        let k = "wal/dt=2026-11-18/hh=21/ing-01-000001795046.wal.zst";
        assert_eq!(segment_name(k), Some("ing-01-000001795046"));
        assert_eq!(segment_dt_hh(k), Some(("2026-11-18", "21")));
        assert_eq!(segment_name("wal/_done/1-0/x.ok"), None);
        assert_eq!(manifest_version("a/_m/00000042.json"), Some(42));
        assert_eq!(plan_number("wal/_plan/1-0/0000001234.json"), Some(1234));
        assert_eq!(plan_number("wal/_plan/1-0/1234.json"), None);
        assert_eq!(segment_name("wal/_done/1-0/_rebuild-0000000012.ok"), None);
        assert_eq!(manifest_version("a/_m/42.json"), None);
        assert_eq!(manifest_version("a/_m/0000004x.json"), None);
        assert_eq!(parse_project_dir("p=1017"), Some(1017));
        assert_eq!(parse_project_dir("p=0"), None);
        assert_eq!(parse_day_dir("d=2026-11-18"), Some("2026-11-18"));
        assert_eq!(parse_day_dir("d=../x"), None);
    }
}
