// Result views for the four insight kinds. Used by the builders (full size)
// and by dashboard tiles (`compact`).
import { useMemo } from 'preact/hooks'
import { S } from '../i18n'
import { heat } from '../lib/chart'
import { fmtBucket, fmtDateTime, fmtDuration, fmtInt, fmtNum, fmtPct, fmtValue } from '../lib/format'
import type { WithUi } from '../lib/insight'
import type { FunnelQuery, FunnelResult, FunnelStepResult, RetentionQuery, RetentionResult, SqlResult, TrendsQuery, TrendsResult } from '../lib/types'
import { TimeChart, type ChartSeries } from './charts'
import { Empty } from './ui'

const eventName = (event: string | null | undefined): string => event || S.query.allEvents

/** Display label of one request series (the response label is only a fallback). */
export const seriesLabel = (q: TrendsQuery, index: number, fallback?: string): string => {
  const s = q.series[index]
  if (!s) return fallback ?? `#${index + 1}`
  if (s.label) return s.label
  const base = eventName(s.event)
  return s.agg == 'count' ? base : `${base} · ${S.agg[s.agg]}${s.prop ? ` (${s.prop})` : ''}`
}

const breakdownLabel = (v: string | null | undefined): string => (v == null ? S.query.noValue : v == '$other' ? S.query.other : v)

export const TrendsView = ({ query, result, compact }: { query: WithUi<TrendsQuery>; result: TrendsResult; compact?: boolean }) => {
  const hasBreakdown = !!query.breakdown
  const series: (ChartSeries & { total: number | null })[] = useMemo(
    () =>
      result.series.map((s, i) => {
        const base = seriesLabel(query, s.index, s.label)
        const label = hasBreakdown ? (query.series.length > 1 ? `${base} · ${breakdownLabel(s.breakdown)}` : breakdownLabel(s.breakdown)) : base
        const agg = query.series[s.index]?.agg
        // unique persons and averages cannot be added up across buckets
        const total = agg == 'count' || agg == 'sum' ? s.values.reduce<number>((a, v) => a + (v ?? 0), 0) : null
        return { key: `${s.index}:${s.breakdown ?? ''}:${i}`, label, values: s.values, total }
      }),
    [result, query, hasBreakdown],
  )
  if (!result.buckets.length || !series.length) return <Empty title={S.query.noData}>{S.query.noDataHint}</Empty>
  const integer = query.series.every((s) => s.agg == 'count' || s.agg == 'unique')
  // one additive measure split by a breakdown is a whole and its parts: stack it
  const stacked = hasBreakdown && query.series.length == 1 && (query.series[0].agg == 'count' || query.series[0].agg == 'sum')
  return (
    <div class="result">
      <TimeChart
        type={query.ui?.chart == 'bar' ? 'bar' : 'line'}
        labels={result.buckets.map((b) => fmtBucket(b, query.interval))}
        titles={result.buckets.map((b) => fmtBucket(b, query.interval, true))}
        series={series}
        height={compact ? 190 : 280}
        integer={integer}
        stacked={stacked}
        ariaLabel={S.query.trendsAria(series.length, result.buckets.length)}
      />
      {!compact && (
        <div class="table-wrap">
          <table class="table table-num table-sticky">
            <thead>
              <tr>
                <th scope="col">{S.query.series}</th>
                <th scope="col">{S.query.total}</th>
                {result.buckets.map((b) => (
                  <th scope="col" key={b}>
                    {fmtBucket(b, query.interval)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {series.map((s) => (
                <tr key={s.key}>
                  <th scope="row">{s.label}</th>
                  <td class="strong">{s.total == null ? '—' : fmtNum(s.total)}</td>
                  {s.values.map((v, i) => (
                    <td key={i}>{fmtNum(v)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

const FunnelBars = ({ steps }: { steps: FunnelStepResult[] }) => {
  const first = steps[0]?.count ?? 0
  return (
    <ol class="funnel">
      {steps.map((s, i) => {
        const ofFirst = first ? s.count / first : 0
        const prev = i > 0 ? steps[i - 1].count : 0
        const ofPrev = i > 0 && prev ? s.count / prev : null
        return (
          <li class="funnel-step" key={i}>
            <div class="funnel-label">
              <span class="funnel-index">{i + 1}</span>
              <span class="funnel-name">{eventName(s.event)}</span>
            </div>
            <div class="funnel-track" role="img" aria-label={`${eventName(s.event)}: ${fmtInt(s.count)} (${fmtPct(ofFirst)})`}>
              <div class="funnel-fill" style={{ width: Math.max(ofFirst * 100, s.count ? 0.5 : 0) + '%' }} />
            </div>
            <div class="funnel-numbers">
              <span class="funnel-count">{fmtInt(s.count)}</span>
              <span class="funnel-pct">{fmtPct(ofFirst)}</span>
            </div>
            <div class="funnel-detail">
              {i == 0 ? (
                <span>{S.funnel.entered}</span>
              ) : (
                <>
                  <span>
                    {S.funnel.fromPrev}: <strong>{ofPrev == null ? '—' : fmtPct(ofPrev)}</strong>
                  </span>
                  <span>
                    {S.funnel.dropped}: <strong>{fmtInt(Math.max(0, prev - s.count))}</strong>
                  </span>
                  <span>
                    {S.funnel.median}: <strong>{fmtDuration(s.median_seconds)}</strong>
                  </span>
                </>
              )}
            </div>
          </li>
        )
      })}
    </ol>
  )
}

export const FunnelView = ({ query, result, compact }: { query: FunnelQuery; result: FunnelResult; compact?: boolean }) => {
  if (!result.steps.length || result.steps[0].count == 0) {
    return <Empty title={S.query.noData}>{S.funnel.noEntrants}</Empty>
  }
  const last = result.steps[result.steps.length - 1]
  return (
    <div class="result">
      <p class="headline">
        <strong>{fmtPct(last.count / result.steps[0].count)}</strong> {S.funnel.overall(fmtInt(result.steps[0].count), fmtInt(last.count))}
      </p>
      <FunnelBars steps={result.steps} />
      {!compact && query.breakdown && result.breakdowns && result.breakdowns.length > 0 && (
        <div class="table-wrap">
          <table class="table table-num">
            <caption>{S.funnel.byBreakdown(query.breakdown)}</caption>
            <thead>
              <tr>
                <th scope="col">{query.breakdown}</th>
                {result.steps.map((s, i) => (
                  <th scope="col" key={i}>
                    {i + 1}. {eventName(s.event)}
                  </th>
                ))}
                <th scope="col">{S.funnel.conversion}</th>
              </tr>
            </thead>
            <tbody>
              {result.breakdowns.map((b) => (
                <tr key={String(b.value)}>
                  <th scope="row">{breakdownLabel(b.value)}</th>
                  {b.steps.map((s, i) => (
                    <td key={i}>
                      {fmtInt(s.count)}
                      {i > 0 && <span class="muted"> · {b.steps[0].count ? fmtPct(s.count / b.steps[0].count) : '—'}</span>}
                    </td>
                  ))}
                  <td class="strong">{b.steps[0].count ? fmtPct(b.steps[b.steps.length - 1].count / b.steps[0].count) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

export const RetentionView = ({ query, result, compact }: { query: RetentionQuery; result: RetentionResult; compact?: boolean }) => {
  const cohorts = result.cohorts.filter((c) => c.size > 0 || !compact)
  const width = Math.max(0, ...result.cohorts.map((c) => c.returned.length))
  if (!result.cohorts.some((c) => c.size > 0)) return <Empty title={S.query.noData}>{S.retention.noCohorts}</Empty>
  const cols = Array.from({ length: width }, (_, k) => k)
  // weighted average per period over the cohorts old enough to have it
  const average = cols.map((k) => {
    const eligible = result.cohorts.filter((c) => c.returned.length > k && c.size > 0)
    const size = eligible.reduce((a, c) => a + c.size, 0)
    return size ? eligible.reduce((a, c) => a + c.returned[k], 0) / size : null
  })
  const cell = (ratio: number | null, count: number | null, key: number) => {
    if (ratio == null) return <td key={key} class="heat-empty" />
    const h = heat(ratio)
    return (
      <td key={key} class={`heat${h.lightText ? ' heat-dark' : ''}`} style={{ backgroundColor: h.color }} title={count == null ? undefined : S.retention.cellTitle(fmtInt(count))}>
        <span class="heat-pct">{fmtPct(ratio, ratio < 0.1 ? 1 : 0)}</span>
        {count != null && !compact && <span class="heat-count">{fmtInt(count)}</span>}
      </td>
    )
  }
  return (
    <div class="result">
      <div class="table-wrap">
        <table class="table table-num retention">
          <thead>
            <tr>
              <th scope="col">{S.retention.cohort}</th>
              <th scope="col">{S.retention.size}</th>
              {cols.map((k) => (
                <th scope="col" key={k}>
                  {S.retention.periodHead[query.period](k)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            <tr class="retention-avg">
              <th scope="row">{S.retention.average}</th>
              <td>{fmtInt(result.cohorts.reduce((a, c) => a + c.size, 0))}</td>
              {average.map((a, k) => cell(a, null, k))}
            </tr>
            {cohorts.map((c) => (
              <tr key={c.period_start}>
                <th scope="row">{fmtBucket(c.period_start, query.period, true)}</th>
                <td>{fmtInt(c.size)}</td>
                {cols.map((k) => (k < c.returned.length && c.size > 0 ? cell(c.returned[k] / c.size, c.returned[k], k) : <td key={k} class="heat-empty" />))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export const SqlView = ({ result, compact }: { result: SqlResult; compact?: boolean }) => {
  if (!result.columns.length) return <Empty title={S.query.noData} />
  const numeric = result.columns.map((c) => /int|float|decimal|double/i.test(c.type))
  const time = result.columns.map((c) => /timestamp/i.test(c.type))
  const rows = compact ? result.rows.slice(0, 50) : result.rows
  return (
    <div class="result">
      {!compact && <p class="muted small">{S.sql.rowCount(fmtInt(result.rows.length))}</p>}
      {result.rows.length == 0 ? (
        <Empty title={S.sql.noRows} />
      ) : (
        <div class={`table-wrap${compact ? '' : ' table-tall'}`}>
          <table class="table grid">
            <thead>
              <tr>
                {result.columns.map((c, i) => (
                  <th scope="col" key={i} class={numeric[i] ? 'num' : undefined} title={c.type}>
                    {c.name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row, r) => (
                <tr key={r}>
                  {row.map((v, i) => (
                    <td key={i} class={numeric[i] ? 'num' : v == null ? 'muted' : undefined}>
                      {v == null ? 'null' : time[i] && typeof v == 'number' ? fmtDateTime(v) : numeric[i] && typeof v == 'number' ? fmtNum(v) : fmtValue(v)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
