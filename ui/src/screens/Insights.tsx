// Saved insights: the list, and one editor for the four kinds (trends,
// funnel, retention, SQL) with save / save-as / rename / delete.
import type { ComponentChildren } from 'preact'
import { useEffect, useRef, useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { boundsOf, setBounds } from '../lib/dates'
import { COLUMNS, QUERY_OPS } from '../lib/filter'
import { fmtAgo } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { NEEDS_PROP, cleanQuery, defaultQuery, requestBody, validateQuery, type QueryError, type WithUi } from '../lib/insight'
import { can } from '../lib/roles'
import { Link, navigate } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import type {
  Agg, Expr, FunnelOrder, FunnelQuery, FunnelResult, Insight, InsightKind, Interval, Meta, Period, RetentionQuery, RetentionResult,
  SqlQuery, SqlResult, StoredQuery, TrendsQuery, TrendsResult,
} from '../lib/types'
import { DateRange } from '../components/DateRange'
import { ExprFilter, InlineFilter, querySuggest } from '../components/FilterEditor'
import { FunnelView, RetentionView, SqlView, TrendsView } from '../components/results'
import { AsyncView, Badge, Button, Card, ConfirmButton, Empty, ErrorNote, Field, MetaLine, PageHead, Select, Spinner, toast, useId } from '../components/ui'

export const KINDS: InsightKind[] = ['trends', 'funnel', 'retention', 'sql']
const isKind = (k: unknown): k is InsightKind => KINDS.includes(k as InsightKind)

export const queryErrorText = (e: QueryError): string => {
  switch (e.code) {
    case 'range':
      return S.range.errors[e.detail]
    case 'series_prop':
      return S.trends.errors.seriesProp(e.index + 1)
    case 'step_event':
      return S.funnel.errors.stepEvent(e.index + 1)
    default:
      return S.insights.errors[e.code]
  }
}

/** Renders the right result view for an insight kind. */
export const ResultView = ({ kind, query, result, compact }: { kind: InsightKind; query: WithUi<StoredQuery>; result: unknown; compact?: boolean }) => {
  if (kind == 'trends') return <TrendsView query={query as WithUi<TrendsQuery>} result={result as TrendsResult} compact={compact} />
  if (kind == 'funnel') return <FunnelView query={query as FunnelQuery} result={result as FunnelResult} compact={compact} />
  if (kind == 'retention') return <RetentionView query={query as RetentionQuery} result={result as RetentionResult} compact={compact} />
  return <SqlView result={result as SqlResult} compact={compact} />
}

// ---------------------------------------------------------------- list

export const InsightList = () => {
  const project = useProject()
  const app = useApp()
  const list = useAsync(async (signal) => asList<Insight>(await app.api.get(`${project.base}/insights`, { signal })), [project.base])
  const canWrite = can(project.role, 'insight.write')
  const [kind, setKind] = useState<'all' | InsightKind>('all')
  return (
    <>
      <PageHead title={S.insights.title} sub={S.insights.sub}>
        {canWrite &&
          KINDS.map((k) => (
            <Link class={`btn ${k == 'trends' ? 'btn-primary' : 'btn-default'}`} href={project.to('insights/new/' + k)} key={k}>
              + {S.insights.kinds[k]}
            </Link>
          ))}
      </PageHead>
      <AsyncView
        state={list}
        empty={
          <Empty title={S.insights.empty}>
            {canWrite ? S.insights.emptyHint : S.insights.emptyViewer}
            {!canWrite && (
              <p>
                <Link href={project.to('insights/new/trends')}>{S.insights.explore}</Link>
              </p>
            )}
          </Empty>
        }
      >
        {(items) => {
          const shown = items.filter((i) => kind == 'all' || i.kind == kind).sort((a, b) => b.updated_at - a.updated_at)
          return (
            <Card
              actions={
                <Select
                  aria-label={S.insights.kind}
                  value={kind}
                  onChange={setKind}
                  options={[['all', S.insights.allKinds] as const, ...KINDS.map((k) => [k, S.insights.kinds[k]] as const)]}
                />
              }
              title={S.insights.count(shown.length)}
            >
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.insights.name}</th>
                    <th scope="col">{S.insights.kind}</th>
                    <th scope="col">{S.common.updated}</th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((i) => (
                    <tr key={i.id}>
                      <td>
                        <Link href={project.to('insights/' + i.id)}>{i.name}</Link>
                      </td>
                      <td>
                        <Badge>{S.insights.kinds[i.kind] ?? i.kind}</Badge>
                      </td>
                      <td class="nowrap">{fmtAgo(i.updated_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
          )
        }}
      </AsyncView>
    </>
  )
}

// ---------------------------------------------------------------- forms

interface FormProps<Q> {
  query: Q
  onChange: (query: Q) => void
  /** changes when a different insight is loaded, so filter editors rebuild */
  resetKey: unknown
  eventsList: string
  propsList: string
}

const EventInput = ({ value, onChange, list, label, anyLabel }: { value: string | null; onChange: (v: string | null) => void; list: string; label: string; anyLabel?: string }) => (
  <input
    class="input event-input"
    list={list}
    value={value ?? ''}
    placeholder={anyLabel ?? S.query.eventPlaceholder}
    aria-label={label}
    onInput={(e) => onChange(e.currentTarget.value.trim() == '' && anyLabel ? null : e.currentTarget.value)}
  />
)

const AGGS: Agg[] = ['count', 'unique', 'sum', 'avg', 'min', 'max']
const INTERVALS: Interval[] = ['hour', 'day', 'week', 'month']

const GlobalFilter = ({ value, onChange, resetKey }: { value: Expr | undefined; onChange: (e: Expr | undefined) => void; resetKey: unknown }) => {
  const { schema } = useProject()
  return (
    <fieldset class="form-section">
      <legend>{S.query.globalFilter}</legend>
      <ExprFilter value={value} onChange={onChange} ops={QUERY_OPS} suggest={querySuggest(schema)} resetKey={resetKey} />
    </fieldset>
  )
}

const TrendsForm = ({ query, onChange, resetKey, eventsList, propsList }: FormProps<WithUi<TrendsQuery>>) => {
  const { schema } = useProject()
  const setSeries = (i: number, patch: Partial<TrendsQuery['series'][number]>) =>
    onChange({ ...query, series: query.series.map((s, k) => (k == i ? { ...s, ...patch } : s)) })
  return (
    <>
      <fieldset class="form-section">
        <legend>{S.trends.series}</legend>
        <ol class="rows">
          {query.series.map((s, i) => (
            <li class="row-item" key={i}>
              <span class="row-index">{String.fromCharCode(65 + (i % 26))}</span>
              <div class="row-fields">
                <EventInput value={s.event} onChange={(event) => setSeries(i, { event })} list={eventsList} label={S.trends.seriesEvent(i + 1)} anyLabel={S.query.allEvents} />
                <select class="input select" aria-label={S.trends.agg} value={s.agg} onChange={(e) => setSeries(i, { agg: e.currentTarget.value as Agg })}>
                  {AGGS.map((a) => (
                    <option value={a} key={a}>
                      {S.agg[a]}
                    </option>
                  ))}
                </select>
                {NEEDS_PROP.has(s.agg) && (
                  <input
                    class="input"
                    list={propsList}
                    value={s.prop ?? ''}
                    placeholder={S.trends.propPlaceholder}
                    aria-label={S.trends.prop}
                    onInput={(e) => setSeries(i, { prop: e.currentTarget.value })}
                  />
                )}
                <input class="input" value={s.label ?? ''} placeholder={S.trends.labelPlaceholder} aria-label={S.trends.label} onInput={(e) => setSeries(i, { label: e.currentTarget.value })} />
                <InlineFilter value={s.filter} onChange={(filter) => setSeries(i, { filter })} ops={QUERY_OPS} suggest={querySuggest(schema)} resetKey={`${resetKey}:${query.series.length}`} />
              </div>
              {query.series.length > 1 && (
                <button type="button" class="btn btn-ghost btn-sm" aria-label={S.trends.removeSeries(i + 1)} onClick={() => onChange({ ...query, series: query.series.filter((_, k) => k != i) })}>
                  ✕
                </button>
              )}
            </li>
          ))}
        </ol>
        <Button small kind="ghost" onClick={() => onChange({ ...query, series: [...query.series, { event: null, agg: 'count' }] })}>
          + {S.trends.addSeries}
        </Button>
      </fieldset>
      <div class="form-grid">
        <Field label={S.trends.interval}>
          {(id) => <Select id={id} value={query.interval} onChange={(interval) => onChange({ ...query, interval })} options={INTERVALS.map((i) => [i, S.intervals[i]] as const)} />}
        </Field>
        <Field label={S.query.breakdown} hint={S.query.breakdownHint}>
          {(id, d) => (
            <input
              id={id}
              class="input"
              list={propsList}
              value={query.breakdown ?? ''}
              placeholder={S.query.breakdownNone}
              aria-describedby={d}
              onInput={(e) => onChange({ ...query, breakdown: e.currentTarget.value || undefined })}
            />
          )}
        </Field>
        {query.breakdown && (
          <Field label={S.trends.breakdownLimit}>
            {(id) => (
              <input
                id={id}
                class="input input-num"
                type="number"
                min={1}
                max={100}
                value={query.breakdown_limit ?? 10}
                onInput={(e) => onChange({ ...query, breakdown_limit: Math.max(1, Math.min(100, Number(e.currentTarget.value) || 10)) })}
              />
            )}
          </Field>
        )}
        <Field label={S.trends.chartType}>
          {(id) => (
            <Select
              id={id}
              value={query.ui?.chart == 'bar' ? 'bar' : 'line'}
              onChange={(chart) => onChange({ ...query, ui: chart == 'bar' ? { chart } : undefined })}
              options={[['line', S.trends.line] as const, ['bar', S.trends.bar] as const]}
            />
          )}
        </Field>
      </div>
      <GlobalFilter value={query.filter} onChange={(filter) => onChange({ ...query, filter })} resetKey={resetKey} />
    </>
  )
}

const WINDOW_UNITS = [
  ['60', 'minute'],
  ['3600', 'hour'],
  ['86400', 'day'],
] as const

const FunnelForm = ({ query, onChange, resetKey, eventsList, propsList }: FormProps<FunnelQuery>) => {
  const { schema } = useProject()
  // steps move around, so filter editors must be rebuilt when the order changes
  const [shuffle, setShuffle] = useState(0)
  const unit = query.window_seconds % 86400 == 0 ? 86400 : query.window_seconds % 3600 == 0 ? 3600 : 60
  const setStep = (i: number, patch: Partial<FunnelQuery['steps'][number]>) => onChange({ ...query, steps: query.steps.map((s, k) => (k == i ? { ...s, ...patch } : s)) })
  const move = (i: number, d: number) => {
    const steps = [...query.steps]
    const [s] = steps.splice(i, 1)
    steps.splice(i + d, 0, s)
    setShuffle(shuffle + 1)
    onChange({ ...query, steps })
  }
  return (
    <>
      <fieldset class="form-section">
        <legend>{S.funnel.steps}</legend>
        <ol class="rows">
          {query.steps.map((s, i) => (
            <li class="row-item" key={`${shuffle}:${i}`}>
              <span class="row-index">{i + 1}</span>
              <div class="row-fields">
                <EventInput value={s.event} onChange={(event) => setStep(i, { event })} list={eventsList} label={S.funnel.stepEvent(i + 1)} />
                <InlineFilter value={s.filter} onChange={(filter) => setStep(i, { filter })} ops={QUERY_OPS} suggest={querySuggest(schema)} resetKey={`${resetKey}:${shuffle}`} />
              </div>
              <div class="row-tools">
                <button type="button" class="btn btn-ghost btn-sm" disabled={i == 0} aria-label={S.funnel.moveUp(i + 1)} onClick={() => move(i, -1)}>
                  ↑
                </button>
                <button type="button" class="btn btn-ghost btn-sm" disabled={i == query.steps.length - 1} aria-label={S.funnel.moveDown(i + 1)} onClick={() => move(i, 1)}>
                  ↓
                </button>
                <button
                  type="button"
                  class="btn btn-ghost btn-sm"
                  disabled={query.steps.length <= 2}
                  aria-label={S.funnel.removeStep(i + 1)}
                  onClick={() => {
                    setShuffle(shuffle + 1)
                    onChange({ ...query, steps: query.steps.filter((_, k) => k != i) })
                  }}
                >
                  ✕
                </button>
              </div>
            </li>
          ))}
        </ol>
        <Button small kind="ghost" disabled={query.steps.length >= 20} onClick={() => onChange({ ...query, steps: [...query.steps, { event: '' }] })}>
          + {S.funnel.addStep}
        </Button>
      </fieldset>
      <div class="form-grid">
        <Field label={S.funnel.window} hint={S.funnel.windowHint}>
          {(id, d) => (
            <div class="row">
              <input
                id={id}
                class="input input-num"
                type="number"
                min={1}
                value={Math.max(1, Math.round(query.window_seconds / unit))}
                aria-describedby={d}
                onInput={(e) => onChange({ ...query, window_seconds: Math.max(1, Math.round(Number(e.currentTarget.value) || 1)) * unit })}
              />
              <Select
                aria-label={S.funnel.windowUnit}
                value={String(unit) as '60' | '3600' | '86400'}
                onChange={(u) => onChange({ ...query, window_seconds: Math.max(1, Math.round(query.window_seconds / unit)) * Number(u) })}
                options={WINDOW_UNITS.map(([v, k]) => [v, S.units[k]] as const)}
              />
            </div>
          )}
        </Field>
        <Field label={S.funnel.order} hint={S.funnel.orderHints[query.order]}>
          {(id, d) => (
            <Select
              id={id}
              aria-describedby={d}
              value={query.order}
              onChange={(order: FunnelOrder) => onChange({ ...query, order })}
              options={(['ordered', 'strict', 'any'] as const).map((o) => [o, S.funnel.orders[o]] as const)}
            />
          )}
        </Field>
        <Field label={S.query.breakdown} hint={S.funnel.breakdownHint}>
          {(id, d) => (
            <input
              id={id}
              class="input"
              list={propsList}
              value={query.breakdown ?? ''}
              placeholder={S.query.breakdownNone}
              aria-describedby={d}
              onInput={(e) => onChange({ ...query, breakdown: e.currentTarget.value || undefined })}
            />
          )}
        </Field>
      </div>
      <GlobalFilter value={query.filter} onChange={(filter) => onChange({ ...query, filter })} resetKey={resetKey} />
    </>
  )
}

const RetentionForm = ({ query, onChange, resetKey, eventsList }: FormProps<RetentionQuery>) => {
  const { schema } = useProject()
  return (
    <>
      <div class="form-grid form-grid-2">
        <fieldset class="form-section">
          <legend>{S.retention.start}</legend>
          <p class="field-hint">{S.retention.startHint}</p>
          <div class="row-fields">
            <EventInput value={query.start.event} onChange={(event) => onChange({ ...query, start: { ...query.start, event } })} list={eventsList} label={S.retention.start} />
            <InlineFilter value={query.start.filter} onChange={(filter) => onChange({ ...query, start: { ...query.start, filter } })} ops={QUERY_OPS} suggest={querySuggest(schema)} resetKey={resetKey} />
          </div>
        </fieldset>
        <fieldset class="form-section">
          <legend>{S.retention.return}</legend>
          <p class="field-hint">{S.retention.returnHint}</p>
          <div class="row-fields">
            <EventInput
              value={query.return.event}
              onChange={(event) => onChange({ ...query, return: { ...query.return, event } })}
              list={eventsList}
              label={S.retention.return}
              anyLabel={S.query.allEvents}
            />
            <InlineFilter value={query.return.filter} onChange={(filter) => onChange({ ...query, return: { ...query.return, filter } })} ops={QUERY_OPS} suggest={querySuggest(schema)} resetKey={resetKey} />
          </div>
        </fieldset>
      </div>
      <div class="form-grid">
        <Field label={S.retention.period}>
          {(id) => <Select id={id} value={query.period} onChange={(period: Period) => onChange({ ...query, period })} options={(['day', 'week', 'month'] as const).map((p) => [p, S.intervals[p]] as const)} />}
        </Field>
        <Field label={S.retention.periods} hint={S.retention.periodsHint}>
          {(id, d) => (
            <input
              id={id}
              class="input input-num"
              type="number"
              min={1}
              max={64}
              value={query.periods}
              aria-describedby={d}
              onInput={(e) => onChange({ ...query, periods: Math.round(Number(e.currentTarget.value)) })}
            />
          )}
        </Field>
      </div>
      <GlobalFilter value={query.filter} onChange={(filter) => onChange({ ...query, filter })} resetKey={resetKey} />
    </>
  )
}

const SqlForm = ({ query, onChange, onRun }: { query: SqlQuery; onChange: (q: SqlQuery) => void; onRun: () => void }) => {
  const { schema } = useProject()
  const id = useId('sql')
  return (
    <div class="sql-form">
      <div class="field">
        <label for={id}>{S.sql.editor}</label>
        <textarea
          id={id}
          class="input mono sql-editor"
          spellcheck={false}
          autocapitalize="off"
          rows={8}
          value={query.sql}
          aria-describedby={id + '-h'}
          onInput={(e) => onChange({ ...query, sql: e.currentTarget.value })}
          onKeyDown={(e) => {
            if (e.key == 'Enter' && (e.ctrlKey || e.metaKey)) {
              e.preventDefault()
              onRun()
            }
          }}
        />
        <p class="field-hint" id={id + '-h'}>
          {S.sql.hint}
        </p>
      </div>
      <details class="sql-help">
        <summary>{S.sql.reference}</summary>
        <dl class="kv">
          <dt>events</dt>
          <dd>
            <code>uuid, ts, recv_ts, {COLUMNS.join(', ')}, flags, props</code>
          </dd>
          <dt>persons</dt>
          <dd>
            <code>person_key, props</code>
          </dd>
          <dt>{S.sql.functions}</dt>
          <dd>
            <code>prop(props, 'k') · prop_num(props, 'k') · prop_bool(props, 'k') · hex(uuid)</code>
          </dd>
          {schema && schema.props.length > 0 && (
            <>
              <dt>{S.sql.knownProps}</dt>
              <dd>
                <code>{schema.props.map((p) => p.key).join(', ')}</code>
              </dd>
            </>
          )}
        </dl>
      </details>
    </div>
  )
}

// ---------------------------------------------------------------- editor

interface Run {
  key: string
  query: WithUi<StoredQuery>
  result?: unknown
  error?: Error
  loading: boolean
}

export const InsightEditor = ({ id, kind: kindParam }: { id?: string; kind?: string }) => {
  const project = useProject()
  const app = useApp()
  const canWrite = can(project.role, 'insight.write')
  const loaded = useAsync<Insight | null>(async (signal) => (id ? app.api.get<Insight>(`${project.base}/insights/${encodeURIComponent(id)}`, { signal }) : null), [id, project.base])

  const [kind, setKind] = useState<InsightKind>(isKind(kindParam) ? kindParam : 'trends')
  const [name, setName] = useState('')
  const [query, setQuery] = useState<WithUi<StoredQuery>>(() => defaultQuery(isKind(kindParam) ? kindParam : 'trends'))
  const [saved, setSaved] = useState<string>('')
  const [resetKey, setResetKey] = useState(0)
  const [run, setRun] = useState<Run | null>(null)
  const seq = useRef(0)
  const eventsList = useId('ev')
  const propsList = useId('pr')

  useEffect(() => {
    const i = loaded.data
    if (!i) return
    const k = isKind(i.kind) ? i.kind : 'sql'
    setKind(k)
    setName(i.name)
    setQuery(i.query)
    setSaved(JSON.stringify([i.name, cleanQuery(k, i.query)]))
    setResetKey((n) => n + 1)
  }, [loaded.data])

  const problem = validateQuery(kind, query, Date.now())
  const cleaned = JSON.stringify(cleanQuery(kind, query))
  const dirty = JSON.stringify([name.trim(), JSON.parse(cleaned)]) != saved

  const execute = async () => {
    if (validateQuery(kind, query, Date.now())) return
    const mine = ++seq.current
    setRun((r) => ({ key: cleaned, query, result: r?.result, loading: true }))
    try {
      const result = await project.query<unknown>(kind, requestBody(kind, query, Date.now()))
      if (mine == seq.current) setRun({ key: cleaned, query, result, loading: false })
    } catch (e) {
      if (mine == seq.current) setRun({ key: cleaned, query, error: e as Error, loading: false })
    }
  }

  // Run when the query changes (debounced). SQL only runs on demand or once after loading.
  const ready = !id || loaded.data !== undefined
  useEffect(() => {
    if (!ready || problem) return
    if (kind == 'sql' && run) return
    const t = setTimeout(() => void execute(), run ? 500 : 0)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cleaned, ready, kind, resetKey])

  const save = useAction(async (asCopy: boolean) => {
    const body = { name: name.trim() || S.insights.untitled(S.insights.kinds[kind]), kind, query: cleanQuery(kind, query) }
    if (id && !asCopy) {
      const updated = await app.api.put<Insight>(`${project.base}/insights/${encodeURIComponent(id)}`, body)
      setName(updated?.name ?? body.name)
      setSaved(JSON.stringify([updated?.name ?? body.name, body.query]))
      toast(S.insights.saved)
    } else {
      if (asCopy) body.name = S.insights.copyOf(body.name)
      const created = await app.api.post<Insight>(`${project.base}/insights`, body)
      toast(S.insights.saved)
      navigate(project.to('insights/' + created.id), !asCopy)
    }
  })

  if (id && loaded.error) return <ErrorNote error={loaded.error} onRetry={loaded.reload} />
  if (id && loaded.data === undefined) return <Spinner />
  if (!id && !isKind(kindParam)) return <Empty title={S.shell.notFound} />

  const stale = run && run.key != cleaned
  const meta = (run?.result as { meta?: Meta } | undefined)?.meta
  const formProps = { resetKey, eventsList, propsList }
  let form: ComponentChildren
  if (kind == 'trends') form = <TrendsForm query={query as WithUi<TrendsQuery>} onChange={setQuery} {...formProps} />
  else if (kind == 'funnel') form = <FunnelForm query={query as FunnelQuery} onChange={setQuery} {...formProps} />
  else if (kind == 'retention') form = <RetentionForm query={query as RetentionQuery} onChange={setQuery} {...formProps} />
  else form = <SqlForm query={query as SqlQuery} onChange={setQuery} onRun={() => void execute()} />

  return (
    <>
      <PageHead
        title={
          <span class="title-edit">
            <Link href={project.to('insights')} class="crumb">
              {S.insights.title}
            </Link>
            <span aria-hidden="true" class="crumb-sep">
              /
            </span>
            <input
              class="input title-input"
              value={name}
              placeholder={S.insights.untitled(S.insights.kinds[kind])}
              aria-label={S.insights.name}
              maxLength={200}
              readOnly={!canWrite}
              onInput={(e) => setName(e.currentTarget.value)}
            />
            <Badge>{S.insights.kinds[kind]}</Badge>
          </span>
        }
      >
        {canWrite ? (
          <>
            {dirty && id && <span class="muted">{S.insights.unsaved}</span>}
            <Button kind="primary" busy={save.busy} disabled={!!problem || (!dirty && !!id)} onClick={() => void save.run(false)}>
              {S.common.save}
            </Button>
            {id && (
              <Button busy={save.busy} disabled={!!problem} onClick={() => void save.run(true)}>
                {S.insights.saveAs}
              </Button>
            )}
            {id && (
              <ConfirmButton
                label={S.common.delete}
                title={S.insights.deleteTitle}
                message={S.insights.deleteMessage(name)}
                onConfirm={async () => {
                  await app.api.del(`${project.base}/insights/${encodeURIComponent(id)}`)
                  toast(S.insights.deleted)
                  navigate(project.to('insights'))
                }}
              />
            )}
          </>
        ) : (
          <span class="muted">{S.insights.readOnly}</span>
        )}
      </PageHead>
      <ErrorNote error={save.error} />
      <datalist id={eventsList}>
        {(project.schema?.events ?? []).map((e) => (
          <option value={e} key={e} />
        ))}
      </datalist>
      <datalist id={propsList}>
        {querySuggest(project.schema).props.map((p) => (
          <option value={p} key={p} />
        ))}
      </datalist>
      <Card class="builder">
        <div class="toolbar">
          <DateRange value={boundsOf(query)} onChange={(b) => setQuery(setBounds(query, b))} />
          <span class="spacer" />
          {problem && <span class="field-error">{queryErrorText(problem)}</span>}
          <Button kind={kind == 'sql' ? 'primary' : 'default'} busy={run?.loading} disabled={!!problem} onClick={() => void execute()}>
            {S.query.run}
          </Button>
        </div>
        {form}
      </Card>
      <Card title={S.query.result} actions={<MetaLine meta={meta} />} class={`result-card${run?.loading || stale ? ' refreshing' : ''}`}>
        {run?.error ? (
          <ErrorNote error={run.error} onRetry={() => void execute()} />
        ) : run?.result !== undefined ? (
          <ResultView kind={kind} query={run.query} result={run.result} />
        ) : run?.loading ? (
          <Spinner label={S.query.running} />
        ) : (
          <Empty title={problem ? S.query.incomplete : S.query.notRun}>{problem ? queryErrorText(problem) : kind == 'sql' ? S.sql.runHint : undefined}</Empty>
        )}
      </Card>
    </>
  )
}
