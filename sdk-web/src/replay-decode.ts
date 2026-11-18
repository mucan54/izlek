// LAR1 decoder (spec/replay.md). Standalone on purpose: the dashboard player
// imports this file without pulling in any capture code.

import type { Chunk, Frame, RNode } from './replay-format'

export type { Chunk, Frame, RNode } from './replay-format'

/** A decoded frame with its absolute time (unix ms), when requested. */
export type TimedFrame = Frame & { t?: number }

export class ReplayDecodeError extends Error {}

const fail = (what: string): never => {
  throw new ReplayDecodeError('LAR1: ' + what)
}

/**
 * Decode one uncompressed chunk.
 *
 * Unknown frame kinds are skipped. Their `dt` still advances the clock, so
 * `dt` of the remaining frames no longer adds up to wall time; pass
 * `withTime` to get an absolute `t` on every frame, which does account for
 * skipped frames. Unknown trailing bytes inside a known frame are ignored.
 */
export const decodeChunk = (bytes: Uint8Array, withTime = false): Chunk & { frames: TimedFrame[] } => {
  let i = 0
  let end = bytes.length
  const decoder = new TextDecoder('utf-8', { fatal: true })

  const u8 = () => (i < end ? bytes[i++] : fail('truncated'))
  const varint = () => {
    let v = 0
    let mul = 1
    for (let n = 0; n < 10; n++) {
      const c = u8()
      v += (c & 127) * mul
      mul *= 128
      if (c < 128) return v <= Number.MAX_SAFE_INTEGER ? v : fail('varint out of range')
    }
    return fail('varint too long')
  }
  const zigzag = () => {
    const v = varint()
    return v % 2 ? -(v + 1) / 2 : v / 2
  }
  const take = (n: number) => {
    if (i + n > end) fail('truncated')
    return bytes.subarray(i, (i += n))
  }
  const str = () => {
    const b = take(varint())
    try {
      return decoder.decode(b)
    } catch {
      return fail('invalid UTF-8')
    }
  }
  const node = (): RNode => {
    const n: RNode = {
      id: varint(),
      parent: varint(),
      type: u8(),
      x: zigzag(),
      y: zigzag(),
      w: varint(),
      h: varint(),
      flags: u8(),
    }
    if (n.flags & 1) {
      const c = take(4)
      n.color = (c[0] | (c[1] << 8) | (c[2] << 16) | (c[3] << 24)) >>> 0
    }
    if (n.flags & 2) n.text = str()
    return n
  }
  const list = <T>(item: () => T): T[] => {
    const n = varint()
    // every item takes at least one byte: reject absurd counts before allocating
    if (n > end - i) fail('truncated')
    const out: T[] = []
    for (let k = 0; k < n; k++) out.push(item())
    return out
  }

  if (end < 20 || bytes[0] != 76 || bytes[1] != 65 || bytes[2] != 82 || bytes[3] != 49) fail('bad magic')
  i = 4
  const session_id = Array.from(take(16), (x) => (x + 256).toString(16).slice(1)).join('')
  const seq = varint()
  const start_ts = varint()
  const frames: TimedFrame[] = []
  let t = start_ts

  while (i < end) {
    const len = varint()
    if (len < 1 || i + len > end) fail('bad frame length')
    const total = end
    const next = i + len
    end = next // reads may not run past this frame
    const kind = u8()
    const dt = varint()
    t += dt
    let f: TimedFrame | undefined
    if (kind == 0) f = { kind, dt, w: varint(), h: varint(), nodes: list(node) }
    else if (kind == 1) f = { kind, dt, removed: list(varint), upserted: list(node) }
    else if (kind == 2) f = { kind, dt, phase: u8(), x: varint(), y: varint() }
    else if (kind == 3) f = { kind, dt, node_id: varint(), dx: zigzag(), dy: zigzag() }
    else if (kind == 4) f = { kind, dt, name: str() }
    else if (kind == 5) f = { kind, dt, w: varint(), h: varint() }
    if (f) {
      if (withTime) f.t = t
      frames.push(f)
    }
    i = next
    end = total
  }
  return { session_id, seq, start_ts, frames }
}

/** Player state: the current wireframe, in draw order, plus the viewport. */
export interface Scene {
  nodes: Map<number, RNode>
  w: number
  h: number
  screen: string
}

export const newScene = (): Scene => ({ nodes: new Map(), w: 0, h: 0, screen: '' })

/**
 * Apply one frame to a scene (mutates and returns it).
 *
 * - full_snapshot replaces everything.
 * - delta: `removed` ids go with their whole subtree, then `upserted` nodes
 *   replace existing ones in place or are appended on top.
 * - scroll moves `node_id` and its whole subtree by (dx, dy); this is how the
 *   web SDK reports window scrolling (root node 1).
 * - touch frames carry no scene state; draw them from the frame itself.
 */
export const applyFrame = (scene: Scene, f: Frame): Scene => {
  const nodes = scene.nodes
  /** ids of `root` and everything below it */
  const subtree = (root: number) => {
    const ids = new Set([root])
    // parents normally precede children, so one pass is enough; re-parenting upserts may need more
    for (let size = 0; size != ids.size; ) {
      size = ids.size
      for (const n of nodes.values()) if (ids.has(n.parent)) ids.add(n.id)
    }
    return ids
  }
  if (f.kind == 0) {
    nodes.clear()
    for (const n of f.nodes) nodes.set(n.id, { ...n })
    scene.w = f.w
    scene.h = f.h
  } else if (f.kind == 1) {
    for (const id of f.removed) for (const x of subtree(id)) nodes.delete(x)
    for (const n of f.upserted) nodes.set(n.id, { ...n })
  } else if (f.kind == 3) {
    for (const id of subtree(f.node_id)) {
      const n = nodes.get(id)
      if (n) {
        n.x += f.dx
        n.y += f.dy
      }
    }
  } else if (f.kind == 4) scene.screen = f.name
  else if (f.kind == 5) {
    scene.w = f.w
    scene.h = f.h
  }
  return scene
}
