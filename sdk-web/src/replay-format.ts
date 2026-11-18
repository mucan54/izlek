// LAR1 session replay chunk: logical types and encoder (spec/replay.md).
// The logical shapes mirror spec/vectors/replay.json.

import { put, str, unhex, type Bytes } from './wire'

/** Node types. */
export const CONTAINER = 0
export const TEXT = 1
export const IMAGE = 2
export const BUTTON = 3
export const INPUT = 4
export const SCROLL = 5
export const OTHER = 6

/** Node flag bits. */
export const HAS_COLOR = 1
export const HAS_TEXT = 2
export const MASKED = 4
export const CLICKABLE = 8
export const FOCUSED = 16

export interface RNode {
  /** ≥ 1 */
  id: number
  /** 0 = root */
  parent: number
  type: number
  /** CSS px, absolute */
  x: number
  y: number
  w: number
  h: number
  flags: number
  /** 0xAARRGGBB; present iff `flags & HAS_COLOR` */
  color?: number
  /** present iff `flags & HAS_TEXT` */
  text?: string
}

export type Frame =
  | { kind: 0; dt: number; w: number; h: number; nodes: RNode[] }
  | { kind: 1; dt: number; removed: number[]; upserted: RNode[] }
  | { kind: 2; dt: number; phase: number; x: number; y: number }
  | { kind: 3; dt: number; node_id: number; dx: number; dy: number }
  | { kind: 4; dt: number; name: string }
  | { kind: 5; dt: number; w: number; h: number }

export interface Chunk {
  /** hex32 */
  session_id: string
  seq: number
  /** unix ms of the first frame */
  start_ts: number
  frames: Frame[]
}

// Replay values are small non-negative integers (and timestamps < 2^53), so a
// plain-number varint is exact here and much cheaper than the BigInt one used
// for event props.
const varint = (o: Bytes, v: number): void => {
  for (; v > 127; v = Math.floor(v / 128)) o.push((v % 128) | 128)
  o.push(v)
}
const zigzag = (o: Bytes, v: number): void => varint(o, v < 0 ? -2 * v - 1 : 2 * v)

const node = (o: Bytes, n: RNode): void => {
  varint(o, n.id)
  varint(o, n.parent)
  o.push(n.type)
  zigzag(o, n.x)
  zigzag(o, n.y)
  varint(o, n.w)
  varint(o, n.h)
  o.push(n.flags)
  if (n.flags & HAS_COLOR) for (let i = 0, c = n.color!; i < 4; i++, c = Math.floor(c / 256)) o.push(c % 256)
  if (n.flags & HAS_TEXT) str(o, n.text!)
}

/** Append one framed Frame (`varint(len) kind dt payload`) to `o`. */
export const encodeFrame = (o: Bytes, f: Frame): void => {
  const b: Bytes = [f.kind]
  varint(b, f.dt)
  if (f.kind == 0) {
    varint(b, f.w)
    varint(b, f.h)
    varint(b, f.nodes.length)
    for (const n of f.nodes) node(b, n)
  } else if (f.kind == 1) {
    varint(b, f.removed.length)
    for (const id of f.removed) varint(b, id)
    varint(b, f.upserted.length)
    for (const n of f.upserted) node(b, n)
  } else if (f.kind == 2) {
    b.push(f.phase)
    varint(b, f.x)
    varint(b, f.y)
  } else if (f.kind == 3) {
    varint(b, f.node_id)
    zigzag(b, f.dx)
    zigzag(b, f.dy)
  } else if (f.kind == 4) str(b, f.name)
  else {
    varint(b, f.w)
    varint(b, f.h)
  }
  varint(o, b.length)
  put(o, b)
}

/** `"LAR1" session_id seq start_ts`, to be followed by frames. */
export const chunkHeader = (sessionId: string, seq: number, startTs: number): Bytes => {
  const o: Bytes = [76, 65, 82, 49]
  put(o, unhex(sessionId))
  varint(o, seq)
  varint(o, startTs)
  return o
}

export const encodeChunk = (c: Chunk): Uint8Array<ArrayBuffer> => {
  const o = chunkHeader(c.session_id, c.seq, c.start_ts)
  for (const f of c.frames) encodeFrame(o, f)
  return new Uint8Array(o)
}
