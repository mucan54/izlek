import assert from 'node:assert/strict'
import { test } from 'node:test'
import { demoRecording, encodeChunk } from '../../mock/lar1.mjs'
import { decodeChunk, type Chunk, type Frame, type RNode } from '../../src/replay/lar1'
import { advance, buildTimeline, progress, seek, skipIdle, type PlayerState } from '../../src/replay/player'

const node = (id: number, parent: number, extra: Partial<RNode> = {}): RNode => ({ id, parent, type: 0, x: 0, y: id * 10, w: 100, h: 10, flags: 0, ...extra })
const T0 = 1_000_000

const chunk = (seq: number, start: number, frames: Frame[]): Chunk => ({ session_id: 'ab'.repeat(16), seq, start_ts: start, frames })
const ids = (s: PlayerState) => [...s.scene.nodes.keys()]

// root(1) → list(2) → item(3) → label(4);  root(1) → footer(5)
const TREE = [node(1, 0), node(2, 1), node(3, 2), node(4, 3, { flags: 2, text: 'a' }), node(5, 1)]

const session = (): Chunk[] => [
  chunk(0, T0, [
    { kind: 4, dt: 0, name: 'Home' },
    { kind: 5, dt: 0, w: 390, h: 844 },
    { kind: 0, dt: 0, w: 390, h: 844, nodes: TREE },
    { kind: 2, dt: 500, phase: 0, x: 10, y: 20 },
    { kind: 2, dt: 50, phase: 1, x: 12, y: 60 },
    { kind: 2, dt: 50, phase: 2, x: 12, y: 60 },
    // t=1000: replace 4 in place, add 6 under 3
    { kind: 1, dt: 400, removed: [], upserted: [node(4, 3, { flags: 2, text: 'b' }), node(6, 3)] },
    // t=1500: scroll the list
    { kind: 3, dt: 500, node_id: 2, dx: 0, dy: -30 },
    // t=2000: remove the list item with everything under it
    { kind: 1, dt: 500, removed: [3], upserted: [] },
  ]),
  chunk(1, T0 + 9000, [
    { kind: 4, dt: 0, name: 'Detail' },
    { kind: 5, dt: 0, w: 390, h: 844 },
    { kind: 0, dt: 0, w: 390, h: 844, nodes: [node(1, 0), node(9, 1)] },
    { kind: 1, dt: 1000, removed: [9], upserted: [node(10, 1)] },
  ]),
]

const timeline = () => buildTimeline(session().map((c) => ({ chunk: c })))

test('timeline: absolute times, keyframes, screens, idle stretches', () => {
  const tl = timeline()
  assert.equal(tl.start, T0)
  assert.equal(tl.end, T0 + 10000)
  assert.deepEqual(tl.frames.map((f) => f.t - T0), [0, 0, 0, 500, 550, 600, 1000, 1500, 2000, 9000, 9000, 9000, 10000])
  assert.deepEqual(tl.keyframes.map((k) => [k.index, k.screen, k.w, k.h]), [[2, 'Home', 390, 844], [11, 'Detail', 390, 844]])
  assert.deepEqual(tl.screens.map((s) => [s.t - T0, s.name]), [[0, 'Home'], [9000, 'Detail']])
  assert.deepEqual(tl.idle.map((s) => [s.from - T0, s.to - T0]), [[2000, 9000]])
  assert.equal(progress(tl, T0 + 5000), 0.5)
  assert.equal(progress(tl, T0 - 1), 0)
  assert.equal(progress(tl, T0 + 99999), 1)
})

test('chunks are ordered by seq, and the chunk duration extends the end', () => {
  const [a, b] = session()
  const tl = buildTimeline([{ chunk: b, durationMs: 4000 }, { chunk: a }])
  assert.equal(tl.frames[0].t, T0)
  assert.equal(tl.end, T0 + 13000)
  assert.deepEqual(tl.idle.map((s) => [s.from - T0, s.to - T0]), [[2000, 9000], [10000, 13000]].filter(([f, t]) => t - f > 3000))
})

test('full snapshot builds the scene in draw order', () => {
  const s = seek(timeline(), T0)
  assert.deepEqual(ids(s), [1, 2, 3, 4, 5])
  assert.equal(s.scene.screen, 'Home')
  assert.deepEqual([s.scene.w, s.scene.h], [390, 844])
  assert.equal(s.next, 3)
})

test('delta upserts replace in place and append new nodes', () => {
  const s = seek(timeline(), T0 + 1000)
  assert.deepEqual(ids(s), [1, 2, 3, 4, 5, 6], 'replaced node keeps its z-order, new node goes on top')
  assert.equal(s.scene.nodes.get(4)?.text, 'b')
  assert.equal(seek(timeline(), T0 + 999).scene.nodes.get(4)?.text, 'a')
})

test('scroll moves the node and its whole subtree', () => {
  const before = seek(timeline(), T0 + 1499)
  const after = seek(timeline(), T0 + 1500)
  for (const id of [2, 3, 4, 6]) assert.equal(after.scene.nodes.get(id)!.y, before.scene.nodes.get(id)!.y - 30, `node ${id}`)
  for (const id of [1, 5]) assert.equal(after.scene.nodes.get(id)!.y, before.scene.nodes.get(id)!.y, `node ${id} is outside the scrolled subtree`)
})

test('removing a node removes its whole subtree', () => {
  const s = seek(timeline(), T0 + 2000)
  assert.deepEqual(ids(s), [1, 2, 5], '3 went with its children 4 and 6')
})

test('advance applies frames incrementally and matches a fresh seek', () => {
  const tl = timeline()
  let s = seek(tl, T0)
  for (const t of [300, 600, 1000, 1700, 2000, 8999, 9000, 9500, 10000]) {
    s = advance(tl, s, T0 + t)
    const fresh = seek(tl, T0 + t)
    assert.deepEqual([...s.scene.nodes.values()], [...fresh.scene.nodes.values()], `t=${t}`)
    assert.equal(s.scene.screen, fresh.scene.screen)
    assert.equal(s.time, T0 + t)
  }
  assert.deepEqual(ids(s), [1, 10])
  assert.equal(s.scene.screen, 'Detail')
})

test('seeking backwards rebuilds from the previous keyframe', () => {
  const tl = timeline()
  let s = seek(tl, T0 + 10000)
  assert.deepEqual(ids(s), [1, 10])
  s = advance(tl, s, T0 + 1200)
  assert.deepEqual(ids(s), [1, 2, 3, 4, 5, 6])
  assert.equal(s.scene.screen, 'Home')
  // a seek into the second chunk starts from its own snapshot, not from the first
  const mid = seek(tl, T0 + 9200)
  assert.deepEqual(ids(mid), [1, 9])
  assert.equal(mid.scene.screen, 'Detail')
  assert.equal(mid.screenAt, T0 + 9000)
})

test('touch gesture: down starts it, move/up extend it', () => {
  const tl = timeline()
  assert.deepEqual(seek(tl, T0 + 400).gesture, [])
  assert.deepEqual(seek(tl, T0 + 500).gesture.map((p) => p.phase), [0])
  const done = seek(tl, T0 + 700)
  assert.deepEqual(done.gesture.map((p) => [p.phase, p.x, p.y, p.t - T0]), [[0, 10, 20, 500], [1, 12, 60, 550], [2, 12, 60, 600]])
})

test('times are clamped to the recording', () => {
  const tl = timeline()
  assert.equal(seek(tl, T0 - 5000).time, T0)
  assert.equal(seek(tl, T0 + 99999).time, tl.end)
})

test('skipIdle jumps to just before the end of an idle stretch', () => {
  const tl = timeline()
  assert.equal(skipIdle(tl, T0 + 1000), T0 + 1000, 'not idle')
  assert.equal(skipIdle(tl, T0 + 2100), T0 + 2100, 'the first moments of a pause still play')
  assert.equal(skipIdle(tl, T0 + 3000), T0 + 8600)
  assert.equal(skipIdle(tl, T0 + 8700), T0 + 8700, 'nearly over: no jump')
  assert.equal(skipIdle(tl, T0 + 9500), T0 + 9500)
})

test('an empty recording gives an empty, harmless timeline', () => {
  const tl = buildTimeline([])
  assert.deepEqual([tl.start, tl.end, tl.frames.length], [0, 0, 0])
  assert.equal(seek(tl, 123).scene.nodes.size, 0)
  const onlyHeader = buildTimeline([{ chunk: chunk(0, T0, []) }])
  assert.deepEqual([onlyHeader.start, onlyHeader.end], [T0, T0])
})

test('the mock demo session plays end to end', () => {
  const rec = demoRecording('0193f1a2b3c47d5e8a9b0c1d2e3f4a5c', T0)
  const tl = buildTimeline(rec.chunks.map((c: Chunk & { duration_ms: number }) => ({ chunk: decodeChunk(encodeChunk(c), true), durationMs: c.duration_ms })))
  assert.deepEqual(tl.screens.map((s) => s.name), ['Giriş', 'Ana sayfa', 'Ürün', 'Sepet'])
  assert.equal(tl.idle.length, 2, 'reading the product page and filling the cart form')
  assert.ok(tl.idle[0].to - tl.idle[0].from > 9000)
  // the toast (70) and its children disappear together
  assert.ok(seek(tl, T0 + 26350).scene.nodes.has(71))
  const afterToast = seek(tl, T0 + 26450)
  for (const id of [70, 71, 72]) assert.equal(afterToast.scene.nodes.has(id), false)
  // scrolling moved the product list up under the fixed header
  assert.ok(seek(tl, T0 + 11500).scene.nodes.get(30)!.y < seek(tl, T0 + 6000).scene.nodes.get(30)!.y)
  // a chunk boundary inside a screen resumes with the same scene
  const a = seek(tl, T0 + 10590)
  const b = seek(tl, T0 + 10600)
  assert.deepEqual([...a.scene.nodes.keys()].sort(), [...b.scene.nodes.keys()].sort())
  assert.equal(seek(tl, tl.end).scene.nodes.get(91)?.text, 'Siparişin alındı')
})
