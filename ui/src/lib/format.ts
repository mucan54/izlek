// Number, date and duration formatting for the current locale.
import { INTL, S } from '../i18n'
import { axisLabels, compact } from './chart'

const nf = (opts: Intl.NumberFormatOptions) => new Intl.NumberFormat(INTL, opts)

export const fmtInt = (n: number): string => nf({ maximumFractionDigits: 0 }).format(n)

/** Up to 2 decimals, grouped; null → em dash. */
export const fmtNum = (n: number | null | undefined): string => (n == null || Number.isNaN(n) ? '—' : nf({ maximumFractionDigits: 2 }).format(n))

export const fmtPct = (ratio: number, digits = 1): string =>
  Number.isFinite(ratio) ? '%' + nf({ maximumFractionDigits: digits, minimumFractionDigits: 0 }).format(ratio * 100) : '—'

export const fmtCompact = (n: number): string => compact(n, S.fmt.compact, S.fmt.decimal)

/** Tick labels for one chart axis (one style per axis). */
export const fmtAxis = (ticks: number[]): string[] => axisLabels(ticks, S.fmt.compact, S.fmt.decimal)

export const fmtBytes = (n: number): string => {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return nf({ maximumFractionDigits: v >= 100 || i == 0 ? 0 : 1 }).format(v) + ' ' + units[i]
}

/** Local date + time with seconds, e.g. 18.11.2026 14:03:27. */
export const fmtDateTime = (ms: number): string =>
  new Intl.DateTimeFormat(INTL, { dateStyle: 'short', timeStyle: 'medium' }).format(new Date(ms))

export const fmtDate = (ms: number): string => new Intl.DateTimeFormat(INTL, { dateStyle: 'medium' }).format(new Date(ms))

export const fmtTime = (ms: number): string => new Intl.DateTimeFormat(INTL, { timeStyle: 'medium' }).format(new Date(ms))

/** "3 dk önce" style relative time for recent instants, a date otherwise. */
export const fmtAgo = (ms: number, now = Date.now()): string => {
  const s = Math.round((now - ms) / 1000)
  if (s < 0 || s > 7 * 86400) return fmtDateTime(ms)
  if (s < 60) return S.fmt.ago(s, S.fmt.sec)
  if (s < 3600) return S.fmt.ago(Math.floor(s / 60), S.fmt.min)
  if (s < 86400) return S.fmt.ago(Math.floor(s / 3600), S.fmt.hour)
  return S.fmt.ago(Math.floor(s / 86400), S.fmt.day)
}

/** Duration from seconds: "42 sn", "3 dk 20 sn", "2 sa 5 dk", "4 gün 3 sa". */
export const fmtDuration = (seconds: number | null | undefined): string => {
  if (seconds == null || !Number.isFinite(seconds)) return '—'
  const s = Math.max(0, seconds)
  if (s < 1) return nf({ maximumFractionDigits: 2 }).format(s) + ' ' + S.fmt.sec
  if (s < 60) return Math.round(s) + ' ' + S.fmt.sec
  const parts = (a: number, ua: string, b: number, ub: string) => (b ? `${a} ${ua} ${b} ${ub}` : `${a} ${ua}`)
  if (s < 3600) return parts(Math.floor(s / 60), S.fmt.min, Math.round(s % 60) % 60, S.fmt.sec)
  if (s < 86400) return parts(Math.floor(s / 3600), S.fmt.hour, Math.floor((s % 3600) / 60), S.fmt.min)
  return parts(Math.floor(s / 86400), S.fmt.day, Math.floor((s % 86400) / 3600), S.fmt.hour)
}

/** mm:ss for the replay clock. */
export const fmtClock = (ms: number): string => {
  const s = Math.max(0, Math.floor(ms / 1000))
  return String(Math.floor(s / 60)).padStart(2, '0') + ':' + String(s % 60).padStart(2, '0')
}

/** Label of a UTC time bucket for the given interval. */
export const fmtBucket = (iso: string, interval: string, long = false): string => {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const base: Intl.DateTimeFormatOptions = { timeZone: 'UTC' }
  if (interval == 'hour')
    return new Intl.DateTimeFormat(INTL, { ...base, day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }).format(d)
  if (interval == 'month') return new Intl.DateTimeFormat(INTL, { ...base, month: long ? 'long' : 'short', year: 'numeric' }).format(d)
  return new Intl.DateTimeFormat(INTL, { ...base, day: 'numeric', month: 'short', ...(long ? { year: 'numeric' } : {}) }).format(d)
}

/** A JSON value for a table cell or a props list. */
export const fmtValue = (v: unknown): string => {
  if (v === null || v === undefined) return '—'
  if (typeof v == 'string') return v
  if (typeof v == 'number') return Number.isInteger(v) && Math.abs(v) < 1e15 ? String(v) : String(Math.round(v * 1e6) / 1e6)
  if (typeof v == 'boolean') return v ? 'true' : 'false'
  return JSON.stringify(v)
}
