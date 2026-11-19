// A self-contained demo: starts the whole stack on one machine with a file
// store, fills it with three weeks of synthetic shop traffic, records one
// real browser session with the web SDK, and creates flags, insights and a
// dashboard through the API.
//
//   node e2e/demo.mjs --serve                 # keep running; browse the UI
//   node e2e/demo.mjs --shots docs/images     # take the README screenshots and exit
//
// Needs the same binaries as e2e/run.mjs:
//   (cd engine && cargo build -p la-shipper -p la-query)
//   (cd sdk-web && npm ci && npm run build) ; (cd ui && npm ci && npm run build)
// All data is synthetic, including the "countries" (a made-up IP→country file).
import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { createRequire } from 'node:module'
import { createServer as netServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const argv = process.argv.slice(2)
const opt = (name) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : undefined)
const SHOTS = opt('--shots') && resolve(opt('--shots'))
const SERVE = argv.includes('--serve')
const LANG = opt('--lang') || 'en'
const USERS = Number(opt("--users") || 900)
const DAYS = 30
if (!SHOTS && !SERVE) {
  console.error('usage: node e2e/demo.mjs --serve | --shots <dir> [--lang en|tr] [--users N]')
  process.exit(2)
}

const firstExisting = (...c) => c.find((p) => p && existsSync(p))
const SHIPPER = firstExisting(process.env.LA_E2E_SHIPPER, join(ROOT, 'engine/target/release/la-shipper'), join(ROOT, 'engine/target/debug/la-shipper'))
const QUERY = firstExisting(process.env.LA_E2E_QUERY, join(ROOT, 'engine/target/release/la-query'), join(ROOT, 'engine/target/debug/la-query'), join(ROOT, 'engine/target-query/debug/la-query'))
assert.ok(SHIPPER && QUERY, 'build la-shipper and la-query first: (cd engine && cargo build -p la-shipper -p la-query)')
for (const f of ['sdk-web/dist/izlek.min.js', 'ui/dist/index.html']) assert.ok(existsSync(join(ROOT, f)), `${f} is missing: run "npm ci && npm run build" in ${f.split('/')[0]}/`)

const WORK = mkdtempSync(join(tmpdir(), 'izlek-demo-'))
const BIN = join(WORK, 'bin')
const LOGS = join(WORK, 'logs')
for (const d of [BIN, LOGS]) mkdirSync(d, { recursive: true })
const UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36'

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const freePort = () =>
  new Promise((res, rej) => {
    const s = netServer()
    s.once('error', rej)
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address()
      s.close(() => res(port))
    })
  })
async function waitFor(what, cond, ms = 90_000, every = 300) {
  const t0 = Date.now()
  let last
  for (;;) {
    try {
      const v = await cond()
      if (v) return v
      last = v
    } catch (e) {
      last = e?.message
    }
    if (Date.now() - t0 > ms) throw new Error(`timed out waiting for ${what} (last: ${JSON.stringify(last)})`)
    await sleep(every)
  }
}
const procs = []
function start(name, cmd, args, env) {
  const log = createWriteStream(join(LOGS, name + '.log'))
  const child = spawn(cmd, args, { env: { ...process.env, ...env }, stdio: ['ignore', 'pipe', 'pipe'] })
  child.stdout.pipe(log)
  child.stderr.pipe(log)
  const p = { name, child }
  child.once('exit', (code, sig) => (p.code = code ?? sig))
  procs.push(p)
  return p
}

// Deterministic randomness: the same demo every time.
let seed = 20261119
const rnd = () => {
  seed |= 0
  seed = (seed + 0x6d2b79f5) | 0
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296
}
const pick = (pairs) => {
  let r = rnd() * pairs.reduce((a, [, w]) => a + w, 0)
  for (const [v, w] of pairs) if ((r -= w) < 0) return v
  return pairs[pairs.length - 1][0]
}

let ok = false
let browser
try {
  const run = (cmd, args, env = {}) => spawnSync(cmd, args, { cwd: ROOT, env: { ...process.env, ...env }, encoding: 'utf8' })
  assert.equal(run('go', ['build', '-o', BIN + '/', './ingest/cmd/la-ingest', './control/cmd/la-control'], { CGO_ENABLED: '0' }).status, 0, 'go build failed')
  assert.equal(run('go', ['run', './e2e/tools/geodb', join(WORK, 'geo.mmdb')]).status, 0, 'geodb failed')

  const [pIngest, pQuery, pControl] = [await freePort(), await freePort(), SERVE ? Number(opt('--port') || 8090) : await freePort()]
  const INGEST = `http://127.0.0.1:${pIngest}`
  const QUERY_URL = `http://127.0.0.1:${pQuery}`
  const CONTROL = `http://127.0.0.1:${pControl}`
  const OWNER = { email: 'demo@example.com', password: 'demo-password-1' }
  const storeEnv = { LA_STORE_URL: 'file://' + join(WORK, 'store') }
  const controlEnv = { ...storeEnv, LA_CONTROL_DB: join(WORK, 'control.db'), LA_CONTROL_LISTEN: `127.0.0.1:${pControl}`, LA_QUERY_URL: QUERY_URL, LA_QUERY_INTERNAL_SECRET: 'demo-internal-secret', LA_UI_DIR: join(ROOT, 'ui/dist'), LA_COOKIE_SECURE: 'false', LA_CONTROL_PUBLISH_DEBOUNCE_MS: '100', LA_PUBLIC_URL: CONTROL, LA_PUBLIC_INGEST_URL: INGEST }

  const b = run(join(BIN, 'la-control'), ['bootstrap', '--email', OWNER.email, '--password', OWNER.password, '--org', 'Acme Inc.', '--project', 'Demo shop', '--json'], controlEnv)
  assert.equal(b.status, 0, b.stderr)
  const boot = JSON.parse(b.stdout.trim().split('\n').pop())
  const P = `/api/projects/${boot.project_id}`

  start('query', QUERY, [], { ...storeEnv, LA_QUERY_LISTEN: `127.0.0.1:${pQuery}`, LA_QUERY_INTERNAL_SECRET: 'demo-internal-secret', LA_QUERY_CACHE_DIR: join(WORK, 'qcache'), LA_QUERY_MANIFEST_TTL_MS: '500' })
  start('control', join(BIN, 'la-control'), ['serve'], controlEnv)
  start('ingest', join(BIN, 'la-ingest'), [], { ...storeEnv, LA_LISTEN: `127.0.0.1:${pIngest}`, LA_NODE_ID: 'demo-01', LA_WAL_DIR: join(WORK, 'wal'), LA_WAL_SEGMENT_SECONDS: '1', LA_TOKENS_POLL_SECONDS: '1', LA_FLAGS_CACHE_SECONDS: '1', LA_RATE_IP: '0', LA_DRAIN_SECONDS: '0', LA_GEO_DB: join(WORK, 'geo.mmdb'), LA_TRUSTED_PROXIES: '1' })
  start('shipper', SHIPPER, ['ship'], { ...storeEnv, LA_METRICS_ADDR: 'off', LA_SHIPPER_POLL_SECS: '1', LA_SHIPPER_BATCH_SECS: '0' })
  for (const [name, url] of [['ingest', INGEST], ['query', QUERY_URL], ['control', CONTROL]]) await waitFor(`${name} /readyz`, async () => (await fetch(url + '/readyz')).status == 200, 30_000)

  const api = async (method, path, body) => {
    const res = await fetch(CONTROL + path, { method, headers: { Authorization: `Bearer ${boot.secret}`, ...(body ? { 'Content-Type': 'application/json' } : {}) }, body: body ? JSON.stringify(body) : undefined })
    const text = await res.text()
    assert.ok(res.status < 300, `${method} ${path} → ${res.status} ${text.slice(0, 300)}`)
    return text ? JSON.parse(text) : null
  }
  const day = (o) => new Date(Date.now() + o * 86_400_000).toISOString().slice(0, 10)
  const sql = async (text) => {
    const res = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { Authorization: `Bearer ${boot.secret}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ from: day(-DAYS - 2), to: day(1), sql: text }) })
    if (res.status != 200) throw new Error(`query ${res.status} ${await res.text()}`)
    return (await res.json()).rows
  }

  // ── flags first, so the recorded session can evaluate one ──
  await api('POST', `${P}/flags`, { key: 'new_checkout', name: 'New checkout flow', description: 'One-page checkout, rolled out to half of the people on recent app versions.', enabled: true, rules: [{ when: { and: [{ prop: 'platform', op: 'in', value: ['ios', 'web'] }, { prop: 'app_version', op: 'semver_gte', value: '3.2.0' }] }, rollout: 5000, variants: [{ key: 'control', w: 5000 }, { key: 'one_page', w: 5000 }] }, { when: { prop: 'plan', op: 'eq', value: 'pro' }, rollout: 10000 }], default: null })
  await api('POST', `${P}/flags`, { key: 'dark_mode', name: 'Dark mode', enabled: true, rules: [{ rollout: 10000 }], default: null })
  await api('POST', `${P}/flags`, { key: 'search_v2', name: 'Search v2', description: 'Disabled while the index is rebuilt.', enabled: false, rules: [{ when: { prop: 'country', op: 'in', value: ['TR', 'DE'] }, rollout: 2500, stable_on: 'anon_id' }], default: 'legacy' })

  // ── three weeks of synthetic traffic through the PostHog-compatible endpoint ──
  const now = Date.now()
  const DAY = 86_400_000
  const COUNTRIES = [['11.1', 30], ['11.2', 20], ['11.3', 20], ['11.4', 12], ['11.5', 10], ['11.6', 8]]
  const PRODUCTS = [['bag-01', 59], ['mug-07', 14.5], ['plant-3', 22], ['lamp-12', 89], ['notebook-5', 9.9], ['chair-2', 249]]
  let seq = 0
  let total = 0
  const batches = []
  for (let u = 1; u <= USERS; u++) {
    const platform = pick([['web', 55], ['ios', 25], ['android', 20]])
    const base =
      platform == 'web'
        ? { $lib: 'web', $os: pick([['Windows', 5], ['macOS', 4], ['Linux', 1]]), $device_type: pick([['Desktop', 7], ['Mobile', 3]]), $referring_domain: pick([['google.com', 5], ['instagram.com', 2], ['newsletter.example', 2], ['', 3]]) }
        : platform == 'ios'
          ? { $lib: 'posthog-ios', $os: 'iOS', $os_version: pick([['17.5', 1], ['18.1', 2]]), $device_type: 'Mobile', $device_model: 'iPhone15,2', $app_version: pick([['3.1.0', 2], ['3.2.0', 3], ['3.3.0', 4]]) }
          : { $lib: 'posthog-android', $os: 'Android', $os_version: pick([['14', 2], ['15', 3]]), $device_type: 'Mobile', $device_model: 'Pixel 8', $app_version: pick([['3.1.0', 3], ['3.2.0', 3], ['3.3.0', 2]]) }
    const ip = `${pick(COUNTRIES)}.${1 + Math.floor(rnd() * 250)}.${1 + Math.floor(rnd() * 250)}`
    const anon = `anon-${u.toString(36).padStart(4, '0')}`
    const user = `user-${1000 + u}`
    const firstDayAgo = Math.floor(DAYS * Math.pow(rnd(), 1.5)) // more people joined recently
    const events = []
    let id = anon
    let signedUp = false
    const at = (dayAgo, minute) => now - dayAgo * DAY - (now % DAY) + Math.min(minute, 1439) * 60_000 + Math.floor(rnd() * 60_000)
    const ev = (ts, event, props = {}, extra = {}) => {
      if (ts > now) return
      events.push({ event, distinct_id: id, uuid: `0193d000-${(u & 0xffff).toString(16).padStart(4, '0')}-7000-8000-${(++seq).toString(16).padStart(12, '0')}`, timestamp: new Date(ts).toISOString(), properties: { ...base, ...props }, ...extra })
    }
    const visit = (dayAgo, first) => {
      let m = 7 * 60 + Math.floor(rnd() * 15 * 60) // between 07:00 and 22:00 UTC
      const page = (path) => (platform == 'web' ? { $pathname: path } : { $screen_name: path == '/' ? 'Home' : path.startsWith('/products') ? 'Product' : 'Cart' })
      ev(at(dayAgo, m), platform == 'web' ? 'pageview' : 'screen', page('/'))
      if (first && rnd() < 0.62) {
        m += 2
        ev(at(dayAgo, m), 'signup', { method: pick([['email', 6], ['google', 3], ['apple', 1]]) })
        const from = id
        id = user
        signedUp = true
        ev(at(dayAgo, m), '$identify', { $anon_distinct_id: from }, { $set: { plan: pick([['free', 8], ['pro', 2]]), signup_platform: platform } })
      }
      const interested = rnd() < (first ? 0.8 : 0.7)
      if (!interested) return
      const [sku, price] = PRODUCTS[Math.floor(rnd() * PRODUCTS.length)]
      for (let i = 0, n = 1 + Math.floor(rnd() * 3); i < n; i++) ev(at(dayAgo, (m += 1 + Math.floor(rnd() * 4))), 'product_viewed', { ...page('/products/' + sku), sku, price })
      if (rnd() < 0.45) {
        ev(at(dayAgo, (m += 2)), 'add_to_cart', { sku, price })
        if (signedUp && rnd() < 0.62) {
          ev(at(dayAgo, (m += 3)), 'checkout_started', { items: 1 })
          if (rnd() < 0.72) ev(at(dayAgo, (m += 2)), 'purchase', { amount: Math.round(price * (1 + Math.floor(rnd() * 2)) * 100) / 100, sku, currency: 'EUR' })
        }
      }
    }
    visit(firstDayAgo, true)
    for (let k = 1; k <= firstDayAgo; k++) if (rnd() < 0.05 + 0.3 * Math.exp(-k / 6)) visit(firstDayAgo - k, false)
    if (events.length) {
      total += events.length
      batches.push({ ip, events })
    }
  }
  console.log(`sending ${total} events from ${batches.length} people …`)
  let next = 0
  await Promise.all(
    Array.from({ length: 24 }, async () => {
      for (;;) {
        const b = batches[next++]
        if (!b) return
        const res = await fetch(`${INGEST}/batch/`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Forwarded-For': b.ip }, body: JSON.stringify({ api_key: boot.token, batch: b.events }) })
        await res.arrayBuffer()
        assert.equal(res.status, 200)
      }
    }),
  )
  await waitFor('every seeded event in the query service', async () => (await sql("SELECT count(*) FROM events WHERE event <> 'flag_exposed'"))[0][0] >= total, 180_000, 1000)

  // ── one real browser session, recorded by the web SDK ──
  const { chromium } = createRequire(import.meta.url)('playwright')
  browser = await chromium.launch()
  const site = createServer(async (req, res) => {
    const path = new URL(req.url, 'http://site').pathname
    try {
      if (path.startsWith('/sdk/')) {
        res.setHeader('Content-Type', 'text/javascript')
        return res.end(await readFile(join(ROOT, 'sdk-web/dist', path.slice(5))))
      }
      if (path == '/favicon.ico') return res.writeHead(204).end()
      res.setHeader('Content-Type', 'text/html; charset=utf-8')
      res.end(await readFile(join(ROOT, 'e2e/demo-shop.html')))
    } catch (e) {
      res.writeHead(404).end(String(e))
    }
  })
  await new Promise((r) => site.listen(0, '127.0.0.1', r))
  {
    const context = await browser.newContext({ userAgent: UA, viewport: { width: 1100, height: 760 }, extraHTTPHeaders: {} })
    await context.route(`${INGEST}/**`, (route) => route.continue({ headers: { ...route.request().headers(), 'x-forwarded-for': '11.1.20.30' } }))
    const page = await context.newPage()
    let chunks = 0
    page.on('response', (r) => r.url().startsWith(`${INGEST}/v1/r`) && r.status() == 200 && chunks++)
    await page.goto(`http://127.0.0.1:${site.address().port}/?${new URLSearchParams({ host: INGEST, token: boot.token, utm_source: 'newsletter' })}`)
    await page.evaluate(() => window.flagsReady)
    await page.evaluate(() => {
      izlek.identify('user-ada', { plan: 'pro', name: 'Ada', signup_platform: 'web' })
      izlekFlags.flag('new_checkout')
    })
    const pause = (ms) => page.waitForTimeout(ms)
    await pause(1200)
    await page.mouse.move(300, 300)
    await page.locator('#add-1').click()
    await pause(900)
    await page.locator('#add-3').click()
    await pause(1200)
    await page.locator('#nav-products').click()
    await pause(1500)
    await page.mouse.wheel(0, 260)
    await pause(1200)
    await page.locator('#email').pressSequentially('ada@example.com', { delay: 45 })
    await pause(600)
    await page.locator('#card').pressSequentially('4242424242424242', { delay: 35 })
    await pause(900)
    await page.locator('#pay').click()
    await pause(2500)
    await page.mouse.wheel(0, -260)
    await pause(6000) // let the last 5-second chunk close and upload
    await page.evaluate(() => izlek.flush())
    await waitFor('replay chunks uploaded', () => chunks >= 2, 20_000)
    await context.close()
  }
  site.close()
  const session = await waitFor('recorded session in the query service', async () => (await sql("SELECT hex(session_id) FROM events WHERE event = 'purchase' AND user_id = 'user-ada'"))[0]?.[0], 60_000, 1000)
  await waitFor('replay index events', async () => (await sql(`SELECT count(*) FROM events WHERE event = 'session_replay_chunk' AND hex(session_id) = '${session}'`))[0][0] >= 2, 60_000, 1000)

  // ── insights and a dashboard ──
  const insight = async (name, kind, query) => (await api('POST', `${P}/insights`, { name, kind, query })).id
  const iActive = await insight('Daily active people and purchases', 'trends', { range: '30d', interval: 'day', series: [{ event: null, agg: 'unique', label: 'Active people' }, { event: 'purchase', agg: 'count', label: 'Purchases' }] })
  const iFunnel = await insight('Signup → purchase funnel', 'funnel', { range: '30d', steps: [{ event: 'signup' }, { event: 'product_viewed' }, { event: 'add_to_cart' }, { event: 'purchase' }], window_seconds: 604800, order: 'ordered', breakdown: 'platform' })
  const iRetention = await insight('Daily retention after signup', 'retention', { range: '14d', period: 'day', periods: 8, start: { event: 'signup' }, return: { event: null } })
  const iRevenue = await insight('Revenue by platform', 'trends', { range: '30d', interval: 'week', series: [{ event: 'purchase', agg: 'sum', prop: 'props.amount', label: 'Revenue (€)' }], breakdown: 'platform', breakdown_limit: 10, ui: { chart: 'bar' } })
  const iCountry = await insight('Signups by country', 'trends', { range: '30d', interval: 'week', series: [{ event: 'signup', agg: 'count', label: 'Signups' }], breakdown: 'country', breakdown_limit: 6, ui: { chart: 'bar' } })
  await insight('Top events this week', 'sql', { range: '7d', sql: 'SELECT event, count(*) AS events, count(DISTINCT person_key) AS people\nFROM events\nGROUP BY 1\nORDER BY 2 DESC\nLIMIT 10' })
  const dashboard = await api('POST', `${P}/dashboards`, { name: 'Overview', items: [{ insight_id: iActive, x: 0, y: 0, w: 8, h: 3 }, { insight_id: iRevenue, x: 8, y: 0, w: 4, h: 3 }, { insight_id: iFunnel, x: 0, y: 3, w: 6, h: 3 }, { insight_id: iRetention, x: 6, y: 3, w: 6, h: 3 }] })
  await api('POST', `${P}/deletions`, { user_ids: ['user-1042'], note: 'Support ticket #1841' })

  const url = (path) => `${CONTROL}/p/${boot.project_id}/${path}`
  const pages = [
    ['dashboard', `dashboards/${dashboard.id}`],
    ['trends', `insights/${iActive}`],
    ['trends-breakdown', `insights/${iCountry}`],
    ['funnel', `insights/${iFunnel}`],
    ['retention', `insights/${iRetention}`],
    ['events', 'events'],
    ['person', `persons/${encodeURIComponent('u:user-ada')}`],
    ['replay', `replay/${session}`],
    ['flags', 'flags'],
    ['flag-editor', 'flags/new_checkout'],
    ['sdk-setup', 'settings/sdk'],
    ['mcp', 'settings/mcp'],
  ]

  if (SHOTS) {
    mkdirSync(SHOTS, { recursive: true })
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, locale: LANG == 'tr' ? 'tr-TR' : 'en-US', colorScheme: 'light' })
    await context.addInitScript((lang) => {
      try {
        localStorage.setItem('la_locale', lang)
      } catch {}
    }, LANG)
    const login = await context.request.post(`${CONTROL}/api/auth/login`, { data: OWNER })
    assert.equal(login.status(), 200, await login.text())
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', (e) => errors.push(String(e)))
    for (const [name, path] of pages) {
      await page.goto(url(path))
      await page.waitForSelector('.main h1')
      await page.waitForLoadState('networkidle')
      if (name == 'mcp') await page.locator('[data-testid="mcp-status"][data-state="oauth"]').waitFor()
      if (name == 'replay') {
        await page.locator('canvas.replay-canvas').waitFor()
        await page.locator('.replay-play').click()
        await page.waitForFunction(() => Number(document.querySelector('canvas.replay-canvas').dataset.time) > 9000, null, { timeout: 60_000 })
        await page.locator('.replay-play').click()
      }
      await page.waitForTimeout(500)
      await page.screenshot({ path: join(SHOTS, `${name}.png`) })
      console.log(`  ${name}.png`)
    }
    // The consent page an assistant's user sees when connecting over OAuth: register a client the
    // way claude.ai does (dynamic registration grants nothing by itself) and open the authorization request.
    {
      const reg = await context.request.post(`${CONTROL}/oauth/register`, { data: { client_name: 'Claude', redirect_uris: ['https://claude.ai/api/mcp/auth_callback'], token_endpoint_auth_method: 'none' } })
      assert.equal(reg.status(), 201, await reg.text())
      const { client_id } = await reg.json()
      const { createHash, randomBytes } = await import('node:crypto')
      const challenge = createHash('sha256').update(randomBytes(32).toString('base64url')).digest('base64url')
      const q = new URLSearchParams({ response_type: 'code', client_id, redirect_uri: 'https://claude.ai/api/mcp/auth_callback', code_challenge: challenge, code_challenge_method: 'S256', scope: 'izlek:read izlek:admin', state: 'demo', resource: `${CONTROL}/mcp`, ui_locales: LANG })
      await page.setViewportSize({ width: 1100, height: 820 })
      await page.goto(`${CONTROL}/oauth/authorize?${q}`)
      await page.waitForLoadState('networkidle')
      await page.screenshot({ path: join(SHOTS, 'mcp-consent.png') })
      console.log('  mcp-consent.png')
    }
    assert.deepEqual(errors, [], 'no page errors while taking screenshots')
    await context.close()
  }

  if (SERVE) {
    console.log(`\nUI:        ${CONTROL}/   (${OWNER.email} / ${OWNER.password})`)
    console.log(`Ingestion: ${INGEST}   write token ${boot.token}`)
    console.log(`Query:     ${QUERY_URL}   secret key ${boot.secret}`)
    console.log(`Pages:     ${pages.map(([, p]) => url(p)).join('\n           ')}`)
    console.log(`Data dir:  ${WORK}\nCtrl-C stops everything and deletes the data.`)
    await new Promise((r) => process.once('SIGINT', r).once('SIGTERM', r))
  }
  ok = true
} catch (e) {
  console.error('demo FAILED:', e?.stack || e)
} finally {
  await browser?.close().catch(() => {})
  for (const p of [...procs].reverse()) if (p.code === undefined) p.child.kill('SIGTERM')
  await sleep(1500)
  for (const p of procs) if (p.code === undefined) p.child.kill('SIGKILL')
  if (ok) rmSync(WORK, { recursive: true, force: true })
  else console.log(`work dir kept: ${WORK}`)
}
process.exit(ok ? 0 : 1)
