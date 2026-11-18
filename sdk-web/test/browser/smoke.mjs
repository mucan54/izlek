// Real-browser smoke test: the minified <script> bundles in Chromium against a
// local collector on another origin (so CORS and preflights are real).
//
//   node test/browser/smoke.mjs        (after `npm run build`)
//
// Uses the Playwright already installed on the machine; it is deliberately not
// a dependency of this package.
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { createRequire } from 'node:module'

import { evalFlag } from '../../dist/flags.js'
import { applyFrame, newScene } from '../../dist/replay-decode.js'
import { startCollector, TOKEN } from '../helpers/collector.mjs'
import { vectors } from '../helpers/vectors.mjs'

const { chromium } = createRequire(import.meta.url)('playwright')

const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36'
const dist = new URL('../../dist/', import.meta.url)
const pageHtml = await readFile(new URL('./page.html', import.meta.url))

// ───────────────────────── servers ─────────────────────────

const site = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://site').pathname
  try {
    if (path.startsWith('/dist/')) {
      res.setHeader('Content-Type', 'text/javascript')
      return res.end(await readFile(new URL(path.slice(6), dist)))
    }
    res.setHeader('Content-Type', 'text/html; charset=utf-8')
    if (path == '/other') return res.end('<!doctype html><title>other</title><p>bye</p>')
    if (path == '/favicon.ico') return res.writeHead(204).end()
    res.end(pageHtml) // any other path is the single-page app
  } catch (e) {
    res.writeHead(404).end(String(e))
  }
})
await new Promise((r) => site.listen(0, '127.0.0.1', r))
const SITE = `http://127.0.0.1:${site.address().port}`
const collector = await startCollector({ ruleset: vectors('flags').ruleset })
const { state } = collector

const browser = await chromium.launch()
const errors = []

async function openPage({ path = '/', interval = 200, replay = false, context } = {}) {
  context ||= await browser.newContext({ userAgent: UA, viewport: { width: 800, height: 600 } })
  const page = await context.newPage()
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => m.type() == 'error' && errors.push(m.text()))
  const q = new URLSearchParams({ host: collector.origin, interval: String(interval), utm_source: 'smoke' })
  if (replay) q.set('replay', '1')
  const url = `${SITE}${path}?${q}`
  await page.goto(url)
  return { page, context, url }
}

async function waitFor(what, cond, ms = 10_000) {
  const t0 = Date.now()
  for (;;) {
    const v = await cond()
    if (v) return v
    if (Date.now() - t0 > ms) throw new Error(`timed out waiting for ${what}`)
    await new Promise((r) => setTimeout(r, 25))
  }
}

const obj = (props) => Object.fromEntries(props.map((p) => [p.k, p.t == 'int' ? Number(p.v) : p.t == 'json' ? JSON.parse(p.v) : p.v]))
const named = (name) => collector.accepted().filter((r) => r.name == name)
const waitEvent = (name, n = 1) => waitFor(`${n} × "${name}"`, () => named(name).length >= n && named(name))

const steps = []
async function step(name, fn) {
  const t0 = Date.now()
  try {
    await fn()
    steps.push({ name, ok: true, ms: Date.now() - t0 })
    console.log(`ok   ${name} (${Date.now() - t0} ms)`)
  } catch (e) {
    steps.push({ name, ok: false })
    console.log(`FAIL ${name}\n     ${String(e.stack || e).replace(/\n/g, '\n     ')}`)
  }
}

// ───────────────────────── events ─────────────────────────

const main = await openPage()
let anonId

await step('pageview arrives, decodes, and carries the page context', async () => {
  const [pv] = await waitEvent('pageview')
  const b = pv.batch
  anonId = b.anon_id
  assert.equal(b.sdk, 'web/0.1.0')
  assert.match(b.anon_id, /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  assert.equal(b.user_id, '')
  assert.notEqual(b.session_id, '0'.repeat(32))
  assert.ok(Math.abs(b.sent_at - Date.now()) < 5000)
  const ctx = obj(b.context)
  assert.equal(ctx.platform, 'web')
  assert.equal(ctx.url, main.url)
  assert.equal(ctx.path, '/')
  assert.equal(ctx.referrer, '')
  assert.equal(ctx.utm_source, 'smoke')
  assert.equal(ctx.ua, UA)
  assert.equal(ctx.viewport_w, 800)
  assert.equal(ctx.viewport_h, 600)
  assert.equal(typeof ctx.locale, 'string')
  assert.equal(obj(pv.props).path, '/')
  assert.equal(pv.post.auth, 'header')
  assert.ok(state.preflights >= 1, 'cross-origin POST with Authorization was preflighted')
  // identity is in a first-party cookie and in localStorage
  const cookies = await main.context.cookies()
  assert.equal(cookies.find((c) => c.name == `la_${TOKEN}`)?.value, anonId)
  assert.equal(await main.page.evaluate((k) => JSON.parse(localStorage.getItem(k)).a, `la_${TOKEN}`), anonId)
})

await step('capture: value types survive the wire', async () => {
  await main.page.evaluate(() => izlek.capture('smoke', { n: 1, neg: -5, f: 1.5, yes: true, nil: null, s: 'çay ☕', o: { a: [1, 2] } }))
  const [e] = await waitEvent('smoke')
  assert.deepEqual(
    e.props.map((p) => [p.k, p.t, p.v]),
    [
      ['n', 'int', '1'],
      ['neg', 'int', '-5'],
      ['f', 'float', 1.5],
      ['yes', 'bool', true],
      ['nil', 'null', null],
      ['s', 'string', 'çay ☕'],
      ['o', 'json', '{"a":[1,2]}'],
    ],
  )
  assert.equal(e.batch.anon_id, anonId)
  assert.match(e.uuid, /^[0-9a-f]{12}7/)
  assert.ok(Math.abs(e.ts - Date.now()) < 5000)
})

await step('identify: new batch whose header carries both ids, identify record first', async () => {
  await main.page.evaluate(() => {
    izlek.capture('before_identify')
    izlek.identify('user-42', { plan: 'pro' })
    izlek.capture('after_identify')
  })
  const [after] = await waitEvent('after_identify')
  const [before] = await waitEvent('before_identify')
  assert.equal(before.batch.user_id, '')
  assert.equal(after.batch.user_id, 'user-42')
  assert.equal(after.batch.anon_id, anonId)
  assert.notEqual(after.batch.batch_id, before.batch.batch_id)
  const first = after.batch.records[0]
  assert.equal(first.kind, 1)
  assert.deepEqual(obj(first.props), { plan: 'pro', user_id: 'user-42' })
})

await step('data-track click is captured with its data-track-* props', async () => {
  await main.page.click('#buy')
  const [e] = await waitEvent('buy_click')
  assert.deepEqual(obj(e.props), { plan: 'pro' })
  await main.page.click('#title') // unmarked: nothing
})

await step('SPA navigation emits pageleave + pageview (pushState and back button)', async () => {
  await main.page.click('#nav') // the page's router calls history.pushState
  const views = await waitEvent('pageview', 2)
  assert.equal(obj(views[1].props).path, '/pricing')
  assert.equal(obj(views[1].props).referrer, main.url)
  const [leave] = await waitEvent('pageleave')
  assert.equal(obj(leave.props).path, '/')

  await main.page.goBack() // popstate
  const back = await waitEvent('pageview', 3)
  assert.equal(obj(back[2].props).path, '/')
  assert.equal(obj(back[2].props).referrer, `${SITE}/pricing`)
  assert.equal((await waitEvent('pageleave', 2))[1].props.find((p) => p.k == 'path').v, '/pricing')
  assert.equal(named('click').length, 0)
})

await step('large batches are gzip-compressed and still decode', async () => {
  await main.page.evaluate(() => {
    for (let i = 0; i < 12; i++) izlek.capture('bulk', { i, text: 'lorem ipsum dolor sit amet '.repeat(10) })
  })
  const bulk = await waitEvent('bulk', 12)
  assert.deepEqual(bulk.map((e) => obj(e.props).i), [...Array(12).keys()])
  assert.ok(bulk.every((e) => e.post.gzip), 'sent with Content-Encoding: gzip')
})

// ───────────────────────── flags ─────────────────────────

await step('flags: ruleset is downloaded and evaluated locally; exposure is recorded once', async () => {
  const values = await main.page.evaluate(async () => {
    await flagsReady
    const f = izlekFlags.flag
    return [f('everyone'), f('nobody', 'off'), f('short_weights'), f('disabled_with_default'), f('missing', 'dflt'), f('everyone'), izlekFlags.allFlags().half]
  })
  assert.deepEqual(values.slice(0, 6), [true, 'off', 'only', 'legacy', 'dflt', true])
  // a 50 % rollout: the browser must agree with the evaluator that passes the shared vectors in Node
  const half = vectors('flags').ruleset.flags.find((f) => f.key == 'half')
  assert.equal(values[6], evalFlag(half, {}, 'user-42', anonId))
  const exposed = await waitEvent('flag_exposed', 4)
  assert.deepEqual(
    exposed.map((e) => obj(e.props)),
    [
      { flag: 'everyone', variant: 'true', ruleset_v: 31 },
      { flag: 'nobody', variant: 'false', ruleset_v: 31 },
      { flag: 'short_weights', variant: 'only', ruleset_v: 31 },
      { flag: 'disabled_with_default', variant: 'legacy', ruleset_v: 31 },
    ],
  )
  assert.deepEqual(state.flagGets[0], { status: 200, ifNoneMatch: undefined })
})

await step('flags: next page load starts from the cache and revalidates with If-None-Match → 304', async () => {
  const before = state.flagGets.length
  const again = await openPage({ context: main.context })
  // usable synchronously from the cached copy, before the network answers
  const early = await again.page.evaluate(() => izlekFlags.flag('everyone', 'not-loaded'))
  assert.equal(early, true)
  await again.page.evaluate(() => flagsReady)
  const get = await waitFor('flag revalidation', () => state.flagGets[before])
  assert.deepEqual(get, { status: 304, ifNoneMatch: state.etag })
  assert.equal(await again.page.evaluate(() => izlekFlags.flags.ruleset().v), 31)
  await again.page.close()
})

// ───────────────────────── unload and persistence ─────────────────────────

await step('events captured right before leaving the page still arrive (keepalive, ?t=)', async () => {
  const p = await openPage({ interval: 600_000, context: main.context })
  await p.page.evaluate(() => izlek.capture('last_words', { n: 1 }))
  await new Promise((r) => setTimeout(r, 100))
  assert.equal(named('last_words').length, 0, 'still queued in memory')
  const preflights = state.preflights
  await p.page.goto(`${SITE}/other`)
  const [e] = await waitEvent('last_words')
  assert.equal(e.post.auth, 'query', 'unload path authenticates with ?t=')
  assert.equal(e.post.gzip, false)
  assert.equal(state.preflights, preflights, 'the unload request is a simple CORS request: no preflight')
  const names = e.batch.records.map((r) => r.name)
  assert.ok(names.includes('pageleave'), `pageleave in the unload batch: ${names}`)
  await p.page.close()
})

await step('batches the server did not accept are persisted and resent with the same batch_id', async () => {
  state.eventStatus = 503
  const from = state.posts.length
  const p = await openPage({ interval: 100 }) // fresh browser profile: nothing left over in localStorage
  await p.page.evaluate(() => izlek.capture('offline_event', { n: 7 }))
  const storedIds = () =>
    p.page.evaluate((prefix) => Object.keys(localStorage).filter((k) => k.startsWith(prefix) && k.length == prefix.length + 32).map((k) => k.slice(-32)), `la_${TOKEN}_`)
  const rejected = await waitFor('a rejected attempt', () =>
    state.posts.slice(from).find((x) => x.status == 503 && x.batch.records.some((r) => r.name == 'offline_event')),
  )
  const ids = await storedIds()
  assert.ok(ids.includes(rejected.batch.batch_id), 'the rejected batch is in localStorage under its batch_id')
  assert.equal(named('offline_event').length, 0)

  await p.page.reload() // the new page load finds the batch in localStorage
  state.eventStatus = 200
  const [e] = await waitFor('the resend', () => named('offline_event').length && named('offline_event'), 15_000)
  assert.ok(ids.includes(e.batch.batch_id), 'resent under the batch_id it was stored with')
  assert.deepEqual(obj(e.props), { n: 7 })
  const ok = await waitFor('the retried batch', () => state.posts.find((x) => x.status == 200 && x.batch.batch_id == rejected.batch.batch_id), 15_000)
  assert.deepEqual(ok.batch.records, rejected.batch.records, 'identical records')
  assert.ok(ok.batch.sent_at > rejected.batch.sent_at, 'sent_at is restamped')
  await waitFor('storage cleanup', async () => !(await storedIds()).some((id) => ids.includes(id)))
  await p.context.close()
})

await step('headless/bot user agents are not tracked', async () => {
  const context = await browser.newContext() // default UA contains "HeadlessChrome"
  const p = await openPage({ context, path: '/bot' })
  assert.match(await p.page.evaluate(() => navigator.userAgent), /HeadlessChrome/)
  await p.page.evaluate(() => izlek.capture('bot_event'))
  await new Promise((r) => setTimeout(r, 700))
  await p.page.goto(`${SITE}/other`)
  await new Promise((r) => setTimeout(r, 300))
  assert.equal(state.posts.filter((x) => x.batch?.context.some((c) => c.k == 'path' && c.v == '/bot')).length, 0)
  assert.equal(named('bot_event').length, 0)
  assert.deepEqual(await context.cookies(), [])
  await context.close()
})

// ───────────────────────── replay ─────────────────────────

await step('replay: decodable chunks, inputs masked, images blank, scroll and touch recorded', async () => {
  const SECRET = 'hunter2-secret'
  const p = await openPage({ replay: true })
  await p.page.waitForTimeout(700)
  await p.page.type('#name', SECRET)
  await p.page.type('#pw', 'p4ssw0rd!')
  await p.page.click('#buy')
  // chunks close after 5 s; an idle page records nothing more
  await waitFor('the first chunk', () => state.chunks.length >= 1, 10_000)
  await p.page.waitForTimeout(1200)
  assert.equal(state.chunks.length, 1, 'no uploads while nothing happens')

  await p.page.click('#title') // input again: a new chunk opens with a full snapshot
  await p.page.waitForTimeout(700)
  await p.page.mouse.wheel(0, 400)
  await p.page.waitForTimeout(900)
  assert.equal(await p.page.evaluate(() => scrollY), 400)
  await p.page.evaluate(() => izlek.identify('replay-user'))
  await p.page.click('#nav') // screen change
  await waitFor('the second chunk', () => state.chunks.length >= 2, 10_000)
  const replayAnon = await p.page.evaluate((k) => JSON.parse(localStorage.getItem(k)).a, `la_${TOKEN}`)
  await p.page.goto(`${SITE}/other`)
  await p.page.waitForTimeout(300)

  const pv = await waitFor('the replay page\'s pageview', () => collector.accepted().find((r) => r.name == 'pageview' && r.batch.anon_id == replayAnon))
  const chunks = state.chunks
  for (const c of chunks) assert.equal(c.error, undefined, `chunk rejected: ${c.error}`)
  assert.deepEqual(chunks.map((c) => c.chunk.seq), chunks.map((_, i) => i), 'seq counts up from 0')

  const scene = newScene()
  const kinds = new Set()
  let lastDelta = -Infinity
  let scrolled
  let settling = false
  for (const c of chunks) {
    const { chunk, query } = c
    assert.equal(c.encoding, 'gzip')
    // query string mirrors the chunk (spec/replay.md §4)
    assert.equal(query.s, chunk.session_id)
    assert.equal(Number(query.n), chunk.seq)
    assert.equal(Number(query.ts), chunk.start_ts)
    assert.equal(Number(query.d), chunk.frames.reduce((n, f) => n + f.dt, 0))
    assert.equal(query.a, pv.batch.anon_id)
    assert.ok(Number(query.d) <= 5500, `chunk spans ${query.d} ms`)
    assert.ok(c.raw.length <= 2 * 65536)
    assert.equal(chunk.session_id, pv.batch.session_id, 'replay and events share the session id')
    // every chunk can be played on its own: a full snapshot comes first (after an optional screen name)
    const lead = chunk.frames.findIndex((f) => f.kind == 0)
    assert.ok(lead >= 0 && chunk.frames.slice(0, lead).every((f) => f.kind == 4), 'chunk starts with a full snapshot')
    // nothing typed, and no image description, anywhere in the bytes
    for (const leak of [SECRET, 'hunter2', 'p4ssw0rd', 'alt text', 'gizli metin', 'görünmez']) {
      assert.ok(!Buffer.from(c.raw).includes(leak), `"${leak}" leaked into a replay chunk`)
    }
    for (const f of chunk.frames) {
      kinds.add(f.kind)
      if (f.kind == 1) {
        assert.ok(f.t - lastDelta >= 500, `deltas ${f.t - lastDelta} ms apart`)
        lastDelta = f.t
      }
      applyFrame(scene, f)
      if (f.kind == 3) {
        assert.equal(f.node_id, 1)
        settling = true
      } else if (f.kind == 1 && settling) {
        // the delta after a scroll puts fixed elements back; look at the wireframe then
        settling = false
        scrolled = structuredClone([...scene.nodes.values()])
      }
    }
  }
  for (const k of [0, 1, 2, 3, 4]) assert.ok(kinds.has(k), `no frame of kind ${k}`)
  assert.ok(chunks.some((c) => c.query.u == 'replay-user'), 'user id is attached once identified')
  assert.equal(chunks[0].query.u, undefined)
  assert.equal(chunks[0].chunk.frames[0].kind, 4)
  assert.equal(chunks[0].chunk.frames[0].name, '/')
  assert.ok(chunks.some((c) => c.chunk.frames.some((f) => f.kind == 4 && f.name == '/pricing')), 'SPA navigation names the new screen')

  // the wireframe right after the first scroll frame
  const all = (pred) => scrolled.filter(pred)
  const byText = (t) => all((n) => n.text == t)[0]
  const MASKED = 4
  const root = scrolled[0]
  assert.deepEqual([root.id, root.parent, root.type, root.x, root.y], [1, 0, 5, 0, -400])
  assert.ok(root.h >= 3000)

  const title = byText('Merhaba dünya')
  assert.ok(title && title.type == 1 && !(title.flags & MASKED), 'plain text is recorded as is')
  assert.ok(title.y < 0, 'content moved up with the scroll')
  const fixed = byText('sabit')
  assert.ok(fixed && fixed.y >= 0 && fixed.y < 40, `fixed element stays in the viewport (y=${fixed?.y})`)

  const inputs = all((n) => n.type == 4)
  assert.equal(inputs.length, 3)
  const typed = inputs.find((n) => n.text == '•'.repeat(SECRET.length))
  assert.ok(typed && typed.flags & MASKED, 'typed text is replaced by bullets of the same length')
  const password = inputs.find((n) => n.text == '•'.repeat(9))
  assert.ok(password && password.flags & MASKED, 'passwords are masked even inside data-la-unmask')
  const open = inputs.find((n) => n.text == 'herkese açık')
  assert.ok(open && !(open.flags & MASKED), 'data-la-unmask shows a field value')
  const masked = byText('•'.repeat('çok gizli metin'.length))
  assert.ok(masked && masked.type == 1 && masked.flags & MASKED, 'data-la-mask hides text')

  const images = all((n) => n.type == 2)
  assert.equal(images.length, 1)
  assert.deepEqual([images[0].w, images[0].h, images[0].text, images[0].color], [120, 60, undefined, undefined])
  const button = all((n) => n.type == 3)[0]
  assert.ok(button && button.text == 'Satın al' && button.flags & 8, 'button with label, clickable')
  const card = all((n) => n.color == 0xffeeeeff)[0]
  assert.ok(card && card.type == 0 && card.w > 700, 'container with its background colour')
  // parents always precede children
  const seen = new Set([0])
  for (const n of scrolled) {
    assert.ok(seen.has(n.parent), `node ${n.id} before its parent ${n.parent}`)
    seen.add(n.id)
  }

  // touch frames: down then up, at the button
  const touches = chunks.flatMap((c) => c.chunk.frames.filter((f) => f.kind == 2))
  assert.deepEqual(touches.slice(0, 2).map((f) => f.phase), [0, 2])
  assert.ok(touches[0].x > 0 && touches[0].y > 0)
  await p.context.close()
})

await step('no errors were thrown into the page', async () => {
  // failed requests are logged by the browser itself; only script errors count here
  const scriptErrors = errors.filter((e) => !/Failed to load resource|net::ERR/.test(e))
  assert.deepEqual(scriptErrors, [])
  for (const post of state.posts) assert.equal(post.error, undefined, `collector rejected a body: ${post.error}`)
})

await browser.close()
await collector.close()
await new Promise((r) => (site.closeAllConnections(), site.close(r)))

// Informational: batches that reached the collector more than once. This is the
// at-least-once delivery at work (same batch_id, so the real server drops the copy).
const seenIds = new Map()
for (const post of state.posts) if (post.status == 200) seenIds.set(post.batch.batch_id, (seenIds.get(post.batch.batch_id) || 0) + 1)
const repeats = [...seenIds.values()].filter((n) => n > 1).length
console.log(`\nbatches accepted: ${seenIds.size}, of which delivered more than once (deduplicated by batch_id): ${repeats}`)

const failed = steps.filter((s) => !s.ok)
console.log(`browser smoke: ${steps.length - failed.length}/${steps.length} steps passed (${state.posts.length} event posts, ${state.chunks.length} replay chunks)`)
process.exit(failed.length ? 1 : 0)
