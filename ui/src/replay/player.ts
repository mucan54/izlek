// Replay timeline and player state machine. Pure data: no DOM, no timers, so
// it runs under node:test. The canvas renderer (draw.ts) and the screen
// (screens/Replay.tsx) sit on top.
import { applyFrame, newScene, FULL_SNAPSHOT, SCREEN_CHANGE, TOUCH, VIEWPORT, type Chunk, type Frame, type Scene } from './lar1'

export interface TimedFrame {
  /** absolute time, unix ms (device clock) */
  t: number
  frame: Frame
}

/** A point the player can start from without replaying everything before it. */
export interface Keyframe {
  /** index into `frames` of a full_snapshot */
  index: number
  /** screen name and viewport in effect just before the snapshot */
  screen: string
  w: number
  h: number
}

export interface Span {
  from: number
  to: number
}

export interface Timeline {
  start: number
  end: number
  frames: TimedFrame[]
  keyframes: Keyframe[]
  /** screen changes, for markers on the seek bar */
  screens: { t: number; name: string }[]
  /** stretches with no frames at all, longer than the inactivity threshold */
  idle: Span[]
}

export interface ChunkInput {
  chunk: Chunk
  /** `props.duration_ms` of the index event, when known */
  durationMs?: number
}

export const DEFAULT_IDLE_MS = 3000

/**
 * Merges decoded chunks into one timeline, ordered by `seq`. A frame's time
 * is its absolute `t` when the decoder supplied one (`decodeChunk(bytes,
 * true)`, which also counts the `dt` of skipped unknown frames); otherwise
 * `start_ts` plus the running sum of `dt`.
 */
export const buildTimeline = (inputs: ChunkInput[], idleMs = DEFAULT_IDLE_MS): Timeline => {
  const sorted = [...inputs].sort((a, b) => a.chunk.seq - b.chunk.seq)
  const frames: TimedFrame[] = []
  let end = 0
  for (const { chunk, durationMs } of sorted) {
    let t = chunk.start_ts
    for (const frame of chunk.frames) {
      const abs = (frame as Frame & { t?: number }).t
      t = abs ?? t + frame.dt
      frames.push({ t, frame })
    }
    end = Math.max(end, t, chunk.start_ts + (durationMs ?? 0))
  }
  // device clocks can step backwards between chunks; keep time monotonic
  for (let i = 1; i < frames.length; i++) if (frames[i].t < frames[i - 1].t) frames[i].t = frames[i - 1].t

  const start = frames.length ? frames[0].t : sorted.length ? sorted[0].chunk.start_ts : 0
  end = Math.max(end, start)

  const keyframes: Keyframe[] = []
  const screens: { t: number; name: string }[] = []
  let screen = ''
  let w = 0
  let h = 0
  frames.forEach(({ t, frame }, index) => {
    if (frame.kind == SCREEN_CHANGE) {
      if (frame.name != screen) screens.push({ t, name: frame.name })
      screen = frame.name
    } else if (frame.kind == VIEWPORT) {
      w = frame.w
      h = frame.h
    } else if (frame.kind == FULL_SNAPSHOT) keyframes.push({ index, screen, w, h })
  })

  const idle: Span[] = []
  let prev = start
  for (const f of frames) {
    if (f.t - prev > idleMs) idle.push({ from: prev, to: f.t })
    prev = f.t
  }
  if (end - prev > idleMs) idle.push({ from: prev, to: end })

  return { start, end, frames, keyframes, screens, idle }
}

export interface Touch {
  x: number
  y: number
  /** 0 down · 1 move · 2 up */
  phase: number
  t: number
}

export interface PlayerState {
  scene: Scene
  /** index of the next frame to apply */
  next: number
  time: number
  /** the current gesture: points since the last `down` (ends with the `up`) */
  gesture: Touch[]
  /** time of the last screen change applied, for the on-canvas banner */
  screenAt: number
}

const applyTimed = (state: PlayerState, { t, frame }: TimedFrame): void => {
  if (frame.kind == TOUCH) {
    const touch = { x: frame.x, y: frame.y, phase: frame.phase, t }
    if (frame.phase == 0) state.gesture = [touch]
    else state.gesture.push(touch)
    // a long drag would otherwise grow without bound
    if (state.gesture.length > 64) state.gesture.splice(1, state.gesture.length - 64)
    return
  }
  if (frame.kind == SCREEN_CHANGE && frame.name != state.scene.screen) state.screenAt = t
  applyFrame(state.scene, frame)
}

/**
 * State at time `t`, built from the nearest full snapshot at or before it
 * (or from the very first frame when there is none).
 */
export const seek = (tl: Timeline, t: number): PlayerState => {
  const time = Math.max(tl.start, Math.min(tl.end, t))
  const state: PlayerState = { scene: newScene(), next: 0, time, gesture: [], screenAt: -Infinity }
  let key: Keyframe | undefined
  for (const k of tl.keyframes) {
    if (tl.frames[k.index].t > time) break
    key = k
  }
  if (key) {
    state.scene.screen = key.screen
    state.scene.w = key.w
    state.scene.h = key.h
    state.next = key.index
    // the banner belongs to the screen change that led to this snapshot
    for (const s of tl.screens) if (s.t <= tl.frames[key.index].t) state.screenAt = s.t
  }
  return advance(tl, state, time)
}

/** Applies every frame up to and including time `t`. Going backwards re-seeks. */
export const advance = (tl: Timeline, state: PlayerState, t: number): PlayerState => {
  const time = Math.max(tl.start, Math.min(tl.end, t))
  if (time < state.time) return seek(tl, time)
  while (state.next < tl.frames.length && tl.frames[state.next].t <= time) applyTimed(state, tl.frames[state.next++])
  state.time = time
  return state
}

/**
 * Where playback should jump to when it is inside an idle stretch: shortly
 * before the stretch ends. Returns `t` unchanged when not idle (or when the
 * stretch is nearly over anyway).
 */
export const skipIdle = (tl: Timeline, t: number, leadMs = 400): number => {
  for (const span of tl.idle) {
    if (t >= span.from + leadMs && t < span.to - leadMs) return span.to - leadMs
    if (span.from > t) break
  }
  return t
}

/** Position of time `t` on the seek bar, 0…1. */
export const progress = (tl: Timeline, t: number): number => (tl.end > tl.start ? Math.max(0, Math.min(1, (t - tl.start) / (tl.end - tl.start))) : 0)
