// Person lookup and the person page: properties, replay sessions and the
// event timeline.
import { Fragment } from 'preact'
import { useEffect, useState } from 'preact/hooks'
import { S } from '../i18n'
import { resolveBounds } from '../lib/dates'
import { fmtAgo, fmtBytes, fmtDateTime, fmtDuration, fmtInt, fmtValue } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { can } from '../lib/roles'
import { Link, navigate } from '../lib/router'
import { useProject } from '../lib/session'
import type { EventRow, EventsResult, Expr, SqlResult, TimeBounds } from '../lib/types'
import { DateRange } from '../components/DateRange'
import { AsyncView, Badge, Button, Card, Empty, ErrorNote, Input, PageHead, Select, Spinner } from '../components/ui'
import { EventTable } from './Events'

export const REPLAY_EVENT = 'session_replay_chunk'

type IdKind = 'user_id' | 'anon_id' | 'person_key'

export const Persons = () => {
  const project = useProject()
  const [kind, setKind] = useState<IdKind>('user_id')
  const [value, setValue] = useState('')
  const [missing, setMissing] = useState(false)
  const find = useAction(async () => {
    setMissing(false)
    const v = value.trim()
    if (kind == 'person_key') return navigate(project.to('persons/' + encodeURIComponent(v)))
    // resolve through the newest event carrying this id: an anon id may have been merged into a user
    const res = await project.query<EventsResult>('events', {
      ...resolveBounds({ range: '90d' }, Date.now()),
      filter: { prop: kind, op: 'eq', value: v },
      limit: 1,
      order: 'desc',
    })
    if (res.events.length) navigate(project.to('persons/' + encodeURIComponent(res.events[0].person_key)))
    else setMissing(true)
  })
  const recent = useAsync<SqlResult>(
    (signal) =>
      project.query(
        'sql',
        {
          ...resolveBounds({ range: '7d' }, Date.now()),
          sql: 'SELECT person_key, count(*) AS events, max(ts) AS last_seen FROM events GROUP BY person_key ORDER BY last_seen DESC LIMIT 50',
        },
        signal,
      ),
    [project.base],
  )
  return (
    <>
      <PageHead title={S.persons.title} sub={S.persons.sub} />
      <Card title={S.persons.lookup}>
        <form
          class="toolbar"
          onSubmit={(e) => {
            e.preventDefault()
            if (value.trim()) void find.run()
          }}
        >
          <Select
            aria-label={S.persons.idKind}
            value={kind}
            onChange={setKind}
            options={[
              ['user_id', S.persons.userId],
              ['anon_id', S.persons.anonId],
              ['person_key', S.persons.personKey],
            ]}
          />
          <Input name="person" value={value} onChange={setValue} aria-label={S.persons.idValue} placeholder={kind == 'person_key' ? 'u:42' : kind == 'user_id' ? '42' : '0190…'} class="grow" required />
          <Button kind="primary" type="submit" busy={find.busy}>
            {S.persons.find}
          </Button>
        </form>
        <ErrorNote error={find.error} />
        {missing && (
          <p class="note note-warn" role="status">
            {S.persons.notFound}
          </p>
        )}
      </Card>
      <Card title={S.persons.recent}>
        <AsyncView state={recent} isEmpty={(r) => r.rows.length == 0} empty={<Empty title={S.persons.recentEmpty} />}>
          {(r) => (
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">{S.persons.personKey}</th>
                  <th scope="col" class="num">
                    {S.persons.eventCount}
                  </th>
                  <th scope="col">{S.persons.lastSeen}</th>
                </tr>
              </thead>
              <tbody>
                {r.rows.map((row) => (
                  <tr key={String(row[0])}>
                    <td>
                      <Link class="mono" href={project.to('persons/' + encodeURIComponent(String(row[0])))}>
                        {String(row[0])}
                      </Link>
                    </td>
                    <td class="num">{typeof row[1] == 'number' ? fmtInt(row[1]) : fmtValue(row[1])}</td>
                    <td class="nowrap">{typeof row[2] == 'number' ? fmtAgo(row[2]) : fmtValue(row[2])}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </AsyncView>
      </Card>
    </>
  )
}

interface ReplaySession {
  id: string
  start: number
  end: number
  chunks: number
  bytes: number
}

/** Groups `session_replay_chunk` index events into sessions, newest first. */
export const groupSessions = (events: EventRow[]): ReplaySession[] => {
  const map = new Map<string, ReplaySession>()
  for (const e of events) {
    if (!e.session_id) continue
    const duration = Number(e.props?.duration_ms) || 0
    const s = map.get(e.session_id) ?? { id: e.session_id, start: e.ts, end: e.ts + duration, chunks: 0, bytes: 0 }
    s.start = Math.min(s.start, e.ts)
    s.end = Math.max(s.end, e.ts + duration)
    s.chunks++
    s.bytes += Number(e.props?.bytes) || 0
    map.set(e.session_id, s)
  }
  return [...map.values()].sort((a, b) => b.start - a.start)
}

const sqlString = (s: string): string => "'" + s.replace(/'/g, "''") + "'"

const PAGE = 100

export const PersonPage = ({ personKey }: { personKey: string }) => {
  const project = useProject()
  const isUser = personKey.startsWith('u:')
  const rawId = personKey.slice(2)
  const mine: Expr = { prop: 'person_key', op: 'eq', value: personKey }

  const props = useAsync<Record<string, unknown>>(
    async (signal) => {
      const r = await project.query<SqlResult>('sql', { ...resolveBounds({ range: '1d' }, Date.now()), sql: `SELECT props FROM persons WHERE person_key = ${sqlString(personKey)} LIMIT 1` }, signal)
      const cell = r.rows[0]?.[r.columns.findIndex((c) => c.name == 'props')] ?? r.rows[0]?.[0]
      if (cell && typeof cell == 'object') return cell as Record<string, unknown>
      if (typeof cell == 'string') {
        try {
          const parsed = JSON.parse(cell) as unknown
          return parsed && typeof parsed == 'object' ? (parsed as Record<string, unknown>) : {}
        } catch {
          return {}
        }
      }
      return {}
    },
    [project.base, personKey],
  )

  const replays = useAsync(
    async (signal) => {
      const r = await project.query<EventsResult>(
        'events',
        { ...resolveBounds({ range: '90d' }, Date.now()), filter: { and: [mine, { prop: 'event', op: 'eq', value: REPLAY_EVENT }] }, limit: 1000, order: 'desc' },
        signal,
      )
      return groupSessions(r.events)
    },
    [project.base, personKey],
  )

  const [bounds, setBounds] = useState<TimeBounds>({ range: '30d' })
  const [events, setEvents] = useState<EventRow[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [error, setError] = useState<Error>()
  const [more, setMore] = useState(false)
  const boundsKey = JSON.stringify(bounds)
  const page = (c?: string, signal?: AbortSignal) =>
    project.query<EventsResult>(
      'events',
      {
        ...resolveBounds(bounds, Date.now()),
        filter: { and: [mine, { prop: 'event', op: 'ne', value: REPLAY_EVENT }] },
        limit: PAGE,
        order: 'desc',
        ...(c ? { cursor: c } : {}),
      },
      signal,
    )
  useEffect(() => {
    const ctl = new AbortController()
    setEvents(null)
    setError(undefined)
    page(undefined, ctl.signal).then(
      (r) => {
        setEvents(r.events)
        setCursor(r.next_cursor)
      },
      (e: Error) => {
        if (!ctl.signal.aborted) setError(e)
      },
    )
    return () => ctl.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [boundsKey, personKey, project.base])

  const latest = events?.[0]
  const facts: [string, unknown][] = latest
    ? [
        ['user_id', latest.user_id],
        ['anon_id', latest.anon_id],
        ['platform', latest.platform],
        ['os', [latest.os, latest.os_version].filter(Boolean).join(' ') || null],
        ['device_model', latest.device_model],
        ['app_version', latest.app_version],
        ['locale', latest.locale],
        ['country', latest.country],
      ]
    : []

  return (
    <>
      <PageHead
        title={
          <span class="title-edit">
            <Link href={project.to('persons')} class="crumb">
              {S.persons.title}
            </Link>
            <span aria-hidden="true" class="crumb-sep">
              /
            </span>
            <span class="mono">{personKey}</span>
            <Badge>{isUser ? S.persons.identified : S.persons.anonymous}</Badge>
          </span>
        }
      >
        {can(project.role, 'admin') && (
          <Link class="btn btn-default" href={project.to('deletions') + `?${isUser ? 'user_id' : 'anon_id'}=${encodeURIComponent(rawId)}`}>
            {S.persons.requestDeletion}
          </Link>
        )}
      </PageHead>
      <div class="split">
        <Card title={S.persons.props}>
          <AsyncView state={props} isEmpty={(p) => Object.keys(p).length == 0} empty={<p class="muted">{S.persons.noProps}</p>}>
            {(p) => (
              <dl class="kv">
                {Object.entries(p).map(([k, v]) => (
                  <Fragment key={k}>
                    <dt>{k}</dt>
                    <dd>{fmtValue(v)}</dd>
                  </Fragment>
                ))}
              </dl>
            )}
          </AsyncView>
          {facts.length > 0 && (
            <>
              <h3 class="sub-title">{S.persons.latestContext}</h3>
              <dl class="kv">
                {facts
                  .filter(([, v]) => v != null && v !== '')
                  .map(([k, v]) => (
                    <Fragment key={k}>
                      <dt>{k}</dt>
                      <dd>{fmtValue(v)}</dd>
                    </Fragment>
                  ))}
              </dl>
            </>
          )}
        </Card>
        <Card title={S.persons.replays}>
          <AsyncView state={replays} empty={<p class="muted">{S.persons.noReplays}</p>}>
            {(list) => (
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.persons.sessionStart}</th>
                    <th scope="col" class="num">
                      {S.persons.duration}
                    </th>
                    <th scope="col" class="num">
                      {S.persons.size}
                    </th>
                    <th scope="col">
                      <span class="sr-only">{S.replay.play}</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((s) => (
                    <tr key={s.id}>
                      <td class="nowrap" title={s.id}>
                        {fmtDateTime(s.start)}
                      </td>
                      <td class="num">{fmtDuration((s.end - s.start) / 1000)}</td>
                      <td class="num">{fmtBytes(s.bytes)}</td>
                      <td class="cell-action">
                        <Link class="btn btn-sm btn-default" href={project.to('replay/' + s.id) + '?person=' + encodeURIComponent(personKey)}>
                          ▶ {S.replay.play}
                        </Link>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </AsyncView>
        </Card>
      </div>
      <Card title={S.persons.timeline} actions={<DateRange value={bounds} onChange={setBounds} />}>
        <ErrorNote error={error} />
        {events === null ? (
          !error && <Spinner />
        ) : events.length == 0 ? (
          <Empty title={S.persons.noEvents}>{S.persons.noEventsHint}</Empty>
        ) : (
          <>
            <EventTable events={events} showPerson={false} />
            <div class="table-foot">
              <span class="muted">{S.events.shown(events.length)}</span>
              {cursor && (
                <Button
                  busy={more}
                  onClick={async () => {
                    setMore(true)
                    try {
                      const r = await page(cursor)
                      setEvents([...events, ...r.events])
                      setCursor(r.next_cursor)
                    } catch (e) {
                      setError(e as Error)
                    }
                    setMore(false)
                  }}
                >
                  {S.common.loadMore}
                </Button>
              )}
            </div>
          </>
        )}
      </Card>
    </>
  )
}
