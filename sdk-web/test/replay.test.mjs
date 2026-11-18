// Replay: pure logic (diff, masking, sampling, colours) and encoder/decoder agreement.
// DOM capture itself is exercised in a real browser by test/browser/smoke.mjs.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { murmur3 } from '../dist/flags.js'
import { applyFrame, decodeChunk, newScene } from '../dist/replay-decode.js'
import { diffTrees, encodeChunk, maskText, parseColor, sampled } from '../dist/replay.js'

const node = (id, parent, extra = {}) => ({ id, parent, type: 0, x: id, y: id, w: 10, h: 10, flags: 0, ...extra })
const tree = (nodes) => new Map(nodes.map((n) => [n.id, n]))

test('maskText: one bullet per code point, never the original characters', () => {
  assert.equal(maskText('gizli'), '•••••')
  assert.equal(maskText('şifre 🚀'), '•••••••')
  assert.equal(maskText(''), '')
  assert.equal(maskText('x'.repeat(5000)).length, 1000)
})

test('sampled: murmur3(session hex) mod 10000 < rate × 10000', () => {
  const sid = '0193f1a2b3c47d5e8a9b0c1d2e3f4a5c'
  const bucket = murmur3(sid) % 10000
  assert.equal(sampled(sid, 1), true)
  assert.equal(sampled(sid, 0), false)
  assert.equal(sampled(sid, (bucket + 1) / 10000), true)
  assert.equal(sampled(sid, bucket / 10000), false)
  // roughly the requested share over many sessions
  let n = 0
  for (let i = 0; i < 4000; i++) if (sampled(i.toString(16).padStart(32, '0'), 0.25)) n++
  assert.ok(n > 850 && n < 1150, `sampled ${n} of 4000 at 25 %`)
})

test('parseColor: computed rgb()/rgba() → 0xAARRGGBB, transparent → none', () => {
  assert.equal(parseColor('rgb(10, 132, 255)'), 0xff0a84ff)
  assert.equal(parseColor('rgba(255, 255, 255, 0.5)'), 0x80ffffff)
  assert.equal(parseColor('rgb(0 0 0 / 0.25)'), 0x40000000)
  assert.equal(parseColor('rgba(0, 0, 0, 0)'), undefined)
  assert.equal(parseColor('transparent'), undefined)
  assert.equal(parseColor('color(srgb 1 0 0)'), undefined)
})

test('diffTrees: nothing changed → no frame', () => {
  const nodes = [node(1, 0), node(2, 1), node(3, 2)]
  assert.equal(diffTrees(tree(nodes), nodes.map((n) => ({ ...n }))), undefined)
})

test('diffTrees: changed and new nodes are upserted, removed subtrees listed by their top node only', () => {
  const prev = tree([node(1, 0), node(2, 1), node(3, 2), node(4, 3), node(5, 1), node(6, 1, { flags: 2, text: 'a' })])
  const next = [node(1, 0), node(5, 1, { x: 99 }), node(6, 1, { flags: 2, text: 'b' }), node(7, 1)]
  assert.deepEqual(diffTrees(prev, next), { removed: [2], upserted: [next[1], next[2], next[3]] })
})

test('diffTrees: a node that survives below a removed ancestor is sent again', () => {
  // 2 disappears (e.g. lost its background), its child 3 now hangs off 1, grandchild 4 is untouched
  const prev = tree([node(1, 0), node(2, 1), node(3, 2), node(4, 3)])
  const next = [node(1, 0), node(3, 1), node(4, 3)]
  const d = diffTrees(prev, next)
  assert.deepEqual(d.removed, [2])
  assert.deepEqual(d.upserted.map((n) => n.id), [3, 4])
})

test('diffTrees + applyFrame: a player that applies the deltas reproduces every tree (randomised)', () => {
  let seed = 42
  const rnd = (n) => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0
    return seed % n
  }
  let nextId = 1
  /** Random forest under root 1, parents before children, ids stable when kept. */
  const mutate = (prev) => {
    const kept = prev.filter((n) => n.id == 1 || rnd(5) > 0)
    const alive = new Set(kept.map((n) => n.id))
    const out = []
    for (const n of kept) {
      // re-parent orphans to the root or keep; sometimes move or retext
      let parent = n.id == 1 ? 0 : alive.has(n.parent) && out.some((o) => o.id == n.parent) ? n.parent : 1
      const m = { ...n, parent }
      if (rnd(4) == 0) m.x += rnd(7) - 3
      if (rnd(6) == 0) {
        m.flags |= 2
        m.text = 't' + rnd(100)
      }
      out.push(m)
    }
    for (let i = rnd(4); i > 0; i--) {
      const parent = out[rnd(out.length)].id
      out.push(node(++nextId, parent, rnd(2) ? { flags: 1, color: 0xff000000 + rnd(0xffffff) } : {}))
    }
    return out
  }

  let current = [node(1, 0)]
  const scene = newScene()
  applyFrame(scene, { kind: 0, dt: 0, w: 100, h: 100, nodes: current })
  let sent = tree(current)
  let deltas = 0
  for (let step = 0; step < 300; step++) {
    const next = mutate(current)
    const d = diffTrees(sent, next)
    if (d) {
      deltas++
      // through the real encoder and decoder, as the player would get it
      const chunk = { session_id: '00'.repeat(16), seq: step, start_ts: 1, frames: [{ kind: 1, dt: 0, ...d }] }
      const [frame] = decodeChunk(encodeChunk(chunk)).frames
      applyFrame(scene, frame)
    }
    const got = new Map([...scene.nodes].sort((a, b) => a[0] - b[0]))
    const want = new Map(next.map((n) => [n.id, n]).sort((a, b) => a[0] - b[0]))
    assert.deepEqual(got, want, `step ${step}`)
    current = next
    sent = tree(next)
  }
  assert.ok(deltas > 250)
})

test('applyFrame: snapshot, scroll (subtree shift), viewport, screen', () => {
  const scene = newScene()
  applyFrame(scene, { kind: 4, dt: 0, name: '/home' })
  applyFrame(scene, { kind: 0, dt: 0, w: 390, h: 844, nodes: [node(1, 0, { x: 0, y: 0 }), node(2, 1, { x: 5, y: 50 }), node(3, 0, { x: 7, y: 7 })] })
  applyFrame(scene, { kind: 3, dt: 0, node_id: 1, dx: 0, dy: -30 })
  applyFrame(scene, { kind: 2, dt: 0, phase: 0, x: 1, y: 1 })
  applyFrame(scene, { kind: 5, dt: 0, w: 800, h: 600 })
  assert.equal(scene.screen, '/home')
  assert.deepEqual([scene.w, scene.h], [800, 600])
  assert.deepEqual([...scene.nodes.values()].map((n) => [n.id, n.x, n.y]), [[1, 0, -30], [2, 5, 20], [3, 7, 7]])
})

test('encoder → decoder round trip keeps large and negative values', () => {
  const chunk = {
    session_id: 'ff'.repeat(16),
    seq: 70000,
    start_ts: 1795046400000,
    frames: [
      { kind: 0, dt: 0, w: 3840, h: 2160, nodes: [{ id: 123456, parent: 99999, type: 5, x: -100000, y: 65536, w: 0, h: 1e6, flags: 31, color: 0x80fffefd, text: 'çok uzun '.repeat(100) }] },
      { kind: 3, dt: 86_400_000, node_id: 1, dx: -1, dy: 1 },
      { kind: 1, dt: 1, removed: [], upserted: [] },
    ],
  }
  assert.deepEqual(decodeChunk(encodeChunk(chunk)), chunk)
})
