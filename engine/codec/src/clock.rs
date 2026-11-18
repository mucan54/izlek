//! Clock-skew correction (`spec/wire.md` §5).

pub const FLAG_CLOCK_CORRECTED: i32 = 1;
pub const FLAG_LATE: i32 = 2;
pub const FLAG_COMPAT: i32 = 4;
pub const FLAG_WEB: i32 = 8;

pub const SKEW_TOLERANCE_MS: i64 = 5_000;
pub const LATE_AFTER_MS: i64 = 30 * 86_400_000;

/// Returns the corrected timestamp and the `clock_corrected`/`late` flag bits.
///
/// `sent_at == 0` means the device did not report a send time.
pub fn correct(recv_ts: i64, sent_at: i64, ts: i64) -> (i64, i32) {
    let skew = if sent_at == 0 { 0 } else { recv_ts.saturating_sub(sent_at) };
    let mut ts = ts;
    let mut flags = 0;
    if skew.saturating_abs() > SKEW_TOLERANCE_MS {
        ts = ts.saturating_add(skew);
        flags |= FLAG_CLOCK_CORRECTED;
    }
    flags |= clamp_flags(recv_ts, &mut ts);
    (ts, flags)
}

/// The "clamp future" and "late" steps only — used when no `sent_at` based
/// correction applies (replay index entries, PostHog events without `sent_at`).
pub fn clamp(recv_ts: i64, ts: i64) -> (i64, i32) {
    let mut ts = ts;
    let flags = clamp_flags(recv_ts, &mut ts);
    (ts, flags)
}

fn clamp_flags(recv_ts: i64, ts: &mut i64) -> i32 {
    let mut flags = 0;
    if *ts > recv_ts {
        *ts = recv_ts;
        flags |= FLAG_CLOCK_CORRECTED;
    }
    if recv_ts.saturating_sub(*ts) > LATE_AFTER_MS {
        flags |= FLAG_LATE;
    }
    if *ts < 0 {
        *ts = 0;
    }
    flags
}

/// Wire timestamps are `u64`; anything beyond `i64::MAX` is treated as "far
/// future" and will be clamped to `recv_ts` by [`correct`].
pub fn wire_ts(ts: u64) -> i64 {
    i64::try_from(ts).unwrap_or(i64::MAX)
}
