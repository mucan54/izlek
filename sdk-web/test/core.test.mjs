// Core SDK behaviour against a fake browser, transport and clock.
import assert from 'node:assert/strict'
import { afterEach, beforeEach, mock, test } from 'node:test'

import { create, SDK } from '../dist/index.js'
import { installEnv, makeStorage, settle, T0 } from './helpers/env.mjs'

const TOKEN = 'pk_test'
const HOST = 'https://in.example'
const UUID7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
const ZERO = '0'.repeat(32)

beforeEach(() => {
  mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: T0 })
})
afterEach(() => {
  mock.timers.reset()
  mock.restoreAll()
})

/** New page + new client. `auto: false` keeps the automatic pageview out of the way. */
function boot(config = {}, envOptions = {}) {
  const env = installEnv(envOptions)
  const la = create()
  la.init({ token: TOKEN, host: HOST, gzip: false, autoPageview: false, ...config })
  return { env, la }
}

const tick = async (ms) => {
  mock.timers.tick(ms)
  await settle()
}

const batchKeys = (env) => [...env.storage.map.keys()].filter((k) => k.startsWith(`la_${TOKEN}_`) && k.length == `la_${TOKEN}_`.length + 32)

// ───────────────────────── basics ─────────────────────────

test('init records a pageview with the documented context', async () => {
  const { env, la } = boot({ autoPageview: true })
  await la.flush()
  assert.equal(env.calls.length, 1)
  const call = env.calls[0]
  assert.equal(call.url, `${HOST}/v1/e`)
  assert.equal(call.headers.authorization, `Bearer ${TOKEN}`)
  assert.equal(call.headers['content-type'], 'application/octet-stream')
  assert.equal(call.keepalive, false)

  const b = call.batch
  assert.equal(b.sdk, SDK)
  assert.equal(b.sdk, 'web/0.1.0')
  assert.match(b.anon_id, UUID7)
  assert.equal(b.user_id, '')
  assert.notEqual(b.session_id, ZERO)
  assert.equal(b.session_id[12], '7')
  assert.equal(b.batch_id[12], '7')
  assert.equal(b.sent_at, T0)
  const ctx = Object.fromEntries(b.context.map((p) => [p.k, p.v]))
  assert.deepEqual(ctx, {
    platform: 'web',
    url: 'https://shop.example/home?utm_source=news&utm_campaign=kis&q=1',
    path: '/home',
    referrer: 'https://search.example/?q=shop',
    locale: 'tr-TR',
    viewport_w: '1280',
    viewport_h: '720',
    ua: navigator.userAgent,
    utm_source: 'news',
    utm_campaign: 'kis',
  })
  assert.equal(b.records.length, 1)
  const [r] = env.records()
  assert.equal(r.kind, 0)
  assert.equal(r.name, 'pageview')
  assert.equal(r.ts, T0)
  assert.equal(r.uuid[12], '7')
  assert.equal(parseInt(r.uuid.slice(0, 12), 16), T0)
  assert.equal(r.props.path, '/home')

  // identity is stored in a first-party cookie and in localStorage
  assert.equal(env.cookies.get(`la_${TOKEN}`), b.anon_id)
  assert.equal(JSON.parse(env.storage.getItem(`la_${TOKEN}`)).a, b.anon_id)
  assert.match(env.document.cookieWrites.at(-1), /;path=\/;max-age=31536000;samesite=lax;secure$/)
})

test('flushes at flushAt records and after flushIntervalMs', async () => {
  const { env, la } = boot({ flushAt: 3, flushIntervalMs: 4000 })
  la.capture('a')
  la.capture('b')
  await settle()
  assert.equal(env.calls.length, 0)
  la.capture('c')
  await settle()
  assert.equal(env.calls.length, 1)
  assert.deepEqual(env.calls[0].batch.records.map((r) => r.name), ['a', 'b', 'c'])

  la.capture('d')
  await tick(3999)
  assert.equal(env.calls.length, 1)
  await tick(1)
  assert.equal(env.calls.length, 2)
  assert.deepEqual(env.calls[1].batch.records.map((r) => r.name), ['d'])
  assert.notEqual(env.calls[1].batch.batch_id, env.calls[0].batch.batch_id)
})

test('value types, super props and client-side limits', async () => {
  const { env, la } = boot()
  const cyclic = {}
  cyclic.self = cyclic
  la.register({ plan: 'pro', theme: 'dark' })
  la.register({ theme: 'light' })
  la.unregister('plan')
  la.capture('types', {
    n: null,
    yes: true,
    no: false,
    int: 42,
    neg: -7,
    big: 2n ** 62n,
    huge: 2n ** 70n,
    float: 1.5,
    nan: NaN,
    s: 'çay ☕',
    arr: [1, { a: null }],
    obj: { k: 'v' },
    date: new Date(T0),
    undef: undefined,
    fn() {},
    cyclic,
    ['k'.repeat(129)]: 'key too long',
    long: 'x'.repeat(8193),
    longUtf8: 'ç'.repeat(4097),
    fits: 'x'.repeat(8192),
  })
  la.capture('many', Object.fromEntries(Array.from({ length: 250 }, (_, i) => ['p' + i, i])))
  la.capture('') // no name
  la.capture('n'.repeat(129)) // name too long
  la.capture('too_big', Object.fromEntries(Array.from({ length: 10 }, (_, i) => ['p' + i, 'x'.repeat(8000)])))
  la.capture('after')
  await la.flush()

  const records = env.records()
  assert.deepEqual(records.map((r) => r.name), ['types', 'many', 'after'])
  const typesRaw = env.calls[0].batch.records[0].props
  const tagOf = Object.fromEntries(typesRaw.map((p) => [p.k, p.t]))
  assert.deepEqual(tagOf, {
    theme: 'string',
    n: 'null',
    yes: 'bool',
    no: 'bool',
    int: 'int',
    neg: 'int',
    big: 'int',
    huge: 'string',
    float: 'float',
    nan: 'float',
    s: 'string',
    arr: 'json',
    obj: 'json',
    date: 'string',
    fits: 'string',
  })
  const p = records[0].props
  assert.equal(p.theme, 'light')
  assert.equal(p.int, 42)
  assert.equal(p.neg, -7)
  assert.equal(p.big, 2n ** 62n)
  assert.equal(p.huge, String(2n ** 70n))
  assert.equal(p.float, 1.5)
  assert.ok(Number.isNaN(p.nan))
  assert.equal(p.s, 'çay ☕')
  assert.deepEqual(p.arr, [1, { a: null }])
  assert.deepEqual(p.obj, { k: 'v' })
  assert.equal(p.date, new Date(T0).toISOString())
  assert.equal(Object.keys(records[1].props).length, 200)

  // the three rejected records are reported with the next batch
  la.capture('next')
  await la.flush()
  const ctx = Object.fromEntries(env.calls[1].batch.context.map((x) => [x.k, x.v]))
  assert.equal(ctx.dropped_count, '2') // name too long + record too big ('' is ignored outright)
})

test('gzip: bodies over 1 KB are compressed and still decode', async () => {
  const { env, la } = boot({ gzip: true })
  la.capture('small')
  await la.flush()
  assert.equal(env.calls[0].headers['content-encoding'], undefined)
  for (let i = 0; i < 10; i++) la.capture('e' + i, { text: 'lorem ipsum '.repeat(20) })
  la.flush()
  await settle(() => env.calls.length == 2)
  assert.equal(env.calls[1].headers['content-encoding'], 'gzip')
  assert.equal(env.calls[1].batch.records.length, 10)
})

// ───────────────────────── status codes (design §3.5) ─────────────────────────

test('200: batch is deleted from the queue and from storage', async () => {
  const { env, la } = boot()
  la.capture('a')
  let stored
  env.respond(() => {
    stored = batchKeys(env).length
    return 200
  })
  await la.flush()
  assert.equal(stored, 1, 'persisted while in flight')
  assert.equal(batchKeys(env).length, 0)
  await tick(10 * 60_000)
  assert.equal(env.calls.length, 1)
})

test('400: batch is dropped, not retried, and counted', async () => {
  const { env, la } = boot()
  la.capture('a')
  la.capture('b')
  env.respond(400)
  await la.flush()
  assert.equal(batchKeys(env).length, 0)
  await tick(10 * 60_000)
  assert.equal(env.calls.length, 1)

  env.respond(200)
  la.capture('c')
  await la.flush()
  assert.equal(env.calls.length, 2)
  const ctx = Object.fromEntries(env.calls[1].batch.context.map((x) => [x.k, x.v]))
  assert.equal(ctx.dropped_count, '2')
  assert.deepEqual(env.calls[1].batch.records.map((r) => r.name), ['c'])
})

for (const status of [401, 403]) {
  test(`${status}: batch is dropped and sending pauses for one hour, across page loads`, async () => {
    const { env, la } = boot()
    la.capture('a')
    env.respond(status)
    await la.flush()
    assert.equal(env.calls.length, 1)
    assert.equal(batchKeys(env).length, 0)

    env.respond(200)
    la.capture('b')
    await la.flush()
    await tick(59 * 60_000)
    assert.equal(env.calls.length, 1, 'no request during the pause')
    assert.equal(batchKeys(env).length, 1, 'new events are kept')

    // a new page load inside the pause must not send either
    const next = boot({}, { storage: env.storage, cookies: env.cookies })
    await settle()
    assert.equal(next.env.calls.length, 0)
    await tick(60_000 - 1)
    assert.equal(next.env.calls.length + env.calls.length, 1)
    await tick(1)
    // both "tabs" hold the batch now; it goes out under one batch_id and the server dedups
    const sent = [...env.calls.slice(1), ...next.env.calls]
    assert.ok(sent.length >= 1)
    assert.equal(new Set(sent.map((c) => c.batch.batch_id)).size, 1)
    assert.deepEqual(sent[0].batch.records.map((r) => r.name), ['b'])
    assert.equal(sent[0].at, T0 + 3_600_000)
  })
}

test('413: batch is split in two and both halves are sent', async () => {
  const { env, la } = boot()
  for (const n of ['a', 'b', 'c', 'd', 'e']) la.capture(n, { n })
  env.respond((_call, i) => (i == 0 ? 413 : 200))
  await la.flush()
  assert.equal(env.calls.length, 3)
  const [whole, first, second] = env.calls.map((c) => c.batch)
  assert.deepEqual(first.records.map((r) => r.name), ['a', 'b'])
  assert.deepEqual(second.records.map((r) => r.name), ['c', 'd', 'e'])
  assert.deepEqual([...first.records, ...second.records], whole.records, 'records are untouched')
  assert.equal(new Set([whole.batch_id, first.batch_id, second.batch_id]).size, 3)
  for (const half of [first, second]) {
    assert.equal(half.anon_id, whole.anon_id)
    assert.equal(half.session_id, whole.session_id)
    assert.deepEqual(half.context, whole.context)
  }
  assert.equal(batchKeys(env).length, 0)
})

test('413: keeps splitting; a single record that is still too large is dropped', async () => {
  const { env, la } = boot()
  for (const n of ['a', 'b', 'c', 'd']) la.capture(n)
  env.respond(413)
  await la.flush()
  // 4 → 2+2 → 1+1+1+1 → dropped: 1 + 2 + 4 requests
  assert.equal(env.calls.length, 7)
  assert.deepEqual(env.calls.map((c) => c.batch.records.length), [4, 2, 1, 1, 2, 1, 1])
  assert.equal(batchKeys(env).length, 0)
  await tick(10 * 60_000)
  assert.equal(env.calls.length, 7)
})

test('429: waits for Retry-After, then resends the same batch', async () => {
  const { env, la } = boot()
  la.capture('a')
  env.respond((_call, i) => (i == 0 ? { status: 429, headers: { 'retry-after': '7' } } : 200))
  await la.flush()
  assert.equal(env.calls.length, 1)
  await tick(6999)
  assert.equal(env.calls.length, 1)
  await tick(1)
  assert.equal(env.calls.length, 2)
  assert.equal(env.calls[1].batch.batch_id, env.calls[0].batch.batch_id)
  assert.equal(env.calls[1].batch.sent_at, T0 + 7000, 'sent_at is restamped on resend')
  assert.deepEqual(env.calls[1].batch.records, env.calls[0].batch.records)
  assert.equal(batchKeys(env).length, 0)
})

test('503 with Retry-After (server back-pressure) is honoured the same way', async () => {
  const { env, la } = boot()
  la.capture('a')
  env.respond((_call, i) => (i == 0 ? { status: 503, headers: { 'retry-after': '30' } } : 200))
  await la.flush()
  await tick(29_999)
  assert.equal(env.calls.length, 1)
  await tick(1)
  assert.equal(env.calls.length, 2)
  assert.equal(env.calls[1].batch.batch_id, env.calls[0].batch.batch_id)
})

test('429 without a readable Retry-After falls back to backoff', async () => {
  mock.method(Math, 'random', () => 1)
  const { env, la } = boot()
  la.capture('a')
  env.respond((_call, i) => (i == 0 ? 429 : 200))
  await la.flush()
  await tick(999)
  assert.equal(env.calls.length, 1)
  await tick(1)
  assert.equal(env.calls.length, 2)
})

for (const failure of [500, 503, 'network']) {
  test(`${failure}: exponential backoff from 1 s to 5 min, same batch_id every time`, async () => {
    mock.method(Math, 'random', () => 1) // upper edge of the jitter window
    const { env, la } = boot()
    la.capture('a')
    env.respond(failure)
    await la.flush()
    const delays = [1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300].map((s) => s * 1000)
    let n = 1
    for (const d of delays) {
      assert.equal(env.calls.length, n)
      await tick(d - 1)
      assert.equal(env.calls.length, n, `no retry before ${d} ms`)
      await tick(1)
      assert.equal(env.calls.length, ++n, `retry after ${d} ms`)
    }
    assert.equal(new Set(env.calls.map((c) => c.batch.batch_id)).size, 1)
    assert.equal(batchKeys(env).length, 1, 'still persisted')

    // success ends the streak: the next failure starts again at 1 s
    env.respond(200)
    await tick(300_000)
    assert.equal(batchKeys(env).length, 0)
    n = env.calls.length
    env.respond(failure)
    la.capture('b')
    await la.flush()
    assert.equal(env.calls.length, n + 1)
    await tick(1000)
    assert.equal(env.calls.length, n + 2)
  })
}

test('backoff jitter stays within [50 %, 100 %] of the step', async () => {
  mock.method(Math, 'random', () => 0)
  const { env, la } = boot()
  la.capture('a')
  env.respond(500)
  await la.flush()
  await tick(499)
  assert.equal(env.calls.length, 1)
  await tick(1)
  assert.equal(env.calls.length, 2) // 0.5 × 1 s
  await tick(999)
  assert.equal(env.calls.length, 2)
  await tick(1)
  assert.equal(env.calls.length, 3) // 0.5 × 2 s
})

test('one request at a time; later batches wait behind a failing one', async () => {
  mock.method(Math, 'random', () => 1)
  const { env, la } = boot({ flushAt: 1 })
  env.respond('network')
  la.capture('a')
  la.capture('b')
  la.capture('c')
  await settle()
  assert.equal(env.calls.length, 1)
  env.respond(200)
  await tick(1000)
  assert.deepEqual(env.calls.map((c) => c.batch.records[0].name), ['a', 'a', 'b', 'c'])
})

// ───────────────────────── persistence ─────────────────────────

test('unsent batch survives a page load and is resent with the same batch_id', async () => {
  const first = boot()
  first.la.capture('checkout', { total: 129.9 })
  mock.timers.tick(1) // batch ids are UUIDv7: stored batches replay in creation order at ms resolution
  first.la.identify('user-1')
  first.la.capture('paid')
  first.env.respond('network')
  await first.la.flush()
  const attempted = first.env.calls.map((c) => c.batch)
  assert.equal(attempted.length, 1)
  assert.equal(batchKeys(first.env).length, 2)

  // next page load, three minutes later; the old page (and its timers) are gone
  mock.timers.reset()
  mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: T0 + 180_000 })
  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  await settle()
  const resent = second.env.calls.map((c) => c.batch)
  assert.equal(resent.length, 2)
  assert.equal(resent[0].batch_id, attempted[0].batch_id)
  assert.equal(resent[0].sent_at, T0 + 180_000)
  for (const f of ['sdk', 'anon_id', 'user_id', 'session_id', 'context', 'records']) assert.deepEqual(resent[0][f], attempted[0][f], f)
  assert.equal(resent[0].user_id, '')
  assert.deepEqual(resent[0].records.map((r) => r.name), ['checkout'])
  assert.equal(resent[1].user_id, 'user-1', 'each stored batch keeps its own identity')
  assert.deepEqual(resent[1].records.map((r) => [r.kind, r.name]), [[1, ''], [0, 'paid']])
  assert.equal(batchKeys(second.env).length, 0)

  // and the identity itself carried over
  second.la.capture('later')
  await second.la.flush()
  assert.equal(second.env.calls[2].batch.anon_id, attempted[0].anon_id)
  assert.equal(second.env.calls[2].batch.user_id, 'user-1')
})

test('stored batches older than 7 days are discarded on load', async () => {
  const first = boot()
  first.la.capture('old')
  first.env.respond('network')
  await first.la.flush()
  assert.equal(batchKeys(first.env).length, 1)
  mock.timers.reset()
  mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: T0 + 7 * 86_400_000 + 1 })
  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  await settle()
  assert.equal(second.env.calls.length, 0)
  assert.equal(batchKeys(second.env).length, 0)
})

test('localStorage cap: oldest batches are evicted, total stays under maxQueueBytes', async () => {
  const cap = 2000
  const { env, la } = boot({ flushAt: 1, maxQueueBytes: cap })
  env.respond('network')
  for (let i = 0; i < 30; i++) {
    la.capture('e' + i, { pad: 'x'.repeat(100) })
    mock.timers.tick(1) // distinct UUIDv7 timestamps keep the eviction order well-defined
  }
  await settle()
  const keys = batchKeys(env)
  const total = keys.reduce((n, k) => n + env.storage.getItem(k).length, 0)
  assert.ok(total <= cap, `stored ${total} > cap ${cap}`)
  assert.ok(keys.length >= 2 && keys.length < 30)

  // what is left are the newest events, in order
  env.respond(200)
  const from = env.calls.length
  await tick(300_000)
  const delivered = env.calls.slice(from).map((c) => c.batch)
  const names = delivered.flatMap((b) => b.records.map((r) => r.name))
  assert.ok(names.length >= 2 && names.length < 30)
  assert.deepEqual(names, Array.from({ length: names.length }, (_, i) => 'e' + (30 - names.length + i)))
  assert.equal(batchKeys(env).length, 0)

  // evicted records are reported as dropped_count by the batches opened after the eviction
  // (a report riding on a batch that is itself evicted is lost, so the sum is a lower bound)
  la.capture('report')
  await la.flush()
  const reported = [...delivered, env.calls.at(-1).batch]
    .flatMap((b) => b.context.filter((p) => p.k == 'dropped_count'))
    .reduce((n, p) => n + Number(p.v), 0)
  assert.ok(reported >= 1 && reported <= 30 - names.length, `dropped_count total ${reported}`)
})

test('localStorage cap also bounds batches left by other tabs', async () => {
  const cap = 1500
  const storage = makeStorage()
  const a = boot({ flushAt: 1, maxQueueBytes: cap }, { storage })
  a.env.respond('network')
  for (let i = 0; i < 6; i++) {
    a.la.capture('a' + i, { pad: 'x'.repeat(100) })
    mock.timers.tick(1)
  }
  const b = boot({ flushAt: 1, maxQueueBytes: cap, autoPageview: false }, { storage, cookies: a.env.cookies })
  b.env.respond('network')
  for (let i = 0; i < 6; i++) {
    b.la.capture('b' + i, { pad: 'x'.repeat(100) })
    mock.timers.tick(1)
  }
  await settle()
  const total = batchKeys(b.env).reduce((n, k) => n + storage.getItem(k).length, 0)
  assert.ok(total <= cap, `stored ${total} > cap ${cap}`)
})

test('works without localStorage and never throws into the page', async () => {
  const broken = {
    getItem() {
      throw new Error('SecurityError')
    },
    setItem() {
      throw new Error('QuotaExceededError')
    },
    removeItem() {
      throw new Error('SecurityError')
    },
    key() {
      throw new Error('SecurityError')
    },
    get length() {
      throw new Error('SecurityError')
    },
  }
  const { env, la } = boot({ autoPageview: true }, { storage: broken })
  Object.defineProperty(env.document, 'cookie', {
    get() {
      throw new Error('SecurityError')
    },
    set() {
      throw new Error('SecurityError')
    },
  })
  la.capture('a', { ok: 1 })
  la.identify('u')
  la.register({ a: 1 })
  la.reset()
  la.capture('b')
  env.hide()
  await la.flush()
  await settle()
  const names = env.records().map((r) => r.name || r.kind)
  for (const n of ['pageview', 'a', 1, 3, 'b']) assert.ok(names.includes(n), `missing ${n}`)

  // hostile inputs
  const cyclic = {}
  cyclic.self = cyclic
  assert.doesNotThrow(() => {
    la.capture(null)
    la.capture({})
    la.capture('x', cyclic)
    la.capture('x', 'not an object')
    la.capture('x', { get boom() { throw new Error('getter') } })
    la.identify()
    la.identify(null, null)
    la.identify(12345)
    la.setPersonProps()
    la.register()
    la.unregister()
    la.page('nope')
    la.init()
    la.init({ token: TOKEN, host: HOST })
  })
  await la.flush()
})

test('fetch that throws synchronously is treated as a network error', async () => {
  mock.method(Math, 'random', () => 1)
  const { env, la } = boot()
  const real = globalThis.fetch
  let thrown = 0
  globalThis.fetch = () => {
    thrown++
    throw new TypeError('blocked')
  }
  la.capture('a')
  await la.flush()
  assert.equal(thrown, 1)
  globalThis.fetch = real
  await tick(1000)
  assert.deepEqual(env.records().map((r) => r.name), ['a'])
})

// ───────────────────────── page unload ─────────────────────────

test('page hide sends queued events with keepalive and ?t=, without preflight headers', async () => {
  const { env, la } = boot({ autoPageview: true, gzip: true })
  for (let i = 0; i < 5; i++) la.capture('e' + i, { text: 'lorem ipsum '.repeat(30) })
  env.fireWindow('pagehide', { persisted: false })
  // the request must have been issued synchronously, before the handler returned
  assert.equal(env.calls.length, 1)
  const call = env.calls[0]
  assert.equal(call.via, 'fetch')
  assert.equal(call.keepalive, true)
  assert.equal(call.url, `${HOST}/v1/e?t=${TOKEN}`)
  assert.deepEqual(call.headers, {})
  assert.deepEqual(call.batch.records.map((r) => r.name), ['pageview', 'e0', 'e1', 'e2', 'e3', 'e4', 'pageleave'])
  assert.equal(batchKeys(env).length, 1, 'persisted until the response arrives')
  await settle()
  assert.equal(batchKeys(env).length, 0)
})

test('page hide: a batch whose keepalive request never completes is resent on the next load', async () => {
  const first = boot()
  first.la.capture('last_click')
  first.env.respond(() => new Promise(() => {})) // the page died first
  first.env.hide()
  assert.equal(first.env.calls.length, 1)
  const id = first.env.calls[0].batch.batch_id
  await settle()

  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  await settle()
  assert.equal(second.env.calls.length, 1)
  assert.equal(second.env.calls[0].batch.batch_id, id)
  assert.deepEqual(second.env.calls[0].batch.records.map((r) => r.name), ['last_click'])
})

test('page hide without fetch keepalive falls back to sendBeacon with ?t=', async () => {
  const first = boot({}, { keepalive: false })
  first.la.capture('a')
  first.env.hide()
  assert.equal(first.env.beacons.length, 1)
  assert.equal(first.env.calls.length, 1)
  assert.equal(first.env.beacons[0].url, `${HOST}/v1/e?t=${TOKEN}`)
  const id = first.env.beacons[0].batch.batch_id
  await settle()
  // a beacon gives no status: the stored copy stays and is not resent by this page
  assert.equal(batchKeys(first.env).length, 1)
  await tick(600_000)
  assert.equal(first.env.calls.length, 1)

  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  await settle()
  assert.equal(second.env.calls.length, 1)
  assert.equal(second.env.calls[0].batch.batch_id, id, 'same batch_id: the server dedups')
  assert.equal(batchKeys(second.env).length, 0)
})

test('page hide respects the 64 KiB keepalive budget; the rest stays persisted', async () => {
  const { env, la } = boot()
  const big = { a: 'x'.repeat(8000), b: 'x'.repeat(8000), c: 'x'.repeat(8000) }
  for (let i = 0; i < 6; i++) la.capture('big' + i, big) // 6 × 24 KB, sealed at ~50 KB each
  await settle()
  env.respond(() => new Promise(() => {}))
  env.calls.length = 0
  la.capture('tail', big)
  la.capture('tail2', big)
  la.capture('tail3', big)
  la.capture('tail4', big)
  env.hide()
  const inFlight = env.calls.filter((c) => c.keepalive).reduce((n, c) => n + c.body.length, 0)
  assert.ok(inFlight > 0 && inFlight <= 64 * 1024, `keepalive bytes in flight: ${inFlight}`)
  assert.ok(batchKeys(env).length >= 1)
})

// ───────────────────────── identity and session ─────────────────────────

test('identify is a batch boundary; the identify record opens the identified batch', async () => {
  const { env, la } = boot()
  la.capture('anon_event')
  la.identify('user-42', { plan: 'pro' })
  la.capture('user_event')
  la.identify('user-42', { plan: 'max' }) // same user: person props only
  la.identify('user-42') // nothing to say
  la.setPersonProps({ age: 34 })
  await la.flush()

  assert.equal(env.calls.length, 2)
  const [anon, user] = env.calls.map((c) => c.batch)
  assert.equal(anon.user_id, '')
  assert.deepEqual(anon.records.map((r) => r.name), ['anon_event'])
  assert.equal(user.user_id, 'user-42')
  assert.equal(user.anon_id, anon.anon_id)
  assert.equal(user.session_id, anon.session_id)
  assert.notEqual(user.batch_id, anon.batch_id)
  assert.deepEqual(
    user.records.map((r) => [r.kind, r.name, Object.fromEntries(r.props.map((p) => [p.k, p.v]))]),
    [
      [1, '', { plan: 'pro', user_id: 'user-42' }],
      [0, 'user_event', {}],
      [2, '', { plan: 'max' }],
      [2, '', { age: '34' }],
    ],
  )
  assert.equal(JSON.parse(env.storage.getItem(`la_${TOKEN}`)).u, 'user-42')
})

test('identify with another user id switches the header identity at a boundary', async () => {
  const { env, la } = boot()
  la.identify('alice')
  la.capture('a')
  la.identify('bob')
  la.capture('b')
  await la.flush()
  const batches = env.calls.map((c) => c.batch)
  assert.deepEqual(batches.map((b) => b.user_id), ['alice', 'bob'])
  assert.deepEqual(batches.map((b) => b.records.map((r) => r.kind)), [[1, 0], [1, 0]])
})

test('identify ignores empty and oversized user ids', async () => {
  const { env, la } = boot()
  la.identify('')
  la.identify('x'.repeat(201))
  la.capture('a')
  await la.flush()
  assert.equal(env.calls[0].batch.user_id, '')
  assert.deepEqual(env.calls[0].batch.records.map((r) => r.kind), [0])
})

test('reset: the reset record goes out under the old identity, then a new anon_id starts', async () => {
  const { env, la } = boot()
  la.identify('user-42')
  la.capture('before')
  la.reset()
  la.capture('after')
  await la.flush()

  assert.equal(env.calls.length, 2)
  const [old, fresh] = env.calls.map((c) => c.batch)
  assert.equal(old.user_id, 'user-42')
  assert.deepEqual(old.records.map((r) => [r.kind, r.name]), [[1, ''], [0, 'before'], [3, '']])
  assert.equal(fresh.user_id, '')
  assert.match(fresh.anon_id, UUID7)
  assert.notEqual(fresh.anon_id, old.anon_id)
  assert.notEqual(fresh.session_id, old.session_id)
  assert.deepEqual(fresh.records.map((r) => r.name), ['after'])
  assert.equal(env.cookies.get(`la_${TOKEN}`), fresh.anon_id)
  const state = JSON.parse(env.storage.getItem(`la_${TOKEN}`))
  assert.equal(state.a, fresh.anon_id)
  assert.equal(state.u, '')
})

test('session: same id within 30 min of activity, new id (and new batch) after', async () => {
  const { env, la } = boot({ flushIntervalMs: 3_600_000 })
  la.capture('a')
  mock.timers.tick(29 * 60_000)
  la.capture('b')
  mock.timers.tick(30 * 60_000) // 30 min since the last event: still the same session
  la.capture('c')
  mock.timers.tick(30 * 60_000 + 1)
  la.capture('d')
  la.capture('e')
  await la.flush()
  const batches = env.calls.map((c) => c.batch)
  assert.deepEqual(batches.map((b) => b.records.map((r) => r.name)), [['a', 'b', 'c'], ['d', 'e']])
  assert.notEqual(batches[0].session_id, batches[1].session_id)
  assert.equal(batches[0].anon_id, batches[1].anon_id)
  for (const b of batches) assert.equal(b.session_id[12], '7')
})

test('session continues across page loads and expires across them too', async () => {
  const first = boot()
  first.la.capture('a')
  await first.la.flush()
  const sid = first.env.calls[0].batch.session_id

  mock.timers.tick(10 * 60_000)
  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  second.la.capture('b')
  await second.la.flush()
  assert.equal(second.env.calls[0].batch.session_id, sid)

  mock.timers.tick(31 * 60_000)
  const third = boot({}, { storage: first.env.storage, cookies: first.env.cookies })
  third.la.capture('c')
  await third.la.flush()
  assert.notEqual(third.env.calls[0].batch.session_id, sid)
  assert.equal(third.env.calls[0].batch.anon_id, first.env.calls[0].batch.anon_id)
})

test('anon_id: the cookie wins over localStorage and cookieDomain is applied', async () => {
  const cookies = new Map([[`la_${TOKEN}`, 'anon-from-other-subdomain']])
  const storage = makeStorage({ [`la_${TOKEN}`]: JSON.stringify({ a: 'stale-local-anon', u: 'someone', p: { k: 1 } }) })
  const { env, la } = boot({ cookieDomain: '.example' }, { storage, cookies })
  la.capture('a')
  await la.flush()
  const b = env.calls[0].batch
  assert.equal(b.anon_id, 'anon-from-other-subdomain')
  assert.equal(b.user_id, '', 'the stored user belonged to the other anon_id')
  assert.equal(env.records()[0].props.k, 1, 'super props are kept')
  assert.match(env.document.cookieWrites.at(-1), /;domain=\.example;secure$/)
})

test('another tab changing identity is picked up through the storage event', async () => {
  const { env, la } = boot()
  la.capture('a')
  const state = JSON.parse(env.storage.getItem(`la_${TOKEN}`))
  env.fireWindow('storage', { key: `la_${TOKEN}`, newValue: JSON.stringify({ ...state, u: 'user-9' }) })
  env.fireWindow('storage', { key: 'unrelated', newValue: '{"u":"nope"}' })
  la.capture('b')
  await la.flush()
  assert.deepEqual(env.calls.map((c) => [c.batch.user_id, c.batch.records.map((r) => r.name)]), [['', ['a']], ['user-9', ['b']]])
})

// ───────────────────────── privacy ─────────────────────────

test('optOut clears the queue, ignores capture and persists; optIn resumes', async () => {
  const { env, la } = boot()
  env.respond('network')
  la.capture('queued')
  await la.flush()
  la.capture('open')
  assert.equal(batchKeys(env).length, 1)

  la.optOut()
  assert.equal(batchKeys(env).length, 0)
  la.capture('ignored')
  la.identify('user-1', { plan: 'x' })
  la.setPersonProps({ a: 1 })
  la.page()
  env.respond(200)
  env.hide()
  await la.flush()
  await tick(600_000)
  assert.equal(env.calls.length, 1, 'only the pre-opt-out attempt')
  assert.equal(JSON.parse(env.storage.getItem(`la_${TOKEN}`)).o, 1)

  // still opted out on the next page load
  const next = boot({ autoPageview: true }, { storage: env.storage, cookies: env.cookies })
  next.la.capture('ignored')
  await next.la.flush()
  assert.equal(next.env.calls.length, 0)

  next.la.optIn()
  next.la.capture('back')
  await next.la.flush()
  assert.deepEqual(next.env.records().map((r) => r.name), ['back'])
})

test('known bot user agents send nothing and store nothing', async () => {
  for (const ua of [
    'Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)',
    'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/140.0.0.0 Safari/537.36',
    'Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm) Chrome/116',
    'facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)',
    'Mozilla/5.0 (Linux; Android 11; moto g power) Chrome/122 Mobile Safari/537.36 (compatible; Google-InspectionTool/1.0;) Chrome-Lighthouse',
  ]) {
    const { env, la } = boot({ autoPageview: true }, { ua })
    la.capture('a')
    la.identify('u')
    env.hide()
    await la.flush()
    assert.equal(env.calls.length, 0, ua)
    assert.equal(env.storage.map.size, 0, ua)
    assert.equal(env.cookies.size, 0, ua)
  }
  // a phone brand that merely contains "bot" is not a bot
  const { env, la } = boot({}, { ua: 'Mozilla/5.0 (Linux; Android 10; CUBOT_X30) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36' })
  la.capture('a')
  await la.flush()
  assert.equal(env.calls.length, 1)
})

// ───────────────────────── automatic events ─────────────────────────

test('SPA navigation: pageleave + pageview on pushState/replaceState/popstate, not on no-op changes', async () => {
  const { env, la } = boot({ autoPageview: true })
  const origin = 'https://shop.example'
  history.pushState({}, '', '/products?page=2')
  history.replaceState({ scroll: 10 }, '') // state only
  history.replaceState({}, '', '/products?page=2#reviews') // hash only
  history.replaceState({}, '', '/products/42')
  env.loc.href = origin + '/home' // the browser's back button
  env.fireWindow('popstate')
  la.capture('custom')
  await la.flush()

  const seen = env.records().map((r) => [r.name, r.props.path, r.props.referrer])
  const first = 'https://shop.example/home?utm_source=news&utm_campaign=kis&q=1'
  assert.deepEqual(seen, [
    ['pageview', '/home', 'https://search.example/?q=shop'],
    ['pageleave', '/home', 'https://search.example/?q=shop'],
    ['pageview', '/products', first],
    ['pageleave', '/products', first],
    ['pageview', '/products/42', origin + '/products?page=2'],
    ['pageleave', '/products/42', origin + '/products?page=2'],
    ['pageview', '/home', origin + '/products/42'],
    // the batch header still describes the first page, so later events carry their own page
    ['custom', '/home', origin + '/products/42'],
  ])
  assert.equal(env.records().at(-1).props.url, origin + '/home')
})

test('history methods keep working and keep their return value and arguments', () => {
  const env = installEnv()
  const seen = []
  history.pushState = function (...a) {
    seen.push([this === history, ...a])
    return 'ret'
  }
  const la = create()
  la.init({ token: TOKEN, host: HOST })
  assert.equal(history.pushState({ s: 1 }, 't', '/x'), 'ret')
  assert.deepEqual(seen, [[true, { s: 1 }, 't', '/x']])
  void env
})

test('manual page() and autoPageview: false', async () => {
  const { env, la } = boot()
  history.pushState({}, '', '/a')
  env.fireWindow('pagehide', {})
  await settle()
  assert.equal(env.calls.length, 0, 'no automatic pageview or pageleave')
  la.page({ section: 'docs' })
  await la.flush()
  const [r] = env.records()
  assert.equal(r.name, 'pageview')
  assert.equal(r.props.path, '/a')
  assert.equal(r.props.section, 'docs')
})

test('bfcache restore counts as a new pageview', async () => {
  const { env, la } = boot({ autoPageview: true })
  env.fireWindow('pagehide', { persisted: true })
  env.fireWindow('pageshow', { persisted: false })
  env.fireWindow('pageshow', { persisted: true })
  await la.flush()
  assert.deepEqual(env.records().map((r) => r.name), ['pageview', 'pageleave', 'pageview'])
})

test('data-track: only explicitly marked elements are tracked', async () => {
  const { env, la } = boot()
  const marked = {
    getAttribute: (n) => (n == 'data-track' ? 'signup_click' : null),
    attributes: [
      { name: 'class', value: 'btn' },
      { name: 'data-track', value: 'signup_click' },
      { name: 'data-track-plan', value: 'pro' },
      { name: 'data-track-position', value: 'hero' },
    ],
  }
  const unnamed = { getAttribute: () => '', attributes: [{ name: 'data-track', value: '' }] }
  env.fireDocument('click', { target: { closest: (sel) => (sel == '[data-track]' ? marked : null) } })
  env.fireDocument('click', { target: { closest: () => null } }) // unmarked element
  env.fireDocument('click', { target: {} }) // not an element
  env.fireDocument('click', { target: { closest: () => unnamed } })
  await la.flush()
  assert.deepEqual(
    env.records().map((r) => [r.name, r.props]),
    [
      ['signup_click', { plan: 'pro', position: 'hero' }],
      ['click', {}],
    ],
  )

  const off = boot({ autoTrack: false })
  off.env.fireDocument('click', { target: { closest: () => marked } })
  await off.la.flush()
  assert.equal(off.env.calls.length, 0)
})

test('calls before init are ignored; a second init is ignored', async () => {
  const env = installEnv()
  const la = create()
  la.capture('too_early')
  la.identify('u')
  await la.flush()
  la.init({ token: TOKEN, host: HOST + '/', gzip: false, autoPageview: false })
  la.init({ token: 'pk_other', host: 'https://other.example' })
  la.capture('a')
  await la.flush()
  assert.equal(env.calls.length, 1)
  assert.equal(env.calls[0].url, `${HOST}/v1/e`, 'trailing slash on host is tolerated')
  assert.deepEqual(env.records().map((r) => r.name), ['a'])
})
