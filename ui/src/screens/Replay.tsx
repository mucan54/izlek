// Session replay player: downloads the session's chunks, decodes them and
// plays the wireframe on a canvas with the session's events on the seek bar.
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { S } from '../i18n'
import { resolveBounds, utcDay, DAY_MS } from '../lib/dates'
import { fmtBytes, fmtClock, fmtDateTime, fmtDuration } from '../lib/format'
import { useAsync, useWidth } from '../lib/hooks'
import { Link, useLocation } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import type { EventRow, EventsResult } from '../lib/types'
import { draw, fitScale } from '../replay/draw'
import { ReplayLoadError, isZstd, loadChunk, type ChunkRef } from '../replay/fetch'
import { advance, buildTimeline, progress, seek, skipIdle, type PlayerState, type Timeline } from '../replay/player'
import { Badge, Button, Card, Checkbox, Empty, ErrorNote, PageHead, Spinner } from '../components/ui'
import { REPLAY_EVENT } from './Persons'

const SPEEDS = [1, 2, 4] as const
const MAX_HEIGHT = 620

interface Loaded {
  timeline: Timeline
  refs: ChunkRef[]
  failed: ReplayLoadError[]
  events: EventRow[]
}

/** Runs `task` over `items` with at most `n` in flight; results keep the input order. */
const pool = async <T, R>(items: T[], n: number, task: (item: T) => Promise<R>): Promise<R[]> => {
  const out: R[] = new Array(items.length)
  let next = 0
  await Promise.all(
    Array.from({ length: Math.min(n, items.length) }, async () => {
      while (next < items.length) {
        const i = next++
        out[i] = await task(items[i])
      }
    }),
  )
  return out
}

export const ReplayPage = ({ session }: { session: string }) => {
  const project = useProject()
  const app = useApp()
  const person = useLocation().query.get('person')
  const [done, setDone] = useState(0)

  const loaded = useAsync<Loaded | null>(
    async (signal) => {
      setDone(0)
      const index = await project.query<EventsResult>(
        'events',
        {
          ...resolveBounds({ range: '90d' }, Date.now()),
          filter: { and: [{ prop: 'session_id', op: 'eq', value: session }, { prop: 'event', op: 'eq', value: REPLAY_EVENT }] },
          limit: 1000,
          order: 'asc',
        },
        signal,
      )
      const refs: ChunkRef[] = index.events
        .filter((e) => typeof e.props?.key == 'string')
        .map((e) => ({
          seq: Number(e.props.seq) || 0,
          key: String(e.props.key),
          ts: e.ts,
          durationMs: Number(e.props.duration_ms) || 0,
          bytes: Number(e.props.bytes) || 0,
          zstd: isZstd(e.props.encoding, String(e.props.key)),
        }))
        .sort((a, b) => a.seq - b.seq)
      if (!refs.length) return null

      const failed: ReplayLoadError[] = []
      const chunks = await pool(refs, 4, async (ref) => {
        try {
          const chunk = await loadChunk(ref, (key) => app.api.bytes(`${project.base}/q/replay?key=${encodeURIComponent(key)}`, { signal }))
          setDone((n) => n + 1)
          return { chunk, durationMs: ref.durationMs }
        } catch (e) {
          if (e instanceof ReplayLoadError) {
            failed.push(e)
            return null
          }
          throw e
        }
      })
      const timeline = buildTimeline(chunks.filter((c) => c !== null))

      // the session's analytics events, for the ticks on the seek bar
      let events: EventRow[] = []
      if (timeline.frames.length) {
        try {
          const r = await project.query<EventsResult>(
            'events',
            {
              from: utcDay(timeline.start - DAY_MS),
              to: utcDay(timeline.end + DAY_MS),
              filter: { and: [{ prop: 'session_id', op: 'eq', value: session }, { prop: 'event', op: 'ne', value: REPLAY_EVENT }] },
              limit: 1000,
              order: 'asc',
            },
            signal,
          )
          events = r.events.filter((e) => e.ts >= timeline.start - 1000 && e.ts <= timeline.end + 1000)
        } catch (e) {
          if ((e as Error).name == 'AbortError') throw e
          // the overlay is optional; playback works without it
        }
      }
      return { timeline, refs, failed, events }
    },
    [project.base, session],
  )

  const back = person ? project.to('persons/' + encodeURIComponent(person)) : project.to('persons')
  return (
    <>
      <PageHead
        title={
          <span class="title-edit">
            <Link href={back} class="crumb">
              {person ?? S.persons.title}
            </Link>
            <span aria-hidden="true" class="crumb-sep">
              /
            </span>
            <span>{S.replay.title}</span>
          </span>
        }
        sub={<span class="mono">{session}</span>}
      />
      {loaded.error ? (
        <ErrorNote error={loaded.error} onRetry={loaded.reload} />
      ) : loaded.data === undefined ? (
        <Spinner label={done ? S.replay.downloading(done) : S.replay.loading} />
      ) : loaded.data === null ? (
        <Empty title={S.replay.noChunks}>{S.replay.noChunksHint}</Empty>
      ) : (
        <>
          {loaded.data.failed.length > 0 && <FailedNote failed={loaded.data.failed} total={loaded.data.refs.length} />}
          {loaded.data.timeline.frames.length > 0 ? <Player data={loaded.data} /> : loaded.data.failed.length == 0 && <Empty title={S.replay.emptyRecording} />}
        </>
      )}
    </>
  )
}

const FailedNote = ({ failed, total }: { failed: ReplayLoadError[]; total: number }) => {
  const zstd = failed.filter((f) => f.code == 'zstd_unsupported')
  const other = failed.filter((f) => f.code != 'zstd_unsupported')
  return (
    <div class="note note-warn stack" role="alert">
      {zstd.length > 0 && (
        <p>
          <strong>{S.replay.zstdTitle(zstd.length, total)}</strong> {S.replay.zstdBody}
        </p>
      )}
      {other.length > 0 && <p>{S.replay.chunkErrors(other.map((f) => f.seq).join(', '))}</p>}
    </div>
  )
}

const Player = ({ data }: { data: Loaded }) => {
  const { timeline, events } = data
  const duration = timeline.end - timeline.start
  const [boxRef, boxWidth] = useWidth<HTMLDivElement>(480)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const stateRef = useRef<PlayerState>(seek(timeline, timeline.start))
  const timeRef = useRef(timeline.start)
  const [time, setTime] = useState(timeline.start)
  const [playing, setPlaying] = useState(false)
  const [speed, setSpeed] = useState<(typeof SPEEDS)[number]>(1)
  const [skip, setSkip] = useState(true)
  const [screen, setScreen] = useState('')
  const speedRef = useRef(speed)
  speedRef.current = speed
  const skipRef = useRef(skip)
  skipRef.current = skip

  const paint = () => {
    const canvas = canvasRef.current
    if (!canvas) return
    const state = stateRef.current
    const w = state.scene.w || 390
    const h = state.scene.h || 844
    const dpr = Math.min(window.devicePixelRatio || 1, 2)
    const scale = Math.min(1.5, fitScale(w, h, boxWidth, MAX_HEIGHT))
    const cssW = Math.round(w * scale)
    const cssH = Math.round(h * scale)
    if (canvas.width != Math.round(cssW * dpr) || canvas.height != Math.round(cssH * dpr)) {
      canvas.width = Math.round(cssW * dpr)
      canvas.height = Math.round(cssH * dpr)
      canvas.style.width = cssW + 'px'
      canvas.style.height = cssH + 'px'
    }
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    draw(ctx, state, scale, { width: cssW, height: cssH, dpr, screenLabel: S.replay.screen, font: getComputedStyle(canvas).fontFamily || 'sans-serif' })
    canvas.dataset.time = String(Math.round(state.time - timeline.start))
  }

  const goTo = (t: number) => {
    const clamped = Math.max(timeline.start, Math.min(timeline.end, t))
    timeRef.current = clamped
    stateRef.current = advance(timeline, stateRef.current, clamped)
    setTime(clamped)
    setScreen(stateRef.current.scene.screen)
    paint()
  }

  // first frame, and repaint when the available width changes
  useEffect(() => {
    goTo(timeRef.current)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [boxWidth])

  useEffect(() => {
    if (!playing) return
    let raf = 0
    let last = performance.now()
    let shown = 0
    const tick = (now: number) => {
      const dt = Math.min(100, now - last) // a background tab must not fast-forward on return
      last = now
      let t = timeRef.current + dt * speedRef.current
      if (skipRef.current) t = skipIdle(timeline, t)
      if (t >= timeline.end) {
        goTo(timeline.end)
        setPlaying(false)
        return
      }
      timeRef.current = t
      stateRef.current = advance(timeline, stateRef.current, t)
      paint()
      if (now - shown > 120) {
        shown = now
        setTime(t)
        setScreen(stateRef.current.scene.screen)
      }
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [playing])

  const toggle = () => {
    if (!playing && timeRef.current >= timeline.end) goTo(timeline.start)
    setPlaying(!playing)
  }

  const pct = (t: number) => progress(timeline, t) * 100 + '%'
  const idleTotal = useMemo(() => timeline.idle.reduce((a, s) => a + (s.to - s.from), 0), [timeline])
  const current = events.reduce<EventRow | null>((cur, e) => (e.ts <= time ? e : cur), null)

  return (
    <div class="replay">
      <Card class="replay-stage">
        <div
          class="replay-view"
          ref={boxRef}
          onKeyDown={(e) => {
            if (e.key == ' ' && (e.target as HTMLElement).tagName != 'BUTTON' && (e.target as HTMLElement).tagName != 'INPUT') {
              e.preventDefault()
              toggle()
            }
          }}
        >
          <canvas ref={canvasRef} class="replay-canvas" role="img" aria-label={S.replay.canvasLabel(screen, fmtClock(time - timeline.start))} onClick={toggle} />
        </div>
        <div class="replay-controls">
          <Button kind="primary" onClick={toggle} aria-label={playing ? S.replay.pause : S.replay.play} class="replay-play">
            {playing ? '❚❚ ' + S.replay.pause : '▶ ' + S.replay.play}
          </Button>
          <span class="replay-clock">
            {fmtClock(time - timeline.start)} / {fmtClock(duration)}
          </span>
          <div class="seek">
            <div class="seek-marks" aria-hidden="true">
              {timeline.idle.map((s, i) => (
                <span class="seek-idle" key={'i' + i} style={{ left: pct(s.from), width: (progress(timeline, s.to) - progress(timeline, s.from)) * 100 + '%' }} />
              ))}
              {events.map((e) => (
                <span class="seek-event" key={e.uuid} style={{ left: pct(e.ts) }} title={`${e.event} · ${fmtClock(e.ts - timeline.start)}`} />
              ))}
              {timeline.screens.map((s, i) => (
                <span class="seek-screen" key={'s' + i} style={{ left: pct(s.t) }} title={`${S.replay.screen}: ${s.name}`} />
              ))}
            </div>
            <input
              type="range"
              class="seek-range"
              min={0}
              max={Math.max(1, duration)}
              step={100}
              value={time - timeline.start}
              aria-label={S.replay.seek}
              aria-valuetext={`${fmtClock(time - timeline.start)} / ${fmtClock(duration)}`}
              onInput={(e) => goTo(timeline.start + Number(e.currentTarget.value))}
            />
          </div>
          <div class="speed" role="group" aria-label={S.replay.speed}>
            {SPEEDS.map((s) => (
              <button type="button" key={s} class={`btn btn-sm${speed == s ? ' btn-primary' : ''}`} aria-pressed={speed == s} onClick={() => setSpeed(s)}>
                {s}×
              </button>
            ))}
          </div>
          <Checkbox checked={skip} onChange={setSkip} label={S.replay.skipIdle} />
        </div>
        <p class="seek-legend muted small">
          <span class="key key-screen" aria-hidden="true" /> {S.replay.legendScreen}
          <span class="key key-event" aria-hidden="true" /> {S.replay.legendEvent}
          <span class="key key-idle" aria-hidden="true" /> {S.replay.legendIdle}
        </p>
      </Card>
      <div class="replay-side">
        <Card title={S.replay.session}>
          <dl class="kv">
            <dt>{S.replay.started}</dt>
            <dd>{fmtDateTime(timeline.start)}</dd>
            <dt>{S.persons.duration}</dt>
            <dd>
              {fmtDuration(duration / 1000)} <span class="muted">({S.replay.idleShare(fmtDuration(idleTotal / 1000))})</span>
            </dd>
            <dt>{S.replay.chunks}</dt>
            <dd>
              {data.refs.length} · {fmtBytes(data.refs.reduce((a, r) => a + r.bytes, 0))}
            </dd>
            <dt>{S.replay.screen}</dt>
            <dd>{screen || '—'}</dd>
          </dl>
        </Card>
        <Card title={S.replay.events}>
          {events.length == 0 ? (
            <p class="muted">{S.replay.noEvents}</p>
          ) : (
            <ol class="replay-events">
              {events.map((e) => (
                <li key={e.uuid}>
                  <button type="button" class={`replay-event${current == e ? ' replay-event-current' : ''}`} aria-current={current == e ? 'true' : undefined} onClick={() => goTo(e.ts)}>
                    <span class="replay-event-time">{fmtClock(e.ts - timeline.start)}</span>
                    <span class="event-name">{e.event}</span>
                    {e.screen && <Badge>{e.screen}</Badge>}
                  </button>
                </li>
              ))}
            </ol>
          )}
        </Card>
      </div>
    </div>
  )
}
