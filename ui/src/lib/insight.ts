// Stored insight queries: defaults, validation, and the body sent to the
// query proxy (relative range resolved, UI-only keys removed).
import { checkBounds, resolveBounds, withBounds, type BoundsError } from './dates'
import type { FunnelQuery, InsightKind, RetentionQuery, SqlQuery, StoredQuery, TrendSeries, TrendsQuery } from './types'

/** Display options stored next to the query; never sent to the query service. */
export interface UiOptions {
  chart?: 'line' | 'bar'
}
export type WithUi<Q> = Q & { ui?: UiOptions }

export const NEEDS_PROP = new Set(['sum', 'avg', 'min', 'max'])

export const defaultQuery = (kind: InsightKind): StoredQuery => {
  switch (kind) {
    case 'trends':
      return { range: '30d', interval: 'day', series: [{ event: null, agg: 'count' }] } satisfies TrendsQuery
    case 'funnel':
      return { range: '30d', steps: [{ event: '' }, { event: '' }], window_seconds: 604800, order: 'ordered' } satisfies FunnelQuery
    case 'retention':
      return { range: '60d', period: 'week', periods: 8, start: { event: '' }, return: { event: null } } satisfies RetentionQuery
    case 'sql':
      return { range: '7d', sql: 'SELECT event, count(*) AS adet\nFROM events\nGROUP BY 1\nORDER BY 2 DESC\nLIMIT 20' } satisfies SqlQuery
  }
}

export type QueryError =
  | { code: 'range'; detail: BoundsError }
  | { code: 'series_empty' | 'funnel_steps' | 'retention_start' | 'sql_empty' | 'periods' | 'window' }
  | { code: 'series_prop' | 'step_event'; index: number }

/** First problem that would make the query service reject the query, or null. */
export const validateQuery = (kind: InsightKind, query: StoredQuery, now: number): QueryError | null => {
  const range = checkBounds(resolveBounds(query, now))
  if (range) return { code: 'range', detail: range }
  if (kind == 'trends') {
    const q = query as TrendsQuery
    if (!q.series.length) return { code: 'series_empty' }
    const i = q.series.findIndex((s) => NEEDS_PROP.has(s.agg) && !s.prop?.trim())
    if (i >= 0) return { code: 'series_prop', index: i }
  } else if (kind == 'funnel') {
    const q = query as FunnelQuery
    if (q.steps.length < 2 || q.steps.length > 20) return { code: 'funnel_steps' }
    const i = q.steps.findIndex((s) => s.event === '')
    if (i >= 0) return { code: 'step_event', index: i }
    if (!(q.window_seconds > 0)) return { code: 'window' }
  } else if (kind == 'retention') {
    const q = query as RetentionQuery
    if (q.start.event === '') return { code: 'retention_start' }
    if (!(Number.isInteger(q.periods) && q.periods >= 1 && q.periods <= 64)) return { code: 'periods' }
  } else if (!(query as SqlQuery).sql.trim()) return { code: 'sql_empty' }
  return null
}

const cleanSeries = (s: TrendSeries): TrendSeries => {
  const out: TrendSeries = { event: s.event || null, agg: s.agg }
  if (NEEDS_PROP.has(s.agg) && s.prop?.trim()) out.prop = s.prop.trim()
  if (s.label?.trim()) out.label = s.label.trim()
  if (s.filter) out.filter = s.filter
  return out
}

/** Normalises a query for saving: trims, drops empty optional fields. Keeps `range` and `ui`. */
export const cleanQuery = (kind: InsightKind, query: WithUi<StoredQuery>): WithUi<StoredQuery> => {
  const time = query.range ? { range: query.range } : { from: query.from, to: query.to }
  const ui = query.ui?.chart == 'bar' ? { ui: { chart: 'bar' as const } } : {}
  if (kind == 'trends') {
    const q = query as TrendsQuery
    return {
      ...time,
      interval: q.interval,
      series: q.series.map(cleanSeries),
      ...(q.filter ? { filter: q.filter } : {}),
      ...(q.breakdown?.trim() ? { breakdown: q.breakdown.trim(), breakdown_limit: q.breakdown_limit ?? 10 } : {}),
      ...ui,
    }
  }
  if (kind == 'funnel') {
    const q = query as FunnelQuery
    return {
      ...time,
      steps: q.steps.map((s) => ({ event: s.event || null, ...(s.filter ? { filter: s.filter } : {}) })),
      window_seconds: q.window_seconds,
      order: q.order,
      ...(q.filter ? { filter: q.filter } : {}),
      ...(q.breakdown?.trim() ? { breakdown: q.breakdown.trim() } : {}),
    }
  }
  if (kind == 'retention') {
    const q = query as RetentionQuery
    const step = (s: RetentionQuery['start']) => ({ event: s.event || null, ...(s.filter ? { filter: s.filter } : {}) })
    return { ...time, period: q.period, periods: q.periods, start: step(q.start), return: step(q.return), ...(q.filter ? { filter: q.filter } : {}) }
  }
  return { ...time, sql: (query as SqlQuery).sql }
}

/** The proxy request body: cleaned, `ui` removed, `range` → `from`/`to`. */
export const requestBody = (kind: InsightKind, query: WithUi<StoredQuery>, now: number): Record<string, unknown> => {
  const { ui: _ui, ...rest } = cleanQuery(kind, query)
  return withBounds(rest, now)
}
