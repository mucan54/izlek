//! When a day gets compacted (`docs/design.md` §10.1).

use crate::config::CompactorConfig;
use crate::job::Group;
use la_core::manifest::{DayManifest, FileEntry};
use la_core::time::{day_start_ms, DAY_MS};

/// The next compaction for a day, if any is due at `now_ms`.
///
/// * open day — all L0 files → one L1 run once there are more than
///   `l0_trigger_files` of them or they exceed `l0_trigger_bytes`; all L0+L1
///   files → L1 once more than `l1_max_files` L1 files piled up;
/// * closed day (`l2_delay` after its end) — everything → L2 the first time,
///   later again only when late files pile up past the L0 thresholds.
///
/// A job never takes more than `max_inputs` files: larger backlogs are first
/// reduced by L1 merges of the smallest files.
pub fn next_job(m: &DayManifest, now_ms: i64, cfg: &CompactorConfig) -> Option<Group> {
    if m.files.is_empty() {
        return None;
    }
    let closed = day_start_ms(&m.day).is_some_and(|start| now_ms >= start + DAY_MS + cfg.l2_delay_ms);
    let level = |l: u8| -> Vec<&FileEntry> { m.files.iter().filter(|f| f.level == l).collect() };
    let (l0, l1, l2) = (level(0), level(1), level(2));
    let bytes = |fs: &[&FileEntry]| fs.iter().map(|f| f.bytes).sum::<u64>();
    let over = |fs: &[&FileEntry]| fs.len() > cfg.l0_trigger_files || bytes(fs) > cfg.l0_trigger_bytes;

    let (inputs, level): (Vec<&FileEntry>, u8) = if closed {
        let below: Vec<&FileEntry> = l0.iter().chain(l1.iter()).copied().collect();
        if below.is_empty() || (!l2.is_empty() && !over(&below)) {
            return None;
        }
        (m.files.iter().collect(), 2)
    } else if over(&l0) {
        (l0, 1)
    } else if cfg.l1_max_files > 0 && l1.len() > cfg.l1_max_files {
        (l0.into_iter().chain(l1).collect(), 1)
    } else {
        return None;
    };
    if inputs.len() <= cfg.max_inputs {
        return Some(Group { inputs: inputs.into_iter().cloned().collect(), level });
    }
    // Too wide: shrink the backlog below L2 first, smallest files first.
    let mut below: Vec<&FileEntry> = m.files.iter().filter(|f| f.level < 2).collect();
    if below.len() < 2 {
        // Only L2 parts left over the fan-in: merge the smallest ones.
        below = m.files.iter().collect();
    }
    below.sort_by(|a, b| a.bytes.cmp(&b.bytes).then_with(|| a.key.cmp(&b.key)));
    below.truncate(cfg.max_inputs);
    let level = if below.iter().all(|f| f.level == 2) { 2 } else { 1 };
    Some(Group { inputs: below.into_iter().cloned().collect(), level })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn f(level: u8, i: usize, bytes: u64) -> FileEntry {
        FileEntry { key: format!("L{level}-{i}"), level, bytes, rows: 1, ..Default::default() }
    }

    fn day(files: Vec<FileEntry>) -> DayManifest {
        DayManifest { version: 1, project: 1, day: "2026-11-18".into(), files, ..Default::default() }
    }

    fn closed(cfg: &CompactorConfig) -> i64 {
        day_start_ms("2026-11-18").unwrap() + DAY_MS + cfg.l2_delay_ms
    }

    #[test]
    fn open_day_l0_thresholds() {
        let cfg = CompactorConfig::default();
        let now = day_start_ms("2026-11-18").unwrap() + 3_600_000;
        assert!(next_job(&day((0..8).map(|i| f(0, i, 10)).collect()), now, &cfg).is_none());
        let g = next_job(&day((0..9).map(|i| f(0, i, 10)).chain([f(1, 99, 10)]).collect()), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (1, 9), "L1 files are not part of an L0→L1 merge");
        let g = next_job(&day(vec![f(0, 1, 200 << 20), f(0, 2, 100 << 20)]), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (1, 2), "byte threshold");
        assert!(next_job(&day(vec![]), now, &cfg).is_none());
    }

    #[test]
    fn open_day_l1_pileup() {
        let cfg = CompactorConfig { l1_max_files: 4, ..Default::default() };
        let now = day_start_ms("2026-11-18").unwrap();
        let files: Vec<_> = (0..5).map(|i| f(1, i, 10)).chain([f(0, 9, 1)]).collect();
        let g = next_job(&day(files.clone()), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (1, 6));
        let off = CompactorConfig { l1_max_files: 0, ..Default::default() };
        assert!(next_job(&day(files), now, &off).is_none());
    }

    #[test]
    fn closed_day_goes_to_l2_once_then_waits_for_a_backlog() {
        let cfg = CompactorConfig::default();
        let now = closed(&cfg);
        let files = vec![f(0, 1, 10), f(1, 2, 10)];
        assert!(next_job(&day(files.clone()), now - 1, &cfg).is_none(), "not before the delay");
        let g = next_job(&day(files), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (2, 2));
        assert_eq!(next_job(&day(vec![f(0, 1, 10)]), now, &cfg).unwrap().level, 2, "a lone L0 is finalized too");
        assert!(next_job(&day(vec![f(2, 1, 10)]), now, &cfg).is_none());
        let mut late = vec![f(2, 1, 10), f(0, 2, 10)];
        assert!(next_job(&day(late.clone()), now, &cfg).is_none(), "a few late files wait");
        late.extend((3..12).map(|i| f(0, i, 10)));
        let g = next_job(&day(late), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (2, 11));
    }

    #[test]
    fn fan_in_is_bounded() {
        let cfg = CompactorConfig { max_inputs: 4, ..Default::default() };
        let now = closed(&cfg);
        let files: Vec<_> = (0..10).map(|i| f(0, i, 100 - i as u64)).chain([f(2, 50, 5)]).collect();
        let g = next_job(&day(files), now, &cfg).unwrap();
        assert_eq!((g.level, g.inputs.len()), (1, 4));
        assert!(g.inputs.iter().all(|f| f.level == 0 && f.bytes <= 94), "smallest first: {:?}", g.inputs);
    }
}
