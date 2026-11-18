// The package must be importable and callable where there is no browser at all.
import assert from 'node:assert/strict'
import { test } from 'node:test'

test('import and use without window/document (SSR)', async () => {
  assert.equal(typeof window, 'undefined')
  assert.equal(typeof document, 'undefined')
  const core = await import('../dist/index.js')
  const flags = await import('../dist/flags.js')
  const replay = await import('../dist/replay.js')
  await import('../dist/replay-decode.js')

  assert.doesNotThrow(() => {
    core.init({ token: 'pk_test', host: 'https://in.example' })
    core.capture('server_render', { a: 1 })
    core.page()
    core.identify('u', { plan: 'x' })
    core.setPersonProps({ a: 1 })
    core.register({ a: 1 })
    core.unregister('a')
    core.reset()
    core.optOut()
    core.optIn()
    replay.startReplay()
    replay.stopReplay()
  })
  assert.equal(await core.flush(), undefined)
  await flags.initFlags()
  assert.equal(flags.flag('anything', 'fallback'), 'fallback')
  assert.deepEqual(flags.allFlags(), {})
  assert.equal(replay.replay.active(), false)
})
