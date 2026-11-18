// Date ranges. Insights store either a relative range ("7d") or concrete
// bounds; the query API only accepts concrete `from`/`to`, so the UI resolves
// the range at call time (spec/control.md §6, spec/query.md §2).
import type { TimeBounds } from './types'

export const DAY_MS = 86_400_000
export const MAX_RANGE_DAYS = 400

/** Relative presets offered by the picker, in days. */
export const PRESET_DAYS = [1, 7, 14, 30, 90, 180, 365]

export interface Relative {
  n: number
  unit: 'd' | 'h'
}

export const parseRelative = (range: string): Relative | null => {
  const m = /^([1-9][0-9]{0,4})([dh])$/.exec(range)
  return m ? { n: Number(m[1]), unit: m[2] as 'd' | 'h' } : null
}

/** `YYYY-MM-DD` of the UTC day containing `ms`. */
export const utcDay = (ms: number): string => new Date(ms).toISOString().slice(0, 10)

export const isDay = (s: string): boolean => /^\d{4}-\d{2}-\d{2}$/.test(s) && !Number.isNaN(Date.parse(s + 'T00:00:00Z'))

export interface Bounds {
  from: string
  to: string
}

/**
 * Concrete bounds for a stored range.
 *
 * - `Nd`: the last N UTC days **including today** — `to` is today (inclusive
 *   day), `from` is N−1 days earlier. "7d" on 2026-11-18 → 11-12 … 11-18.
 * - `Nh`: RFC 3339 instants, `to` exclusive, both snapped to the minute so
 *   that refreshes within a minute hit the same result-cache key.
 * - no/unknown range: the stored `from`/`to` unchanged.
 */
export const resolveBounds = (b: TimeBounds, now: number): Bounds => {
  const rel = b.range ? parseRelative(b.range) : null
  if (!rel) return { from: b.from ?? utcDay(now - 6 * DAY_MS), to: b.to ?? utcDay(now) }
  if (rel.unit == 'd') return { from: utcDay(now - (rel.n - 1) * DAY_MS), to: utcDay(now) }
  const end = Math.ceil((now + 1) / 60_000) * 60_000
  const iso = (ms: number) => new Date(ms).toISOString().replace('.000Z', 'Z')
  return { from: iso(end - rel.n * 3_600_000), to: iso(end) }
}

/** The request body for the query proxy: `range` replaced by `from`/`to`. */
export const withBounds = <Q extends TimeBounds>(query: Q, now: number): Omit<Q, 'range'> & Bounds => {
  const { range: _range, ...rest } = query
  return { ...rest, ...resolveBounds(query, now) }
}

const toMs = (s: string, endOfDay: boolean): number =>
  isDay(s) ? Date.parse(s + 'T00:00:00Z') + (endOfDay ? DAY_MS : 0) : Date.parse(s)

export type BoundsError = 'invalid' | 'order' | 'too_long'

/** Checks concrete bounds against the API limits. */
export const checkBounds = (b: Bounds): BoundsError | null => {
  const from = toMs(b.from, false)
  const to = toMs(b.to, true)
  if (Number.isNaN(from) || Number.isNaN(to)) return 'invalid'
  if (to <= from) return 'order'
  if (to - from > MAX_RANGE_DAYS * DAY_MS) return 'too_long'
  return null
}

/** Number of UTC days covered by concrete bounds (at least 1). */
export const spanDays = (b: Bounds): number => Math.max(1, Math.ceil((toMs(b.to, true) - toMs(b.from, false)) / DAY_MS))

/** Just the time part of a stored query. */
export const boundsOf = (q: TimeBounds): TimeBounds => (q.range ? { range: q.range } : { from: q.from, to: q.to })

/** Replaces the time part of a stored query, leaving everything else. */
export const setBounds = <Q extends TimeBounds>(q: Q, b: TimeBounds): Q => {
  const { range: _r, from: _f, to: _t, ...rest } = q
  return { ...rest, ...boundsOf(b) } as Q
}
