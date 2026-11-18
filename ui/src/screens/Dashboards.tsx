// Dashboards: a grid of saved insights. Every tile re-runs its own query.
import { useEffect, useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { boundsOf } from '../lib/dates'
import { fmtAgo } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { requestBody, validateQuery } from '../lib/insight'
import { HEIGHTS, WIDTHS, addItem, moveItem, ordered, pack, removeItem, resizeItem } from '../lib/layout'
import { can } from '../lib/roles'
import { Link, navigate } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import type { Dashboard, DashboardItem, Insight, Meta } from '../lib/types'
import { rangeLabel } from '../components/DateRange'
import { AsyncView, Button, Card, ConfirmButton, Empty, ErrorNote, Field, Input, Modal, PageHead, Select, Spinner, toast } from '../components/ui'
import { ResultView, queryErrorText } from './Insights'

export const DashboardList = () => {
  const project = useProject()
  const app = useApp()
  const list = useAsync(async (signal) => asList<Dashboard>(await app.api.get(`${project.base}/dashboards`, { signal })), [project.base])
  const canWrite = can(project.role, 'insight.write')
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const create = useAction(async () => {
    const d = await app.api.post<Dashboard>(`${project.base}/dashboards`, { name: name.trim(), items: [] })
    navigate(project.to('dashboards/' + d.id))
  })
  return (
    <>
      <PageHead title={S.dashboards.title} sub={S.dashboards.sub}>
        {canWrite && (
          <Button kind="primary" onClick={() => setOpen(true)}>
            {S.dashboards.create}
          </Button>
        )}
      </PageHead>
      <AsyncView state={list} empty={<Empty title={S.dashboards.empty}>{canWrite ? S.dashboards.emptyHint : S.dashboards.emptyViewer}</Empty>}>
        {(items) => (
          <Card>
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">{S.dashboards.name}</th>
                  <th scope="col" class="num">
                    {S.dashboards.tiles}
                  </th>
                  <th scope="col">{S.common.updated}</th>
                </tr>
              </thead>
              <tbody>
                {[...items]
                  .sort((a, b) => b.updated_at - a.updated_at)
                  .map((d) => (
                    <tr key={d.id}>
                      <td>
                        <Link href={project.to('dashboards/' + d.id)}>{d.name}</Link>
                      </td>
                      <td class="num">{d.items.length}</td>
                      <td class="nowrap">{fmtAgo(d.updated_at)}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </Card>
        )}
      </AsyncView>
      {open && (
        <Modal
          title={S.dashboards.create}
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button onClick={() => setOpen(false)}>{S.common.cancel}</Button>
              <Button kind="primary" type="submit" form="dashboard-create" busy={create.busy}>
                {S.common.create}
              </Button>
            </>
          }
        >
          <form
            id="dashboard-create"
            class="stack"
            onSubmit={(e) => {
              e.preventDefault()
              void create.run()
            }}
          >
            <Field label={S.dashboards.name}>{(id) => <Input id={id} name="name" value={name} onChange={setName} required maxLength={200} />}</Field>
            <ErrorNote error={create.error} />
          </form>
        </Modal>
      )}
    </>
  )
}

const Tile = ({ insight, tick }: { insight: Insight | undefined; tick: number }) => {
  const project = useProject()
  const problem = insight ? validateQuery(insight.kind, insight.query, Date.now()) : null
  const run = useAsync<unknown>(
    (signal) => project.query(insight!.kind, requestBody(insight!.kind, insight!.query, Date.now()), signal),
    [insight?.id, insight?.updated_at, tick],
    !insight || !!problem,
  )
  if (!insight) return <Empty title={S.dashboards.missingInsight} />
  const meta = (run.data as { meta?: Meta } | undefined)?.meta
  return (
    <>
      <header class="tile-head">
        <h2>
          <Link href={project.to('insights/' + insight.id)}>{insight.name}</Link>
        </h2>
        <span class="muted nowrap">{rangeLabel(boundsOf(insight.query))}</span>
      </header>
      <div class={`tile-body${run.loading && run.data !== undefined ? ' refreshing' : ''}`}>
        {problem ? (
          <ErrorNote error={queryErrorText(problem)} />
        ) : run.error ? (
          <ErrorNote error={run.error} onRetry={run.reload} />
        ) : run.data === undefined ? (
          <Spinner />
        ) : (
          <ResultView kind={insight.kind} query={insight.query} result={run.data} compact />
        )}
      </div>
      {meta && (
        <footer class="tile-foot">
          {meta.cached ? S.query.cached : S.dashboards.fresh} · {meta.elapsed_ms} ms
        </footer>
      )}
    </>
  )
}

export const DashboardPage = ({ id }: { id: string }) => {
  const project = useProject()
  const app = useApp()
  const url = `${project.base}/dashboards/${encodeURIComponent(id)}`
  const canWrite = can(project.role, 'insight.write')
  const loaded = useAsync(
    async (signal) => {
      const [dashboard, insights] = await Promise.all([app.api.get<Dashboard>(url, { signal }), app.api.get(`${project.base}/insights`, { signal }).then((x) => asList<Insight>(x))])
      return { dashboard, insights }
    },
    [url],
  )
  const [items, setItems] = useState<DashboardItem[]>([])
  const [name, setName] = useState('')
  const [editing, setEditing] = useState(false)
  const [tick, setTick] = useState(0)
  const [adding, setAdding] = useState('')

  useEffect(() => {
    if (!loaded.data) return
    setItems(pack(ordered(loaded.data.dashboard.items ?? [])))
    setName(loaded.data.dashboard.name)
  }, [loaded.data])

  const save = useAction(async (next: DashboardItem[], nextName: string) => {
    const before = items
    setItems(next)
    try {
      await app.api.put(url, { name: nextName.trim() || name, items: next })
    } catch (e) {
      setItems(before)
      throw e
    }
  })

  if (loaded.error) return <ErrorNote error={loaded.error} onRetry={loaded.reload} />
  if (!loaded.data) return <Spinner />
  const insights = loaded.data.insights
  const byId = new Map(insights.map((i) => [String(i.id), i]))
  const unused = insights.filter((i) => !items.some((it) => String(it.insight_id) == String(i.id)))

  return (
    <>
      <PageHead
        title={
          <span class="title-edit">
            <Link href={project.to('dashboards')} class="crumb">
              {S.dashboards.title}
            </Link>
            <span aria-hidden="true" class="crumb-sep">
              /
            </span>
            {editing ? (
              <input
                class="input title-input"
                value={name}
                aria-label={S.dashboards.name}
                maxLength={200}
                onInput={(e) => setName(e.currentTarget.value)}
                onBlur={() => name.trim() && name.trim() != loaded.data!.dashboard.name && void save.run(items, name)}
              />
            ) : (
              <span>{name}</span>
            )}
          </span>
        }
      >
        <Button onClick={() => setTick(tick + 1)}>{S.common.refresh}</Button>
        {canWrite && (
          <Button kind={editing ? 'primary' : 'default'} aria-pressed={editing} onClick={() => setEditing(!editing)}>
            {editing ? S.dashboards.done : S.dashboards.edit}
          </Button>
        )}
        {canWrite && editing && (
          <ConfirmButton
            label={S.common.delete}
            title={S.dashboards.deleteTitle}
            message={S.dashboards.deleteMessage(name)}
            onConfirm={async () => {
              await app.api.del(url)
              toast(S.dashboards.deleted)
              navigate(project.to('dashboards'))
            }}
          />
        )}
      </PageHead>
      <ErrorNote error={save.error} />
      {editing && (
        <Card class="toolbar-card">
          <form
            class="toolbar"
            onSubmit={(e) => {
              e.preventDefault()
              const pick = adding || String(unused[0]?.id ?? '')
              const insight = byId.get(pick)
              if (!insight) return
              setAdding('')
              void save.run(addItem(items, insight.id, insight.kind == 'retention' || insight.kind == 'sql' ? 12 : 6, 3), name)
            }}
          >
            {unused.length ? (
              <>
                <label for="add-insight">{S.dashboards.addInsight}</label>
                <Select id="add-insight" value={adding || String(unused[0].id)} onChange={setAdding} options={unused.map((i) => [String(i.id), `${i.name} (${S.insights.kinds[i.kind]})`] as const)} />
                <Button kind="primary" type="submit" busy={save.busy}>
                  {S.common.add}
                </Button>
              </>
            ) : (
              <span class="muted">{insights.length ? S.dashboards.allAdded : S.dashboards.noInsights}</span>
            )}
            <span class="spacer" />
            <Link href={project.to('insights')}>{S.dashboards.manageInsights}</Link>
          </form>
        </Card>
      )}
      {items.length == 0 ? (
        <Empty title={S.dashboards.noTiles}>{canWrite ? S.dashboards.noTilesHint : undefined}</Empty>
      ) : (
        <div class="dash-grid">
          {items.map((it, i) => (
            <section class={`tile tile-w${it.w} tile-h${it.h}`} key={String(it.insight_id)} aria-label={byId.get(String(it.insight_id))?.name}>
              <Tile insight={byId.get(String(it.insight_id))} tick={tick} />
              {editing && (
                <div class="tile-tools">
                  <button type="button" class="btn btn-sm" disabled={i == 0 || save.busy} aria-label={S.dashboards.moveBack} title={S.dashboards.moveBack} onClick={() => void save.run(moveItem(items, i, -1), name)}>
                    ←
                  </button>
                  <button
                    type="button"
                    class="btn btn-sm"
                    disabled={i == items.length - 1 || save.busy}
                    aria-label={S.dashboards.moveForward}
                    title={S.dashboards.moveForward}
                    onClick={() => void save.run(moveItem(items, i, 1), name)}
                  >
                    →
                  </button>
                  <Select
                    aria-label={S.dashboards.width}
                    value={String(WIDTHS.includes(it.w as 4) ? it.w : 6) as '4'}
                    onChange={(w) => void save.run(resizeItem(items, i, { w: Number(w) }), name)}
                    options={WIDTHS.map((w) => [String(w) as '4', S.dashboards.widths[w]] as const)}
                  />
                  <Select
                    aria-label={S.dashboards.height}
                    value={String(HEIGHTS.includes(it.h as 2) ? it.h : 3) as '2'}
                    onChange={(h) => void save.run(resizeItem(items, i, { h: Number(h) }), name)}
                    options={HEIGHTS.map((h) => [String(h) as '2', S.dashboards.heights[h]] as const)}
                  />
                  <button type="button" class="btn btn-sm btn-danger" disabled={save.busy} onClick={() => void save.run(removeItem(items, i), name)}>
                    {S.dashboards.removeTile}
                  </button>
                </div>
              )}
            </section>
          ))}
        </div>
      )}
    </>
  )
}
