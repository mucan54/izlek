// Live event feed: newest first from /q/events, auto-refreshing. "Live" means
// the latest manifest, so events show up roughly 90 s after they happen.
import { Fragment } from 'preact'
import { useEffect, useRef, useState } from 'preact/hooks'
import { S } from '../i18n'
import { resolveBounds } from '../lib/dates'
import { QUERY_OPS } from '../lib/filter'
import { fmtDateTime, fmtTime, fmtValue } from '../lib/format'
import { useInterval } from '../lib/hooks'
import { Link } from '../lib/router'
import { useProject } from '../lib/session'
import type { EventRow, EventsResult, Expr, Meta, TimeBounds } from '../lib/types'
import { DateRange } from '../components/DateRange'
import { ExprFilter, querySuggest } from '../components/FilterEditor'
import { Badge, Button, Card, Empty, ErrorNote, MetaLine, PageHead, Spinner, Switch } from '../components/ui'

const PAGE = 100
const REFRESH_MS = 15_000

/** All non-empty fixed columns plus props, for the expanded row. */
const FIELDS: (keyof EventRow)[] = [
  'uuid', 'person_key', 'user_id', 'anon_id', 'session_id', 'platform', 'os', 'os_version', 'device_model', 'device_type',
  'app_version', 'locale', 'country', 'sdk', 'url_path', 'referrer_host', 'screen',
]

export const EventDetail = ({ ev }: { ev: EventRow }) => {
  const props = Object.entries(ev.props ?? {})
  return (
    <div class="event-detail">
      <div>
        <h3>{S.events.props}</h3>
        {props.length == 0 ? (
          <p class="muted">{S.events.noProps}</p>
        ) : (
          <dl class="kv">
            {props.map(([k, v]) => (
              <Fragment key={k}>
                <dt>{k}</dt>
                <dd>{fmtValue(v)}</dd>
              </Fragment>
            ))}
          </dl>
        )}
      </div>
      <div>
        <h3>{S.events.columns}</h3>
        <dl class="kv">
          <dt>ts</dt>
          <dd>{new Date(ev.ts).toISOString()}</dd>
          <dt>recv_ts</dt>
          <dd>{new Date(ev.recv_ts).toISOString()}</dd>
          {FIELDS.filter((f) => ev[f] != null && ev[f] !== '').map((f) => (
            <Fragment key={f}>
              <dt>{f}</dt>
              <dd>{fmtValue(ev[f])}</dd>
            </Fragment>
          ))}
        </dl>
      </div>
    </div>
  )
}

/** Event rows with expandable details; shared with the person timeline. */
export const EventTable = ({ events, fresh, showPerson = true }: { events: EventRow[]; fresh?: Set<string>; showPerson?: boolean }) => {
  const { to } = useProject()
  const [open, setOpen] = useState<Set<string>>(new Set())
  const toggle = (uuid: string) => {
    const next = new Set(open)
    if (next.has(uuid)) next.delete(uuid)
    else next.add(uuid)
    setOpen(next)
  }
  const cols = showPerson ? 6 : 5
  return (
    <div class="table-wrap">
      <table class="table events">
        <thead>
          <tr>
            <th scope="col" class="col-toggle">
              <span class="sr-only">{S.events.details}</span>
            </th>
            <th scope="col">{S.events.time}</th>
            <th scope="col">{S.events.event}</th>
            {showPerson && <th scope="col">{S.events.person}</th>}
            <th scope="col">{S.events.platform}</th>
            <th scope="col">{S.events.where}</th>
          </tr>
        </thead>
        <tbody>
          {events.map((ev) => {
            const isOpen = open.has(ev.uuid)
            return (
              <Fragment key={ev.uuid}>
                <tr class={`${isOpen ? 'row-open' : ''}${fresh?.has(ev.uuid) ? ' row-new' : ''}`}>
                  <td class="col-toggle">
                    <button type="button" class="btn btn-ghost btn-sm" aria-expanded={isOpen} aria-label={S.events.details} onClick={() => toggle(ev.uuid)}>
                      {isOpen ? '▾' : '▸'}
                    </button>
                  </td>
                  <td class="nowrap" title={new Date(ev.ts).toISOString()}>
                    {fmtDateTime(ev.ts)}
                  </td>
                  <td>
                    <span class="event-name">{ev.event}</span>
                  </td>
                  {showPerson && (
                    <td>
                      <Link href={to('persons/' + encodeURIComponent(ev.person_key))} class="mono">
                        {ev.person_key.length > 22 ? ev.person_key.slice(0, 20) + '…' : ev.person_key}
                      </Link>
                    </td>
                  )}
                  <td>{[ev.platform, ev.country].filter(Boolean).join(' · ') || '—'}</td>
                  <td class="ellipsis">{ev.screen ?? ev.url_path ?? '—'}</td>
                </tr>
                {isOpen && (
                  <tr class="row-detail">
                    <td colSpan={cols}>
                      <EventDetail ev={ev} />
                    </td>
                  </tr>
                )}
              </Fragment>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

export const Events = () => {
  const project = useProject()
  const [bounds, setBounds] = useState<TimeBounds>({ range: '24h' })
  const [filter, setFilter] = useState<Expr | undefined>()
  const [live, setLive] = useState(true)
  const [events, setEvents] = useState<EventRow[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [meta, setMeta] = useState<Meta>()
  const [error, setError] = useState<Error>()
  const [loading, setLoading] = useState(false)
  const [more, setMore] = useState(false)
  const [fresh, setFresh] = useState<Set<string>>(new Set())
  const [updated, setUpdated] = useState<number>()
  const generation = useRef(0)
  const filterKey = JSON.stringify([bounds, filter])

  const fetchPage = (cursorArg?: string) =>
    project.query<EventsResult>('events', {
      ...resolveBounds(bounds, Date.now()),
      ...(filter ? { filter } : {}),
      limit: PAGE,
      order: 'desc',
      ...(cursorArg ? { cursor: cursorArg } : {}),
    })

  /** (Re)loads the first page; with `merge`, new events are added on top of what is shown. */
  const refresh = async (merge: boolean) => {
    const gen = ++generation.current
    setLoading(true)
    try {
      const res = await fetchPage()
      if (gen != generation.current) return
      setError(undefined)
      setMeta(res.meta)
      setUpdated(Date.now())
      if (merge && events) {
        const known = new Set(events.map((e) => e.uuid))
        const added = res.events.filter((e) => !known.has(e.uuid))
        setFresh(new Set(added.map((e) => e.uuid)))
        if (added.length) setEvents([...added, ...events])
      } else {
        setFresh(new Set())
        setEvents(res.events)
        setCursor(res.next_cursor)
      }
    } catch (e) {
      if (gen == generation.current) setError(e as Error)
    }
    if (gen == generation.current) setLoading(false)
  }

  useEffect(() => {
    setEvents(null)
    void refresh(false)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filterKey, project.project.id])

  useInterval(() => void refresh(true), REFRESH_MS, live && !more)

  const loadMore = async () => {
    if (!cursor) return
    setMore(true)
    try {
      const res = await fetchPage(cursor)
      setEvents((cur) => [...(cur ?? []), ...res.events])
      setCursor(res.next_cursor)
    } catch (e) {
      setError(e as Error)
    }
    setMore(false)
  }

  return (
    <>
      <PageHead
        title={S.events.title}
        sub={
          <>
            <Badge kind={live ? 'ok' : 'neutral'}>{live ? S.events.live : S.events.paused}</Badge> {S.events.lagNote}
            {updated && <span class="muted"> · {S.events.updatedAt(fmtTime(updated))}</span>}
          </>
        }
      >
        <Switch checked={live} onChange={setLive} label={S.events.autoRefresh} />
        <Button busy={loading} onClick={() => void refresh(false)}>
          {S.common.refresh}
        </Button>
      </PageHead>
      <Card class="toolbar-card">
        <div class="toolbar">
          <DateRange value={bounds} onChange={setBounds} hours />
        </div>
        <ExprFilter value={filter} onChange={setFilter} ops={QUERY_OPS} suggest={querySuggest(project.schema)} />
      </Card>
      <ErrorNote error={error} onRetry={() => void refresh(false)} />
      {events === null ? (
        !error && <Spinner />
      ) : events.length == 0 ? (
        <Empty title={S.events.empty}>{S.events.emptyHint}</Empty>
      ) : (
        <Card>
          <EventTable events={events} fresh={fresh} />
          <div class="table-foot">
            <span class="muted">{S.events.shown(events.length)}</span>
            {cursor && (
              <Button busy={more} onClick={() => void loadMore()}>
                {S.common.loadMore}
              </Button>
            )}
            <MetaLine meta={meta} />
          </div>
        </Card>
      )}
    </>
  )
}
