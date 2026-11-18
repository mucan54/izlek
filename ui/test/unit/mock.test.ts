// The mock is what the UI is developed against, so its flag evaluation and
// its query shapes are held to the shared vectors and to spec/query.md.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { buildDataset, createEngine } from '../../mock/data.mjs'
import { evalFlag, match, murmur3 } from '../../mock/expr.mjs'
import { totp, verifyTotp } from '../../mock/totp.mjs'
import { vectors } from './helpers'

test('murmur3 vectors', () => {
  const v = vectors<{ cases: { input: string; hash: number; bucket: number }[] }>('murmur3.json')
  for (const c of v.cases) {
    assert.equal(murmur3(c.input), c.hash, JSON.stringify(c.input.slice(0, 30)))
    assert.equal(murmur3(c.input) % 10000, c.bucket)
  }
})

test('flag evaluation vectors', () => {
  const v = vectors<{ ruleset: { flags: { key: string }[] }; cases: { flag: string; props: unknown; user_id: string; anon_id: string; expect: unknown }[] }>('flags.json')
  assert.ok(v.cases.length > 50)
  for (const [i, c] of v.cases.entries()) {
    const flag = v.ruleset.flags.find((f) => f.key == c.flag)
    assert.ok(flag, c.flag)
    const got = evalFlag(flag, c.props, c.user_id, c.anon_id)
    // `false` is the same result as null (spec/flags.md §2)
    assert.deepEqual(got === false ? null : got, c.expect === false ? null : c.expect, `case ${i} (${c.flag})`)
  }
})

test('loose matching follows spec/query.md §2', () => {
  const get = (k: string) => ({ n: 3, s: '3', b: true, z: null }) [k as 'n']
  assert.equal(match({ prop: 'n', op: 'eq', value: '3' }, get), true)
  assert.equal(match({ prop: 's', op: 'gt', value: 2 }, get), true)
  assert.equal(match({ prop: 'b', op: 'gt', value: 0 }, get), false, 'booleans are not numbers')
  assert.equal(match({ prop: 'z', op: 'ne', value: 1 }, get), true, 'null: ne is true')
  assert.equal(match({ prop: 'z', op: 'is_set' }, get), false)
  assert.equal(match({ prop: 'missing', op: 'not_in', value: [1] }, get), true)
  assert.equal(match({ prop: 'missing', op: 'eq', value: 1 }, get), false)
  assert.equal(match({ and: [] }, get), true)
  assert.equal(match({ or: [] }, get), false)
})

test('TOTP matches RFC 6238 (SHA-1) test values', () => {
  // secret "12345678901234567890" in base32
  const secret = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ'
  assert.equal(totp(secret, 59_000), '287082')
  assert.equal(totp(secret, 1111111109_000), '081804')
  assert.equal(verifyTotp(secret, '287082', 59_000), true)
  assert.equal(verifyTotp(secret, '287082', 59_000 + 30_000), true, 'one step of skew')
  assert.equal(verifyTotp(secret, '287082', 59_000 + 120_000), false)
})

const NOW = Date.parse('2026-11-18T12:00:00Z')
const engine = createEngine(buildDataset(NOW), () => NOW)
const range = { from: '2026-11-12', to: '2026-11-18' }

test('trends: one value per bucket, breakdown key only when requested', () => {
  const r = engine.run('trends', { ...range, interval: 'day', series: [{ event: 'purchase', agg: 'count' }, { event: null, agg: 'unique', label: 'Kişi' }] }, 1)
  assert.equal(r.buckets.length, 7)
  assert.equal(r.buckets[0], '2026-11-12T00:00:00Z')
  assert.equal(r.series.length, 2)
  for (const s of r.series) {
    assert.equal(s.values.length, 7)
    assert.equal('breakdown' in s, false)
  }
  assert.equal(r.series[1].label, 'Kişi')
  assert.equal(typeof r.meta.elapsed_ms, 'number')
  assert.equal(r.meta.cached, false)
  assert.equal(engine.run('trends', { ...range, interval: 'day', series: [{ event: 'purchase', agg: 'count' }, { event: null, agg: 'unique', label: 'Kişi' }] }, 1).meta.cached, true)

  const b = engine.run('trends', { ...range, interval: 'week', series: [{ event: null, agg: 'count' }], breakdown: 'platform' }, 1)
  assert.deepEqual(b.buckets, ['2026-11-09T00:00:00Z', '2026-11-16T00:00:00Z'], 'weeks start on Monday')
  assert.deepEqual(b.series.map((s: { breakdown: string }) => s.breakdown).sort(), ['android', 'ios', 'web'])
  const avg = engine.run('trends', { from: '2026-11-18T03:00:00Z', to: '2026-11-18T05:00:00Z', interval: 'hour', series: [{ event: 'purchase', agg: 'avg', prop: 'props.amount' }] }, 1)
  assert.deepEqual(avg.series[0].values, [null, null], 'empty avg buckets are null, not 0')
})

test('funnel: counts never increase, breakdown sums to the total', () => {
  const q = { from: '2026-10-20', to: '2026-11-18', steps: [{ event: 'signup' }, { event: 'product_viewed' }, { event: 'purchase' }], window_seconds: 604800 }
  const r = engine.run('funnel', { ...q, breakdown: 'platform' }, 1)
  const counts = r.steps.map((s: { count: number }) => s.count)
  assert.ok(counts[0] > 0)
  assert.ok(counts[0] >= counts[1] && counts[1] >= counts[2])
  assert.equal(r.steps[0].median_seconds, null)
  assert.equal(typeof r.steps[1].median_seconds, 'number')
  assert.equal(r.breakdowns.reduce((a: number, b: { steps: { count: number }[] }) => a + b.steps[0].count, 0), counts[0])
  // a tiny window converts fewer people than a long one
  const tight = engine.run('funnel', { ...q, window_seconds: 60 }, 1)
  assert.ok(tight.steps[2].count <= counts[2])
  assert.throws(() => engine.run('funnel', { ...q, steps: [{ event: 'signup' }] }, 1), /2 ile 20/)
})

test('retention: returned[0] = size when return = start, rows shorten towards `to`', () => {
  const r = engine.run('retention', { from: '2026-10-05', to: '2026-11-18', period: 'week', periods: 4, start: { event: 'signup' }, return: { event: 'signup' } }, 1)
  assert.equal(r.cohorts.length, 7)
  for (const c of r.cohorts) assert.equal(c.returned[0], c.size)
  assert.deepEqual(r.cohorts.map((c: { returned: number[] }) => c.returned.length), [4, 4, 4, 4, 3, 2, 1])
})

test('events: newest first, paged by cursor, replay keys carry the project prefix', () => {
  const first = engine.run('events', { ...range, limit: 5 }, 7)
  assert.equal(first.events.length, 5)
  assert.ok(first.events[0].ts >= first.events[4].ts)
  assert.ok(first.next_cursor)
  const second = engine.run('events', { ...range, limit: 5, cursor: first.next_cursor }, 7)
  assert.ok(second.events[0].ts <= first.events[4].ts)
  assert.equal(new Set([...first.events, ...second.events].map((e: { uuid: string }) => e.uuid)).size, 10)
  assert.match(first.events[0].uuid, /^[0-9a-f]{32}$/)

  const chunks = engine.run('events', { ...range, filter: { prop: 'event', op: 'eq', value: 'session_replay_chunk' }, limit: 100, order: 'asc' }, 7).events
  assert.ok(chunks.length >= 4)
  for (const c of chunks) assert.ok(c.props.key.startsWith('replay/p=7/'), c.props.key)
  assert.ok(engine.replayChunk(chunks[0].props.key, 7).body.length > 0)
  assert.throws(() => engine.replayChunk(chunks[0].props.key, 8), /bu projeye ait değil/)
  assert.throws(() => engine.run('events', { ...range, filter: { prop: 'nope', op: 'eq', value: 1 } }, 7), /bilinmeyen sütun/)
})

test('sql: only SELECT, persons lookup returns props as JSON text', () => {
  const r = engine.run('sql', { ...range, sql: "SELECT props FROM persons WHERE person_key = 'u:1001'" }, 1)
  assert.deepEqual(r.columns.map((c: { name: string }) => c.name), ['person_key', 'props'])
  assert.equal(JSON.parse(r.rows[0][1]).name, 'Ayşe')
  assert.throws(() => engine.run('sql', { ...range, sql: 'DROP TABLE events' }, 1), /SELECT/)
  assert.throws(() => engine.run('sql', { ...range, sql: 'SELECT 1; SELECT 2' }, 1), /SELECT/)
})
