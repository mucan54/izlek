// Scale and tick helpers for the hand-rolled SVG charts.

/** A "nice" step (1, 2, 2.5, 5 × 10^k) so that `span / step` is close to `count`. */
export const niceStep = (span: number, count: number): number => {
  if (!(span > 0) || count < 1) return 1
  const raw = span / count
  const pow = Math.pow(10, Math.floor(Math.log10(raw)))
  const unit = raw / pow
  const nice = unit <= 1 ? 1 : unit <= 2 ? 2 : unit <= 2.5 ? 2.5 : unit <= 5 ? 5 : 10
  return nice * pow
}

export interface Axis {
  min: number
  max: number
  step: number
  ticks: number[]
}

/**
 * Axis covering [lo, hi] with round tick values. The axis always includes 0
 * (counts and sums are read against the baseline) unless `zero` is false.
 * Integer data (`integer`) never gets fractional ticks.
 */
export const niceAxis = (lo: number, hi: number, count = 5, options: { zero?: boolean; integer?: boolean } = {}): Axis => {
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) lo = hi = 0
  if (lo > hi) [lo, hi] = [hi, lo]
  if (options.zero !== false) {
    lo = Math.min(0, lo)
    hi = Math.max(0, hi)
  }
  if (lo == hi) hi = lo + 1
  let step = niceStep(hi - lo, count)
  if (options.integer) step = Math.max(1, Math.ceil(step))
  const round = (v: number) => Number(v.toPrecision(12))
  const min = round(Math.floor(lo / step + 1e-9) * step)
  const max = round(Math.ceil(hi / step - 1e-9) * step)
  const ticks: number[] = []
  for (let i = 0; min + i * step <= max + step * 1e-9; i++) ticks.push(round(min + i * step))
  return { min, max, step, ticks }
}

/** Linear map from a domain to a pixel range. A zero-width domain maps to the range start. */
export const scaleLinear = (d0: number, d1: number, r0: number, r1: number): ((v: number) => number) => {
  const k = d1 == d0 ? 0 : (r1 - r0) / (d1 - d0)
  return (v) => r0 + (v - d0) * k
}

/** Positions of `n` evenly spaced points/bands across [r0, r1]. */
export const band = (n: number, r0: number, r1: number): { step: number; center: (i: number) => number } => {
  const step = n > 0 ? (r1 - r0) / n : 0
  return { step, center: (i) => r0 + step * (i + 0.5) }
}

/**
 * Indices of the x labels to draw so that at most `max` are shown, evenly
 * spread, always starting at the first bucket.
 */
export const pickLabels = (n: number, max: number): number[] => {
  if (n <= 0 || max <= 0) return []
  const every = Math.max(1, Math.ceil(n / max))
  const out: number[] = []
  for (let i = 0; i < n; i += every) out.push(i)
  return out
}

/** Compact tick label: 950, 1,2 B, 3,4 Mn (decimal mark and suffixes supplied by the locale). */
export const compact = (v: number, suffix: { k: string; m: string; b: string } = { k: 'K', m: 'M', b: 'B' }, mark = '.'): string => {
  const abs = Math.abs(v)
  const fmt = (x: number, s: string) => {
    const r = Math.abs(x) >= 100 ? Math.round(x) : Math.round(x * 10) / 10
    return String(r).replace('.', mark) + s
  }
  if (abs >= 1e9) return fmt(v / 1e9, suffix.b)
  if (abs >= 1e6) return fmt(v / 1e6, suffix.m)
  if (abs >= 1e4) return fmt(v / 1e3, suffix.k)
  return String(Math.round(v * 100) / 100).replace('.', mark)
}

/** Index of the bucket whose centre is nearest to pixel `x`. */
export const nearestIndex = (x: number, n: number, r0: number, r1: number): number => {
  if (n <= 1) return 0
  const step = (r1 - r0) / n
  return Math.max(0, Math.min(n - 1, Math.floor((x - r0) / step)))
}

/** Sequential blue ramp (light → dark) used by the retention heatmap. */
export const SEQUENTIAL = ['#cde2fb', '#b7d3f6', '#9ec5f4', '#86b6ef', '#6da7ec', '#5598e7', '#3987e5', '#2a78d6', '#256abf', '#1c5cab', '#184f95', '#104281']

/** Step of the ramp for a ratio in [0, 1]; also says whether text on it should be light. */
export const heat = (ratio: number): { index: number; color: string; lightText: boolean } => {
  const r = Math.max(0, Math.min(1, Number.isFinite(ratio) ? ratio : 0))
  const index = Math.min(SEQUENTIAL.length - 1, Math.floor(r * SEQUENTIAL.length))
  return { index, color: SEQUENTIAL[index], lightText: index >= 6 }
}
