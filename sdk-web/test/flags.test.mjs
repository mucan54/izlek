// Flags module: download, caching, refresh, exposure events.
import assert from 'node:assert/strict'
import { afterEach, beforeEach, mock, test } from 'node:test'

import { createFlags, evalFlag, matchCond } from '../dist/flags.js'
import { create } from '../dist/index.js'
import { installEnv, settle, T0 } from './helpers/env.mjs'
import { vectors } from './helpers/vectors.mjs'

const TOKEN = 'pk_test'
const HOST = 'https://in.example'
const { ruleset } = vectors('flags')

beforeEach(() => {
  mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: T0 })
})
afterEach(() => {
  mock.timers.reset()
  mock.restoreAll()
})

/** Serve `rules` under an ETag, honouring If-None-Match; events go to a 200 sink. */
function serve(env, state) {
  env.respond((call) => {
    if (!call.url.includes('/flags/')) return 200
    if (state.fail) return state.fail
    if (call.headers['if-none-match'] == state.etag) return 304
    return { status: 200, headers: { etag: state.etag }, json: state.rules }
  })
}

function boot(config = {}, envOptions = {}, state = { etag: '"v31"', rules: ruleset }) {
  const env = installEnv(envOptions)
  serve(env, state)
  const la = create()
  la.init({ token: TOKEN, host: HOST, gzip: false, autoPageview: false, ...config })
  const flags = createFlags(la)
  return { env, la, flags, state }
}

const flagCalls = (env) => env.calls.filter((c) => c.url.includes('/flags/'))
const exposures = (env) => env.records().filter((r) => r.name == 'flag_exposed').map((r) => r.props)

test('downloads the ruleset, caches it with its ETag and revalidates with If-None-Match', async () => {
  const { env, flags, state } = boot()
  assert.equal(flags.flag('everyone', 'fallback'), 'fallback', 'no ruleset yet → fallback')
  await flags.start()
  assert.equal(flagCalls(env).length, 1)
  assert.equal(flagCalls(env)[0].url, `${HOST}/flags/${TOKEN}.json`)
  assert.equal(flagCalls(env)[0].headers['if-none-match'], undefined)
  assert.equal(flags.ruleset().v, 31)
  assert.equal(flags.flag('everyone'), true)
  assert.deepEqual(JSON.parse(env.storage.getItem(`la_${TOKEN}_f`)), { e: '"v31"', r: ruleset })

  await flags.reload()
  assert.equal(flagCalls(env)[1].headers['if-none-match'], '"v31"')
  assert.equal(flags.ruleset().v, 31, '304 keeps the ruleset')

  let changes = 0
  const off = flags.onChange(() => changes++)
  state.etag = '"v32"'
  state.rules = { v: 32, flags: [{ key: 'everyone', enabled: false, salt: 's1', rules: [], default: null }] }
  await flags.reload()
  assert.equal(changes, 1)
  assert.equal(flags.ruleset().v, 32)
  assert.equal(flags.flag('everyone', 'off'), 'off')
  off()
  state.etag = '"v33"'
  await flags.reload()
  assert.equal(changes, 1)
  flags.stop()
})

test('starts from the last good ruleset in localStorage and keeps it through failures', async () => {
  const first = boot()
  await first.flags.start()
  first.flags.stop()

  const state = { fail: 'network' }
  const second = boot({}, { storage: first.env.storage, cookies: first.env.cookies }, state)
  const started = second.flags.start()
  assert.equal(second.flags.flag('everyone'), true, 'usable synchronously, before the network answers')
  await started
  assert.equal(second.flags.ruleset().v, 31)
  for (const fail of [500, 404, { status: 200, json: { nope: true } }, { status: 200, json: null }]) {
    state.fail = fail
    await second.flags.reload()
    assert.equal(second.flags.ruleset().v, 31)
  }
  second.flags.stop()
})

test('flagsHost overrides the download origin', async () => {
  const { env, flags } = boot({ flagsHost: 'https://cdn.example/' })
  await flags.start()
  assert.equal(flagCalls(env)[0].url, `https://cdn.example/flags/${TOKEN}.json`)
  flags.stop()
})

test('refreshes every 5 minutes while visible, not while hidden', async () => {
  const { env, flags } = boot()
  await flags.start()
  mock.timers.tick(299_999)
  await settle()
  assert.equal(flagCalls(env).length, 1)
  mock.timers.tick(1)
  await settle()
  assert.equal(flagCalls(env).length, 2)

  env.document.visibilityState = 'hidden'
  mock.timers.tick(900_000)
  await settle()
  assert.equal(flagCalls(env).length, 2)

  // back in the foreground after more than 5 minutes: refresh right away
  env.document.visibilityState = 'visible'
  env.fireDocument('visibilitychange')
  await settle()
  assert.equal(flagCalls(env).length, 3)
  env.fireDocument('visibilitychange')
  await settle()
  assert.equal(flagCalls(env).length, 3, 'not again within the interval')
  flags.stop()
  mock.timers.tick(900_000)
  await settle()
  assert.equal(flagCalls(env).length, 3)
})

test('flag(): fallback for unknown flags and for off; values for on', async () => {
  const { flags } = boot()
  await flags.start()
  assert.equal(flags.flag('no_such_flag', 'dflt'), 'dflt')
  assert.equal(flags.flag('no_such_flag'), null)
  assert.equal(flags.flag('nobody', false), false)
  assert.equal(flags.flag('everyone', false), true)
  assert.equal(flags.flag('disabled_with_default', 'x'), 'legacy')
  assert.equal(flags.flag('short_weights'), 'only')
  const all = flags.all()
  assert.equal(all.everyone, true)
  assert.equal(all.nobody, null)
  assert.equal(Object.keys(all).length, ruleset.flags.length)
  flags.stop()
})

test('flag_exposed: once per session and flag, with flag / variant / ruleset_v', async () => {
  const { env, la, flags } = boot()
  await flags.start()
  flags.flag('everyone')
  flags.flag('everyone')
  flags.flag('nobody')
  flags.flag('short_weights')
  flags.flag('short_weights')
  flags.flag('no_such_flag')
  flags.all() // never exposes
  await la.flush()
  assert.deepEqual(exposures(env), [
    { flag: 'everyone', variant: 'true', ruleset_v: 31 },
    { flag: 'nobody', variant: 'false', ruleset_v: 31 },
    { flag: 'short_weights', variant: 'only', ruleset_v: 31 },
  ])

  // same session on the next page load: still exposed
  flags.stop()
  mock.timers.tick(60_000)
  const next = boot({}, { storage: env.storage, cookies: env.cookies })
  await next.flags.start()
  next.flags.flag('everyone')
  await next.la.flush()
  assert.deepEqual(exposures(next.env), [])

  // a new session exposes again
  mock.timers.tick(31 * 60_000)
  next.flags.flag('everyone')
  await next.la.flush()
  assert.deepEqual(exposures(next.env), [{ flag: 'everyone', variant: 'true', ruleset_v: 31 }])
  const [exposure] = next.env.records().filter((r) => r.name == 'flag_exposed')
  assert.notEqual(exposure.batch.session_id, env.calls.at(-1).batch.session_id)
  next.flags.stop()
})

test('opted-out users get flag values but no exposure events', async () => {
  const { env, la, flags } = boot()
  await flags.start()
  la.optOut()
  assert.equal(flags.flag('everyone'), true)
  la.optIn()
  assert.equal(flags.flag('everyone'), true)
  await la.flush()
  assert.deepEqual(exposures(env), [{ flag: 'everyone', variant: 'true', ruleset_v: 31 }])
  flags.stop()
})

test('targeting sees context, super props and person props, in that precedence', async () => {
  const rules = {
    v: 7,
    flags: [
      { key: 'web_only', enabled: true, salt: 'a', rules: [{ when: { prop: 'platform', op: 'eq', value: 'web' }, rollout: 10000 }], default: null },
      { key: 'by_path', enabled: true, salt: 'a', rules: [{ when: { prop: 'path', op: 'eq', value: '/home' }, rollout: 10000 }], default: null },
      { key: 'pro', enabled: true, salt: 'a', rules: [{ when: { prop: 'plan', op: 'eq', value: 'pro' }, rollout: 10000 }], default: null },
      { key: 'known_user', enabled: true, salt: 'a', rules: [{ when: { prop: 'user_id', op: 'is_set' }, rollout: 10000 }], default: null },
      { key: 'is_u1', enabled: true, salt: 'a', rules: [{ when: { prop: 'user_id', op: 'eq', value: 'u1' }, rollout: 10000 }], default: null },
    ],
  }
  const { env, la, flags } = boot({}, {}, { etag: '"x"', rules })
  await flags.start()
  assert.equal(flags.flag('web_only'), true)
  assert.equal(flags.flag('by_path'), true)
  assert.equal(flags.flag('pro'), null)
  assert.equal(flags.flag('known_user'), null)

  la.register({ plan: 'pro' }) // super prop
  assert.equal(flags.flag('pro'), true)
  la.identify('u1', { plan: 'free' }) // person prop overrides the super prop
  assert.equal(flags.flag('pro'), null)
  assert.equal(flags.flag('known_user'), true)
  assert.equal(flags.flag('is_u1'), true)
  la.setPersonProps({ plan: 'pro' })
  assert.equal(flags.flag('pro'), true)

  // person props survive a page load…
  flags.stop()
  const next = boot({}, { storage: env.storage, cookies: env.cookies }, { etag: '"x"', rules })
  await next.flags.start()
  next.la.unregister('plan')
  assert.equal(next.flags.flag('pro'), true)
  assert.equal(next.flags.flag('is_u1'), true)
  // …and are forgotten on reset
  next.la.reset()
  assert.equal(next.flags.flag('pro'), null)
  assert.equal(next.flags.flag('known_user'), null)
  next.flags.stop()
})

test('bucketing follows the live identity: user_id when identified, anon_id otherwise or when stable_on says so', async () => {
  const { la, flags } = boot()
  await flags.start()
  const anon = la._.s().a
  const half = ruleset.flags.find((f) => f.key == 'half')
  const sticky = ruleset.flags.find((f) => f.key == 'sticky')
  assert.equal(flags.all().half, evalFlag(half, {}, '', anon))
  const stickyBefore = flags.all().sticky
  la.identify('user-1') // "half" → true for user-1 in the shared vectors
  assert.equal(flags.all().half, true)
  assert.equal(flags.all().sticky, stickyBefore)
  assert.equal(flags.all().sticky, evalFlag(sticky, {}, 'user-1', anon))
  flags.stop()
})

test('start() before init is a no-op; start() twice fetches once', async () => {
  const env = installEnv()
  serve(env, { etag: '"v31"', rules: ruleset })
  const la = create()
  const flags = createFlags(la)
  await flags.start()
  assert.equal(env.calls.length, 0)
  la.init({ token: TOKEN, host: HOST, autoPageview: false })
  await Promise.all([flags.start(), flags.start()])
  assert.equal(flagCalls(env).length, 1)
  flags.stop()
})

test('operators: edge cases beyond the shared vectors', () => {
  const m = (prop, op, value, props) => matchCond({ prop, op, value }, props)
  // str(): integer-valued floats print as integers; booleans as words
  assert.ok(m('a', 'eq', '3', { a: 3.0 }))
  assert.ok(m('a', 'eq', 'true', { a: true }))
  assert.ok(m('a', 'eq', '1e+21', { a: '1e+21' }))
  assert.ok(m('a', 'eq', '1000000000000000000000', { a: 1e21 }))
  assert.ok(m('a', 'contains', '.5', { a: 2.5 }))
  // numbers compare numerically, numeric strings too, booleans never
  assert.ok(m('a', 'gt', 9, { a: '10' }))
  assert.ok(!m('a', 'gt', 9, { a: '10 ' }))
  assert.ok(!m('a', 'gte', 0, { a: true }))
  assert.ok(!m('a', 'lt', '0x10', { a: 1 }))
  // absent or null props
  for (const op of ['eq', 'in', 'gt', 'gte', 'lt', 'lte', 'contains', 'regex', 'is_set', 'semver_gte', 'semver_lt']) {
    assert.ok(!m('a', op, 'x', {}), op)
    assert.ok(!m('a', op, 'x', { a: null }), op)
  }
  assert.ok(m('a', 'ne', 'x', {}))
  assert.ok(m('a', 'not_in', ['x'], { a: null }))
  // in / not_in need an array
  assert.ok(!m('a', 'in', 'x', { a: 'x' }))
  assert.ok(m('a', 'not_in', 'x', { a: 'x' }))
  // invalid regex and unknown operators are false, never throw
  assert.ok(!m('a', 'regex', '(', { a: '(' }))
  assert.ok(!m('a', 'matches_moon_phase', 1, { a: 1 }))
  // semver: v prefix, pre-release and build suffixes, missing parts
  assert.ok(m('a', 'semver_gte', '3.2', { a: 'v3.2.0-beta+7' }))
  assert.ok(m('a', 'semver_lt', '3.10.0', { a: '3.9.9' }))
  assert.ok(!m('a', 'semver_gte', '1.0', { a: '1.0.0.0' }))
  assert.ok(!m('a', 'semver_lt', '2', { a: 'one' }))
  assert.ok(m('a', 'semver_gte', 3, { a: 3 }))
})
