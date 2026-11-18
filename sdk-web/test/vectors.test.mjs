// Shared test vectors: wire (LA1), murmur3, flags, replay (LAR1).
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { evalFlag, murmur3 } from '../dist/flags.js'
import { decodeChunk } from '../dist/replay-decode.js'
import { encodeChunk } from '../dist/replay.js'
import { batch, headerTail, record, unhex } from '../dist/wire.js'
import { decodeBatch } from './helpers/la1.mjs'
import { fromHex, toHex, vectors } from './helpers/vectors.mjs'

const TAG = { null: 0, int: 3, float: 4, string: 5, json: 6 }

/** Vector props → the encoder's typed list. */
const typed = (list) =>
  list.map(({ k, t, v }) => {
    if (t == 'bool') return [k, v ? 2 : 1]
    if (t == 'int') return [k, 3, BigInt(v)]
    if (t == 'float') return [k, 4, Number(v)]
    return [k, TAG[t], v]
  })

const encode = (b) => {
  const records = b.records.flatMap((r) => record(r.kind, unhex(r.uuid), r.ts, r.name, typed(r.props)))
  return batch(unhex(b.batch_id), b.sent_at, headerTail(b.sdk, b.anon_id, b.user_id, unhex(b.session_id), typed(b.context)), records)
}

/** Vector floats are decimal strings; compare them by value (NaN == NaN, -0 != 0). */
const sameProps = (got, want, where) => {
  assert.equal(got.length, want.length, where)
  got.forEach((g, i) => {
    const w = want[i]
    assert.equal(g.k, w.k, where)
    assert.equal(g.t, w.t, where)
    if (w.t == 'float') assert.ok(Object.is(g.v, Number(w.v)), `${where}: ${w.k} = ${g.v}, want ${w.v}`)
    else assert.deepEqual(g.v, w.v, `${where}: ${w.k}`)
  })
}

const sameBatch = (got, want, where) => {
  for (const f of ['batch_id', 'sent_at', 'sdk', 'anon_id', 'user_id', 'session_id']) assert.deepEqual(got[f], want[f], `${where}: ${f}`)
  sameProps(got.context, want.context, `${where}: context`)
  assert.equal(got.records.length, want.records.length, `${where}: record count`)
  got.records.forEach((r, i) => {
    const w = want.records[i]
    for (const f of ['kind', 'uuid', 'ts', 'name']) assert.deepEqual(r[f], w[f], `${where}: record ${i} ${f}`)
    sameProps(r.props, w.props, `${where}: record ${i}`)
  })
}

const wire = vectors('wire')

test('wire.json valid[]: encoder output is byte-identical', () => {
  assert.ok(wire.valid.length >= 6)
  for (const v of wire.valid) assert.equal(toHex(encode(v.batch)), v.hex, v.name)
})

test('wire.json valid[] + tolerant[]: the test helper decoder agrees', () => {
  for (const v of wire.valid) sameBatch(decodeBatch(fromHex(v.hex), { strict: true }), v.batch, v.name)
  for (const v of wire.tolerant) {
    const got = decodeBatch(fromHex(v.hex))
    if (v.name == 'duplicate_key_last_wins') {
      // a list decoder keeps both entries; the last one must be the vector's value
      const props = got.records[0].props
      assert.equal(props.at(-1).v, v.batch.records[0].props[0].v)
      got.records[0].props = [props.at(-1)]
    }
    sameBatch(got, v.batch, v.name)
  }
})

test('murmur3.json', () => {
  const m = vectors('murmur3')
  assert.equal(m.seed, 0)
  assert.ok(m.cases.length >= 17)
  for (const c of m.cases) {
    assert.equal(murmur3(c.input), c.hash, JSON.stringify(c.input))
    assert.equal(murmur3(c.input) % 10000, c.bucket)
  }
})

test('flags.json: every case', () => {
  const { ruleset, cases } = vectors('flags')
  assert.ok(cases.length >= 117)
  for (const c of cases) {
    const flag = ruleset.flags.find((f) => f.key == c.flag)
    assert.ok(flag, c.flag)
    assert.deepEqual(evalFlag(flag, c.props, c.user_id, c.anon_id), c.expect, JSON.stringify(c))
  }
})

const replay = vectors('replay')

test('replay.json valid[]: encoder output is byte-identical', () => {
  assert.ok(replay.valid.length >= 2)
  for (const v of replay.valid) assert.equal(toHex(encodeChunk(v.chunk)), v.hex, v.name)
})

test('replay.json valid[] + tolerant[]: decoder', () => {
  for (const v of [...replay.valid, ...replay.tolerant]) assert.deepEqual(decodeChunk(fromHex(v.hex)), v.chunk, v.name)
})

test('replay decoder: absolute times account for skipped frames', () => {
  const v = replay.tolerant.find((x) => x.name == 'unknown_frame_kind_skipped')
  const c = decodeChunk(fromHex(v.hex), true)
  assert.equal(c.frames[0].t, c.start_ts + 5 + 7)
})

test('replay decoder: rejects truncated and malformed input', () => {
  const hex = replay.valid[0].hex
  for (let n = 0; n < hex.length / 2; n += 7) {
    const cut = fromHex(hex).subarray(0, n)
    // every proper prefix either fails or (on a frame boundary) decodes to fewer frames
    try {
      const c = decodeChunk(cut)
      assert.ok(c.frames.length < replay.valid[0].chunk.frames.length)
    } catch (e) {
      assert.match(e.message, /^LAR1: /)
    }
  }
  assert.throws(() => decodeChunk(fromHex('4c41310000')), /bad magic/)
})
