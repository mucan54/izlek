// End-to-end test against the real binaries on one machine:
//
//   SDK / HTTP client → la-ingest → WAL → la-shipper → Parquet → la-query
//                        la-control (API, publishing, query proxy, UI)
//
//   node e2e/run.mjs [--s3] [--keep] [--skip-kmp] [--skip-ui] [--verbose]
//
// --s3 puts every service on the S3 protocol instead of the file:// store:
// by default the repository's devs3 server (no signature checks). To use a
// real S3 implementation, export LA_E2E_S3_ENDPOINT, LA_E2E_S3_BUCKET (must
// exist), LA_E2E_S3_ACCESS_KEY and LA_E2E_S3_SECRET_KEY; the test then looks
// at objects through the `aws` CLI.
//
// Go binaries are built here. The Rust binaries must exist already:
//   (cd engine && cargo build -p la-shipper -p la-query)
// or point LA_E2E_SHIPPER / LA_E2E_QUERY at them. Uses the Playwright that is
// installed on the machine (not a dependency of this repository).
import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { createRequire } from 'node:module'
import { createServer as netServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const args = new Set(process.argv.slice(2))
const KEEP = args.has('--keep')
const VERBOSE = args.has('--verbose')
const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36'

const firstExisting = (...c) => c.find((p) => p && existsSync(p))
const SHIPPER = firstExisting(process.env.LA_E2E_SHIPPER, join(ROOT, 'engine/target/release/la-shipper'), join(ROOT, 'engine/target/debug/la-shipper'))
const QUERY = firstExisting(
  process.env.LA_E2E_QUERY,
  join(ROOT, 'engine/target/release/la-query'),
  join(ROOT, 'engine/target/debug/la-query'),
  join(ROOT, 'engine/target-query/debug/la-query'),
)
if (!SHIPPER || !QUERY) {
  console.error('la-shipper / la-query not found. Build them first: (cd engine && cargo build -p la-shipper -p la-query)')
  process.exit(2)
}

const WORK = mkdtempSync(join(tmpdir(), 'izlek-e2e-'))
const BIN = join(WORK, 'bin')
const S3 = args.has('--s3')
// With --s3 this is where devs3 keeps the bucket, so the test can look at objects either way.
const EXT = S3 && process.env.LA_E2E_S3_ENDPOINT ? { endpoint: process.env.LA_E2E_S3_ENDPOINT, bucket: process.env.LA_E2E_S3_BUCKET, prefix: `e2e-${Date.now()}`, region: process.env.LA_E2E_S3_REGION || 'us-east-1' } : null
const STORE = S3 ? join(WORK, 's3', 'e2e') : join(WORK, 'store')
const LOGS = join(WORK, 'logs')
for (const d of [BIN, STORE, LOGS]) mkdirSync(d, { recursive: true })

// ───────────────────────── helpers ─────────────────────────

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(what, cond, ms = 60_000, every = 250) {
  const t0 = Date.now()
  let last
  for (;;) {
    try {
      const v = await cond()
      if (v) return v
      last = v
    } catch (e) {
      last = e
    }
    if (Date.now() - t0 > ms) throw new Error(`timed out waiting for ${what}${last instanceof Error ? ': ' + last.message : last !== undefined ? ` (last: ${JSON.stringify(last)})` : ''}`)
    await sleep(every)
  }
}

const freePort = () =>
  new Promise((res, rej) => {
    const s = netServer()
    s.once('error', rej)
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address()
      s.close(() => res(port))
    })
  })

const procs = []
function start(name, cmd, argv, env) {
  const log = createWriteStream(join(LOGS, name + '.log'))
  const child = spawn(cmd, argv, { env: { ...process.env, ...env }, stdio: ['ignore', 'pipe', 'pipe'] })
  child.stdout.pipe(log)
  child.stderr.pipe(log)
  const p = { name, child, exit: new Promise((r) => child.once('exit', (code, sig) => r(code ?? sig))) }
  p.exit.then((c) => (p.code = c))
  procs.push(p)
  return p
}

function run(name, cmd, argv, env, opts = {}) {
  const r = spawnSync(cmd, argv, { env: { ...process.env, ...env }, encoding: 'utf8', maxBuffer: 64 << 20, ...opts })
  if (VERBOSE || r.status !== 0) process.stderr.write(`── ${name} (exit ${r.status})\n${r.stdout}${r.stderr}\n`)
  return r
}

async function stop(p, signal = 'SIGTERM', ms = 30_000) {
  if (p.code !== undefined) return p.code
  p.child.kill(signal)
  const code = await Promise.race([p.exit, sleep(ms).then(() => 'timeout')])
  if (code === 'timeout') {
    p.child.kill('SIGKILL')
    await p.exit
  }
  return code
}

const tail = (file, n = 25) => {
  try {
    return readFileSync(file, 'utf8').trimEnd().split('\n').slice(-n).join('\n')
  } catch {
    return '(no log)'
  }
}

const walk = (dir) => {
  if (!existsSync(dir)) return []
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)]))
}
// The store as the test sees it: a directory (file://, devs3 -dir) or, for an external S3, the aws CLI.
function aws(args) {
  const r = spawnSync('aws', ['--endpoint-url', EXT.endpoint, ...args], {
    env: { ...process.env, AWS_ACCESS_KEY_ID: process.env.LA_E2E_S3_ACCESS_KEY, AWS_SECRET_ACCESS_KEY: process.env.LA_E2E_S3_SECRET_KEY, AWS_DEFAULT_REGION: EXT.region, AWS_EC2_METADATA_DISABLED: 'true' },
    maxBuffer: 64 << 20,
  })
  if (r.error) throw r.error
  return r
}
function storeKeys(prefix = '') {
  if (!EXT) return walk(join(STORE, prefix)).map((f) => f.slice(STORE.length + 1))
  const r = aws(['s3api', 'list-objects-v2', '--bucket', EXT.bucket, '--prefix', `${EXT.prefix}/${prefix}`, '--query', 'Contents[].Key', '--output', 'json'])
  assert.equal(r.status, 0, String(r.stderr))
  return (JSON.parse(String(r.stdout) || 'null') || []).map((k) => k.slice(EXT.prefix.length + 1))
}
const storeHas = (key) => (EXT ? storeKeys(key).includes(key) : existsSync(join(STORE, key)))
function storeRead(key) {
  if (!EXT) return readFileSync(join(STORE, key), 'utf8')
  const r = aws(['s3', 'cp', `s3://${EXT.bucket}/${EXT.prefix}/${key}`, '-'])
  assert.equal(r.status, 0, String(r.stderr))
  return String(r.stdout)
}

const steps = []
async function step(name, fn) {
  const t0 = Date.now()
  try {
    await fn()
    steps.push({ name, ok: true, ms: Date.now() - t0 })
    console.log(`  ok    ${name} (${Date.now() - t0} ms)`)
  } catch (e) {
    steps.push({ name, ok: false, ms: Date.now() - t0, err: e })
    console.log(`  FAIL  ${name}\n        ${String(e?.stack || e).replaceAll('\n', '\n        ')}`)
    throw e
  }
}
function skip(name, why) {
  steps.push({ name, ok: true, skipped: why })
  console.log(`  skip  ${name} — ${why}`)
}

// ───────────────────────── build ─────────────────────────

console.log(`work dir: ${WORK} (${EXT ? `s3://${EXT.bucket}/${EXT.prefix} at ${EXT.endpoint}` : S3 ? 's3:// via devs3' : 'file:// store'})`)
{
  const r = run('go build', 'go', ['build', '-o', BIN + '/', './ingest/cmd/la-ingest', './control/cmd/la-control', './canary/cmd/la-canary', './golib/devs3/cmd/devs3'], { CGO_ENABLED: '0' }, { cwd: ROOT })
  if (r.status !== 0) process.exit(2)
}
for (const f of ['sdk-web/dist/izlek.min.js', 'ui/dist/index.html']) {
  if (!existsSync(join(ROOT, f))) {
    console.error(`${f} is missing: run "npm ci && npm run build" in ${f.split('/')[0]}/`)
    process.exit(2)
  }
}

// ───────────────────────── services ─────────────────────────

const [pIngest, pQuery, pControl] = [await freePort(), await freePort(), await freePort()]
const INGEST = `http://127.0.0.1:${pIngest}`
const QUERY_URL = `http://127.0.0.1:${pQuery}`
const CONTROL = `http://127.0.0.1:${pControl}`
const INTERNAL = 'e2e-internal-secret'
const OWNER = { email: 'e2e@example.com', password: 'e2e-password-123' }
let storeEnv = { LA_STORE_URL: 'file://' + STORE }
if (EXT) {
  assert.ok(EXT.bucket && process.env.LA_E2E_S3_ACCESS_KEY && process.env.LA_E2E_S3_SECRET_KEY, 'LA_E2E_S3_BUCKET, LA_E2E_S3_ACCESS_KEY and LA_E2E_S3_SECRET_KEY are required with LA_E2E_S3_ENDPOINT')
  storeEnv = { LA_STORE_URL: `s3://${EXT.bucket}/${EXT.prefix}`, LA_S3_ENDPOINT: EXT.endpoint, LA_S3_ACCESS_KEY: process.env.LA_E2E_S3_ACCESS_KEY, LA_S3_SECRET_KEY: process.env.LA_E2E_S3_SECRET_KEY, LA_S3_REGION: EXT.region }
} else if (S3) {
  const pS3 = await freePort()
  start('devs3', join(BIN, 'devs3'), ['-listen', `127.0.0.1:${pS3}`, '-dir', join(WORK, 's3')], {})
  storeEnv = { LA_STORE_URL: 's3://e2e', LA_S3_ENDPOINT: `http://127.0.0.1:${pS3}`, LA_S3_ACCESS_KEY: 'e2e', LA_S3_SECRET_KEY: 'e2e', LA_S3_REGION: 'us-east-1' }
  await waitFor('devs3', async () => (await fetch(storeEnv.LA_S3_ENDPOINT + '/')).status > 0, 10_000)
}
const controlEnv = {
  ...storeEnv,
  LA_CONTROL_DB: join(WORK, 'control.db'),
  LA_CONTROL_LISTEN: `127.0.0.1:${pControl}`,
  LA_QUERY_URL: QUERY_URL,
  LA_QUERY_INTERNAL_SECRET: INTERNAL,
  LA_UI_DIR: join(ROOT, 'ui/dist'),
  LA_COOKIE_SECURE: 'false',
  LA_CONTROL_PUBLISH_DEBOUNCE_MS: '100',
  LA_CONTROL_DELETION_CHECK_SECONDS: '1',
  LA_PUBLIC_INGEST_URL: INGEST,
}
const engineEnv = { ...storeEnv, LA_METRICS_ADDR: 'off' }

let boot
let ingest
const fail = []

// A site on its own origin (so CORS is real) that serves the SDK bundles.
const site = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://site').pathname
  try {
    if (path.startsWith('/sdk/')) {
      res.setHeader('Content-Type', 'text/javascript')
      return res.end(await readFile(join(ROOT, 'sdk-web/dist', path.slice(5))))
    }
    if (path == '/favicon.ico') return res.writeHead(204).end()
    res.setHeader('Content-Type', 'text/html; charset=utf-8')
    res.end(await readFile(join(ROOT, 'e2e/page.html')))
  } catch (e) {
    res.writeHead(404).end(String(e))
  }
})
await new Promise((r) => site.listen(0, '127.0.0.1', r))
const SITE = `http://127.0.0.1:${site.address().port}`

// ───────────────────────── API clients ─────────────────────────

const day = (offset) => new Date(Date.now() + offset * 86_400_000).toISOString().slice(0, 10)
const RANGE = { from: day(-1), to: day(1) }

async function q(endpoint, body, { key = boot.secret, expect = 200, range = RANGE } = {}) {
  const res = await fetch(`${QUERY_URL}/q/${endpoint}`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...range, ...body }),
  })
  const text = await res.text()
  assert.equal(res.status, expect, `/q/${endpoint} → ${res.status} ${text.slice(0, 400)}`)
  return text ? JSON.parse(text) : null
}
const sql = async (text) => (await q('sql', { sql: text })).rows
const scalar = async (text) => (await sql(text))[0]?.[0] ?? 0
const count = (where) => scalar(`SELECT count(*) FROM events WHERE ${where}`)

// Admin API with the secret key (no cookie, no CSRF).
async function api(method, path, body, { expect = [200, 201, 204] } = {}) {
  const res = await fetch(CONTROL + path, {
    method,
    headers: { Authorization: `Bearer ${boot.secret}`, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  assert.ok([expect].flat().includes(res.status), `${method} ${path} → ${res.status} ${text.slice(0, 400)}`)
  return text ? JSON.parse(text) : null
}

async function posthog(batch) {
  const res = await fetch(`${INGEST}/batch/`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ api_key: boot.token, batch }),
  })
  assert.equal(res.status, 200, await res.text())
}

const P = () => `/api/projects/${boot.project_id}`

// ───────────────────────── scenario ─────────────────────────

let browser
const counts = {}
try {
  await step('bootstrap creates user, org, project, token, key and publishes the control files', async () => {
    const r = run('bootstrap', join(BIN, 'la-control'), ['bootstrap', '--email', OWNER.email, '--password', OWNER.password, '--org', 'E2E', '--project', 'Shop', '--json'], controlEnv)
    assert.equal(r.status, 0)
    boot = JSON.parse(r.stdout.trim().split('\n').pop())
    assert.match(boot.token, /^pk_[0-9A-Za-z]{32}$/)
    assert.match(boot.secret, /^sk_[0-9A-Za-z]{32}$/)
    for (const f of ['control/tokens.json', 'control/keys.json', 'control/projects.json']) assert.ok(storeHas(f), f)
    assert.ok(!storeRead('control/keys.json').includes(boot.secret), 'only the hash of the secret is published')
    // Running it again creates nothing and prints no secret.
    const again = JSON.parse(run('bootstrap again', join(BIN, 'la-control'), ['bootstrap', '--email', OWNER.email, '--password', OWNER.password, '--org', 'E2E', '--project', 'Shop', '--json'], controlEnv).stdout.trim())
    assert.deepEqual([again.created, again.secret, again.token], [[], undefined, boot.token])
  })

  await step('services start and report ready', async () => {
    start('query', QUERY, [], {
      ...engineEnv,
      LA_QUERY_LISTEN: `127.0.0.1:${pQuery}`,
      LA_QUERY_INTERNAL_SECRET: INTERNAL,
      LA_QUERY_CACHE_DIR: join(WORK, 'qcache'),
      LA_QUERY_MANIFEST_TTL_MS: '200',
      LA_QUERY_TOMBSTONE_TTL_MS: '500',
      LA_QUERY_CONTROL_REFRESH_MS: '500',
    })
    start('control', join(BIN, 'la-control'), ['serve'], controlEnv)
    ingest = start('ingest', join(BIN, 'la-ingest'), [], {
      ...storeEnv,
      LA_LISTEN: `127.0.0.1:${pIngest}`,
      LA_NODE_ID: 'e2e-01',
      LA_WAL_DIR: join(WORK, 'wal'),
      LA_WAL_SEGMENT_SECONDS: '0.5',
      LA_TOKENS_POLL_SECONDS: '1',
      LA_FLAGS_CACHE_SECONDS: '1',
      LA_DRAIN_SECONDS: '0',
    })
    start('shipper', SHIPPER, ['ship'], { ...engineEnv, LA_SHIPPER_POLL_SECS: '1', LA_SHIPPER_BATCH_SECS: '0' })
    for (const [name, url] of [['ingest', INGEST], ['query', QUERY_URL], ['control', CONTROL]]) {
      await waitFor(`${name} /readyz`, async () => (await fetch(url + '/readyz')).status == 200, 30_000)
    }
  })

  await step('canary: every event it sends becomes visible exactly once', async () => {
    const r = run('la-canary', join(BIN, 'la-canary'), ['-once', '-batches', '3', '-timeout', '90s', '-poll', '1s'], {
      LA_CANARY_INGEST_URL: INGEST,
      LA_CANARY_TOKEN: boot.token,
      LA_CANARY_QUERY_URL: QUERY_URL,
      LA_CANARY_QUERY_KEY: boot.secret,
      LA_CANARY_LISTEN: 'off',
    })
    assert.equal(r.status, 0, 'la-canary -once failed')
  })

  await step('authentication and isolation at the edges', async () => {
    const bad = await fetch(`${INGEST}/v1/e`, { method: 'POST', headers: { Authorization: 'Bearer pk_' + 'x'.repeat(32) }, body: 'LA1\0' })
    assert.equal(bad.status, 401)
    await q('sql', { sql: 'SELECT 1' }, { key: 'sk_' + 'x'.repeat(32), expect: 401 })
    await q('sql', { sql: 'SELECT 1' }, { key: boot.token, expect: 401 }) // a write token is not a query key
    const forged = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { 'X-LA-Internal': 'wrong', 'X-LA-Project': '1' }, body: '{}' })
    assert.equal(forged.status, 401)
    await q('sql', { sql: 'DROP TABLE events' }, { expect: 400 })
    await q('sql', { sql: "SELECT * FROM read_parquet('/etc/passwd')" }, { expect: 400 })
    assert.equal((await fetch(`${CONTROL}/api/me`)).status, 401)
    const proxyNoAuth = await fetch(`${CONTROL}${P()}/q/sql`, { method: 'POST', body: '{}' })
    assert.equal(proxyNoAuth.status, 401)
  })

  let flagValue
  await step('flag: created through the API, published, evaluated the same by server and ingestion-served ruleset', async () => {
    const flag = await api('POST', `${P()}/flags`, {
      key: 'new_checkout',
      name: 'Yeni ödeme akışı',
      enabled: true,
      rules: [{ rollout: 10000, variants: [{ key: 'control', w: 5000 }, { key: 'b', w: 5000 }] }],
      default: null,
    })
    assert.ok(flag.salt, 'the server generates the salt')
    await waitFor('ruleset on the ingestion node', async () => {
      const res = await fetch(`${INGEST}/flags/${boot.token}.json`)
      if (res.status != 200) return false
      const body = await res.json()
      assert.ok(res.headers.get('etag'))
      return body.flags.some((f) => f.key == 'new_checkout' && f.salt == flag.salt)
    })
    flagValue = async (user_id, anon_id = 'none') => (await api('POST', `${P()}/flags/new_checkout/evaluate`, { props: {}, user_id, anon_id })).value
    assert.ok(['control', 'b'].includes(await flagValue('web-user-1')))
  })

  const { chromium } = createRequire(import.meta.url)('playwright')
  browser = await chromium.launch()
  let webSession
  const SECRET_INPUT = 'hunter2-never-recorded'

  await step('web SDK in Chromium: events, identify, flag, replay upload', async () => {
    const context = await browser.newContext({ userAgent: UA, viewport: { width: 900, height: 700 } })
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', (e) => errors.push(String(e)))
    page.on('console', (m) => m.type() == 'error' && errors.push(m.text()))
    const replayOk = page.waitForResponse((r) => r.url().startsWith(`${INGEST}/v1/r`) && r.request().method() == 'POST' && r.status() == 200, { timeout: 30_000 })
    await page.goto(`${SITE}/shop?${new URLSearchParams({ host: INGEST, token: boot.token, replay: '1', utm_source: 'e2e' })}`)
    await page.evaluate(() => window.flagsReady)
    // Three events while anonymous, then identify, then one more.
    await page.evaluate(() => {
      for (let n = 1; n <= 3; n++) izlek.capture('e2e_web', { n, fiyat: 129.9, etiket: 'ilk' })
    })
    await page.locator('#name').fill('Deneme Kullanıcı')
    await page.locator('#pw').fill(SECRET_INPUT)
    await page.locator('#buy').click()
    const flagInPage = await page.evaluate(async () => {
      izlek.identify('web-user-1', { plan: 'pro' })
      const v = izlekFlags.flag('new_checkout')
      izlek.capture('purchase', { amount: 12.5 })
      await izlek.flush()
      return v
    })
    assert.equal(flagInPage, await flagValue('web-user-1'), 'browser and server pick the same variant')
    await replayOk
    await page.evaluate(() => izlek.flush())
    await context.close()
    assert.deepEqual(errors, [], 'no console errors on the page')

    await waitFor('web events in the query service', async () => (await count("event = 'purchase'")) == 1 && (await count("event = 'e2e_web'")) == 3)
    const rows = await sql("SELECT DISTINCT person_key, platform, sdk, url_path FROM events WHERE event IN ('e2e_web', 'purchase', 'buy_click', 'pageview')")
    // Events sent before identify() belong to the same person afterwards.
    assert.deepEqual(rows.map((r) => r[0]), ['u:web-user-1'], JSON.stringify(rows))
    assert.equal(rows[0][1], 'web')
    assert.match(rows[0][2], /^web\//)
    assert.equal(rows[0][3], '/shop')
    assert.equal(await count("event = 'buy_click' AND prop(props, 'plan') = 'pro'"), 1)
    assert.equal(await scalar("SELECT prop_num(props, 'fiyat') FROM events WHERE event = 'e2e_web' LIMIT 1"), 129.9)
    const exposed = await sql("SELECT prop(props, 'variant') FROM events WHERE event = 'flag_exposed' AND prop(props, 'flag') = 'new_checkout'")
    assert.deepEqual(exposed, [[flagInPage]])
    webSession = await scalar("SELECT hex(session_id) FROM events WHERE event = 'purchase'")
    assert.match(webSession, /^[0-9a-f]{32}$/)
    const person = await sql("SELECT prop(props, 'plan') FROM persons WHERE person_key = 'u:web-user-1'")
    assert.deepEqual(person, [['pro']])
  })

  await step('replay: chunk is listed, downloads through the query service, decodes, and input stays masked', async () => {
    const { events } = await waitFor('replay index event', async () => {
      const r = await q('events', { filter: { and: [{ prop: 'event', op: 'eq', value: 'session_replay_chunk' }, { prop: 'session_id', op: 'eq', value: webSession }] }, order: 'asc' })
      return r.events.length ? r : null
    })
    const key = events[0].props.key
    assert.ok(key.startsWith(`replay/p=${boot.project_id}/`) && key.includes(webSession), key)
    assert.ok(storeHas(key), 'the chunk object exists in the store')
    const res = await fetch(`${QUERY_URL}/q/replay?key=${encodeURIComponent(key)}`, { headers: { Authorization: `Bearer ${boot.secret}` } })
    assert.equal(res.status, 200)
    const bytes = new Uint8Array(await res.arrayBuffer()) // fetch undoes Content-Encoding: gzip
    const { decodeChunk, newScene, applyFrame } = await import(join(ROOT, 'sdk-web/dist/replay-decode.js'))
    const chunk = decodeChunk(bytes)
    assert.ok(chunk.frames.length >= 1)
    const scene = newScene()
    for (const f of chunk.frames) applyFrame(scene, f)
    const text = JSON.stringify(chunk, (_, v) => (typeof v == 'bigint' ? String(v) : v))
    assert.ok(text.includes('Uçtan uca mağaza'), 'page text is in the wireframe')
    assert.ok(!text.includes(SECRET_INPUT) && !text.includes('Deneme Kullanıcı'), 'typed input is never recorded')
    // Another project's prefix is refused.
    const other = await fetch(`${QUERY_URL}/q/replay?key=${encodeURIComponent(key.replace(`p=${boot.project_id}/`, 'p=999/'))}`, { headers: { Authorization: `Bearer ${boot.secret}` } })
    assert.equal(other.status, 403)
  })

  const phBatch = [
    { event: 'ph_event', distinct_id: 'ph-anon-1', uuid: '01930000-0000-7000-8000-000000000001', timestamp: new Date(Date.now() - 60_000).toISOString(), properties: { step: 1, $lib: 'e2e', $current_url: 'https://shop.example/a?x=1' } },
    { event: '$identify', distinct_id: 'ph-user-1', uuid: '01930000-0000-7000-8000-000000000002', timestamp: new Date(Date.now() - 50_000).toISOString(), properties: { $anon_distinct_id: 'ph-anon-1' }, $set: { tier: 'gold' } },
    { event: 'ph_event', distinct_id: 'ph-user-1', uuid: '01930000-0000-7000-8000-000000000003', timestamp: new Date(Date.now() - 40_000).toISOString(), properties: { step: 2 } },
    { event: 'ph_event', distinct_id: 'ph-other-9', uuid: '01930000-0000-7000-8000-000000000004', timestamp: new Date(Date.now() - 30_000).toISOString(), properties: { step: 1 } },
  ]
  await step('PostHog wire compat: /batch JSON, $identify merges the anonymous history, a re-sent batch is not double counted', async () => {
    await posthog(phBatch)
    await waitFor('PostHog events', async () => (await count("event = 'ph_event'")) == 3)
    await posthog(phBatch) // the client retries the whole batch
    await sleep(3000) // let the shipper write the duplicates as a second file
    assert.equal(await count("event = 'ph_event'"), 3, 'duplicates collapse on uuid before compaction')
    const people = await sql("SELECT person_key, count(*) FROM events WHERE event = 'ph_event' GROUP BY 1 ORDER BY 1")
    assert.deepEqual(people, [['a:ph-other-9', 1], ['u:ph-user-1', 2]])
    assert.deepEqual(await sql("SELECT prop(props, 'tier') FROM persons WHERE person_key = 'u:ph-user-1'"), [['gold']])
    for (const p of ['/decide/?v=3', '/flags/?v=2']) assert.equal((await fetch(INGEST + p, { method: 'POST', body: '{}' })).status, 200)
  })

  if (args.has('--skip-kmp')) skip('KMP SDK (JVM sample)', '--skip-kmp')
  else if (spawnSync('java', ['-version']).status !== 0) skip('KMP SDK (JVM sample)', 'no JVM on this machine')
  else
    await step('KMP SDK (JVM sample): events, identify and the same flag value as the server', async () => {
      const r = run(
        'sample-jvm',
        join(ROOT, 'sdk-kmp/gradlew'),
        ['-p', join(ROOT, 'sdk-kmp'), '-q', ...(process.env.LA_E2E_GRADLE_ARGS || '--offline').split(' ').filter(Boolean), ':sample-jvm:run', `--args=--host ${INGEST} --token ${boot.token} --events 5 --event kmp_event --user kmp-user-1 --flag new_checkout`],
        {},
        { timeout: 600_000 },
      )
      assert.equal(r.status, 0, 'sample-jvm failed')
      const printed = /flag new_checkout = (\S+) \(ruleset downloaded\)/.exec(r.stdout)
      assert.ok(printed, r.stdout)
      assert.equal(printed[1], await flagValue('kmp-user-1'), 'Kotlin and Go evaluators agree on the published ruleset')
      await waitFor('KMP events', async () => (await count("event = 'kmp_event'")) == 5)
      const rows = await sql("SELECT DISTINCT person_key, sdk, prop(props, 'source') FROM events WHERE event = 'kmp_event'")
      assert.equal(rows.length, 1, JSON.stringify(rows))
      assert.equal(rows[0][0], 'u:kmp-user-1')
      assert.match(rows[0][1], /^kmp\//)
      assert.equal(rows[0][2], 'sample-jvm')
      assert.equal(await count("event = 'kmp_event' AND prop_bool(props, 'even')"), 3)
    })

  await step('query API: trends, funnel, retention, events paging, schema', async () => {
    const trends = await q('trends', { interval: 'day', series: [{ event: 'e2e_web', agg: 'count' }, { event: null, agg: 'unique' }], breakdown: 'platform' })
    assert.equal(trends.buckets.length, 3)
    const web = trends.series.find((s) => s.index == 0 && s.breakdown == 'web')
    assert.equal(web.values.reduce((a, b) => a + b, 0), 3)

    const funnel = await q('funnel', { steps: [{ event: 'e2e_web' }, { event: 'purchase', filter: { prop: 'props.amount', op: 'gt', value: 10 } }], window_seconds: 3600 })
    assert.deepEqual(funnel.steps.map((s) => s.count), [1, 1])
    assert.ok(funnel.steps[1].median_seconds >= 0)
    const strict = await q('funnel', { steps: [{ event: 'ph_event', filter: { prop: 'props.step', op: 'eq', value: 1 } }, { event: 'ph_event', filter: { prop: 'props.step', op: 'eq', value: 2 } }], order: 'ordered' })
    assert.deepEqual(strict.steps.map((s) => s.count), [2, 1], 'the anonymous step-1 event counts for the identified person')

    const retention = await q('retention', { period: 'day', periods: 3, start: { event: 'ph_event' }, return: { event: 'ph_event' } })
    assert.equal(retention.cohorts.reduce((a, c) => a + c.size, 0), 2)

    const seen = []
    let cursor
    do {
      const page = await q('events', { filter: { prop: 'event', op: 'in', value: ['e2e_web', 'ph_event'] }, limit: 4, order: 'asc', ...(cursor ? { cursor } : {}) })
      seen.push(...page.events.map((e) => e.uuid))
      cursor = page.next_cursor
    } while (cursor)
    assert.equal(seen.length, 6)
    assert.equal(new Set(seen).size, 6, 'paging returns every event once')

    const schema = await q('schema', {}, { range: {} })
    for (const name of ['e2e_web', 'purchase', 'ph_event']) assert.ok(schema.events.includes(name), name)
    assert.ok(schema.props.some((p) => p.key == 'amount'))
    assert.ok(schema.person_props.includes('plan'))
    // Person property filters use the current value.
    assert.equal(await (async () => (await q('trends', { interval: 'day', series: [{ event: 'e2e_web', agg: 'count' }], filter: { prop: 'person.plan', op: 'eq', value: 'pro' } })).series[0].values.reduce((a, b) => a + b, 0))(), 3)
  })

  await step('MCP: an assistant with a secret key sets up and checks an installation (real query service behind it)', async () => {
    let id = 0
    const rpc = async (method, params, { key = boot.secret, version } = {}) => {
      const res = await fetch(`${CONTROL}/mcp`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', ...(version ? { 'MCP-Protocol-Version': version } : {}) },
        body: JSON.stringify({ jsonrpc: '2.0', id: ++id, method, params }),
      })
      return { status: res.status, headers: res.headers, body: res.status == 202 ? null : await res.json().catch(() => null) }
    }
    // No credential → 401 that tells an OAuth client where to look (or, without a public URL, at least Bearer).
    const anon = await fetch(`${CONTROL}/mcp`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' })
    assert.equal(anon.status, 401)
    assert.match(anon.headers.get('www-authenticate') || '', /^Bearer/)
    assert.equal((await rpc('tools/list', {}, { key: boot.token })).status, 401, 'a write token is not a credential for MCP')

    const init = await rpc('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'e2e', version: '0' } })
    assert.equal(init.status, 200, JSON.stringify(init.body))
    const V = init.body.result.protocolVersion
    assert.equal(V, '2025-06-18')
    assert.ok(init.body.result.serverInfo.name)
    const call = async (name, args = {}) => {
      const r = await rpc('tools/call', { name, arguments: args }, { version: V })
      assert.equal(r.status, 200, `${name}: ${JSON.stringify(r.body)}`)
      assert.ok(!r.body.error && !r.body.result.isError, `${name}: ${JSON.stringify(r.body).slice(0, 500)}`)
      return r.body.result.structuredContent
    }
    const tools = (await rpc('tools/list', {}, { version: V })).body.result.tools.map((t) => t.name)
    for (const t of ['list_projects', 'get_sdk_setup', 'verify_installation', 'create_flag', 'query_funnel', 'run_sql']) assert.ok(tools.includes(t), t)
    assert.ok(!tools.includes('create_project'), 'a project key cannot create projects')

    const setup = await call('get_sdk_setup', { platform: 'web' })
    const text = JSON.stringify(setup)
    assert.ok(text.includes(boot.token) && text.includes(INGEST), 'the snippet carries the real write token and ingestion host')

    const seen = await call('verify_installation', { minutes: 60 })
    const seenText = JSON.stringify(seen)
    assert.ok(seenText.includes('e2e_web') && seenText.includes('purchase'), seenText.slice(0, 400))

    const funnel = await call('query_funnel', { ...RANGE, steps: [{ event: 'e2e_web' }, { event: 'purchase' }], window_seconds: 3600 })
    assert.deepEqual(funnel.steps.map((s) => s.count), [1, 1])
    const rows = await call('run_sql', { ...RANGE, sql: "SELECT count(*) FROM events WHERE event = 'e2e_web'" })
    assert.deepEqual(rows.rows, [[3]])

    await call('create_flag', { key: 'mcp_flag', name: 'Created by an assistant', enabled: true, rules: [{ rollout: 10000 }] })
    await waitFor('the flag in the published ruleset', async () => (await (await fetch(`${INGEST}/flags/${boot.token}.json`)).json()).flags.some((f) => f.key == 'mcp_flag'), 15_000)
    // An unknown tool is a protocol error, not a crash.
    const bad = await rpc('tools/call', { name: 'drop_everything', arguments: {} }, { version: V })
    assert.ok(bad.body.error, JSON.stringify(bad.body))
  })

  await step('compaction keeps every count (exactly-once across L0 → L1)', async () => {
    const snapshot = async () => Object.fromEntries(await sql('SELECT event, count(*) FROM events GROUP BY 1 ORDER BY 1'))
    const before = await snapshot()
    const filesBefore = storeKeys(`data/p=${boot.project_id}`).filter((k) => k.endsWith('.parquet')).length
    const r = run('compact', SHIPPER, ['compact', '--once', '--no-lease'], { ...engineEnv, LA_COMPACTOR_L0_FILES: '2', LA_COMPACTOR_GC_GRACE_HOURS: '0' })
    assert.equal(r.status, 0)
    await sleep(600) // manifest TTL of the query service
    const after = await snapshot()
    assert.deepEqual(after, before)
    Object.assign(counts, after)
    const manifestDir = storeKeys(`data/p=${boot.project_id}/d=${day(0)}/_m`).sort().pop()
    const manifest = JSON.parse(storeRead(manifestDir))
    assert.ok(manifest.files.some((f) => f.level >= 1), 'the day now has a compacted file')
    assert.ok(manifest.files.length < filesBefore, `${manifest.files.length} files listed, ${filesBefore} before`)
    assert.equal(await count("event = 'ph_event'"), 3)
  })

  await step('deletion request: tombstone hides the person at once, compaction removes the rows', async () => {
    const del = await api('POST', `${P()}/deletions`, { user_ids: ['ph-user-1'], note: 'e2e' })
    assert.equal(del.status, 'pending')
    await waitFor('tombstone object', () => storeKeys(`tombstones/p=${boot.project_id}`).some((k) => k.endsWith('.json')))
    // Both the identified rows and the earlier anonymous row of that person disappear.
    await waitFor('person hidden from queries', async () => (await count("event = 'ph_event'")) == 1, 15_000)
    assert.deepEqual(await sql("SELECT person_key FROM events WHERE event = 'ph_event'"), [['a:ph-other-9']])
    assert.deepEqual(await sql("SELECT count(*) FROM persons WHERE person_key = 'u:ph-user-1'"), [[0]])
    // Events of that person arriving later are dropped at write time.
    await posthog([{ event: 'ph_event', distinct_id: 'ph-user-1', uuid: '01930000-0000-7000-8000-000000000009', timestamp: new Date().toISOString(), properties: { step: 3 } }])
    const r = run('compact', SHIPPER, ['compact', '--once', '--no-lease'], { ...engineEnv, LA_COMPACTOR_L0_FILES: '2', LA_COMPACTOR_GC_GRACE_HOURS: '0' })
    assert.equal(r.status, 0)
    await sleep(3000)
    assert.equal(await count("event = 'ph_event'"), 1)
    await waitFor('deletion request marked done', async () => (await api('GET', `${P()}/deletions`)).find((d) => d.id == del.id)?.status == 'done', 30_000)
    assert.equal(await count("event = 'e2e_web'"), counts.e2e_web, 'other people are untouched')
  })

  if (args.has('--skip-ui')) skip('UI against the real control plane', '--skip-ui')
  else
    await step('UI served by the control plane: login, live events, trend chart, flag list, replay player', async () => {
      const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'tr-TR' })
      const page = await context.newPage()
      const errors = []
      page.on('pageerror', (e) => errors.push(String(e)))
      page.on('console', (m) => m.type() == 'error' && errors.push(m.text()))
      await page.goto(CONTROL + '/')
      await page.getByRole('heading', { name: 'Oturum aç' }).waitFor()
      await page.getByLabel('E-posta').fill(OWNER.email)
      await page.getByLabel('Parola').fill('wrong-password-1')
      await page.getByRole('button', { name: 'Oturum aç' }).click()
      await page.getByRole('alert').first().waitFor()
      errors.length = 0 // the 401 above is logged by the browser
      await page.getByLabel('Parola').fill(OWNER.password)
      await page.getByRole('button', { name: 'Oturum aç' }).click()
      await page.waitForURL(new RegExp(`/p/${boot.project_id}/dashboards`))

      await page.goto(`${CONTROL}/p/${boot.project_id}/events`)
      await page.getByText('e2e_web').first().waitFor()
      await page.getByText('purchase').first().waitFor()

      await page.goto(`${CONTROL}/p/${boot.project_id}/insights/new/trends`)
      await page.locator('.chart svg path.line').first().waitFor()

      await page.goto(`${CONTROL}/p/${boot.project_id}/flags`)
      await page.getByText('new_checkout').first().waitFor()

      const chunkLoaded = page.waitForResponse((r) => r.url().includes('/q/replay?key=') && r.status() == 200, { timeout: 30_000 })
      await page.goto(`${CONTROL}/p/${boot.project_id}/replay/${webSession}`)
      await chunkLoaded
      await page.locator('canvas.replay-canvas').waitFor()
      if (process.env.LA_E2E_SHOTS) await page.screenshot({ path: join(process.env.LA_E2E_SHOTS, 'replay.png') })

      // The session cookie cannot be used cross-site without the CSRF header.
      const noCsrf = await page.evaluate(async (path) => (await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"sql":"SELECT 1"}' })).status, `${P()}/q/sql`)
      assert.equal(noCsrf, 403)
      assert.deepEqual(errors.filter((e) => !e.includes('403')), [], 'no console errors in the UI')
      await context.close()
    })

  await step('token disabled, then deleted in the control plane → ingestion answers 403, then 401, within the poll interval', async () => {
    const tok = await api('POST', `${P()}/tokens`, { name: 'temporary' })
    const send = async () => (await fetch(`${INGEST}/batch/`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ api_key: tok.token, batch: [{ event: 'tmp', distinct_id: 'x' }] }) })).status
    await waitFor('new token accepted', async () => (await send()) == 200, 15_000)
    await waitFor('ruleset published for the new token', async () => (await fetch(`${INGEST}/flags/${tok.token}.json`)).status == 200, 15_000)
    await api('PATCH', `${P()}/tokens/${tok.token}`, { status: 'disabled' })
    await waitFor('disabled token refused', async () => (await send()) == 403, 15_000)
    assert.equal((await fetch(`${INGEST}/flags/${tok.token}.json`)).status, 404)
    await api('DELETE', `${P()}/tokens/${tok.token}`, null)
    await waitFor('deleted token unknown', async () => (await send()) == 401, 15_000)
    assert.ok(!storeHas(`flags/${tok.token}.json`), 'its ruleset file is removed')
  })

  await step('ingestion drains on SIGTERM: exit 0, nothing left on local disk, late events still arrive', async () => {
    await posthog([{ event: 'last_words', distinct_id: 'ph-other-9', uuid: '01930000-0000-7000-8000-00000000000a' }])
    assert.equal(await stop(ingest), 0)
    const left = walk(join(WORK, 'wal')).filter((f) => /\.(wal|sealed)$/.test(f))
    assert.deepEqual(left, [], 'every segment was uploaded')
    await waitFor('event accepted just before shutdown', async () => (await count("event = 'last_words'")) == 1, 30_000)
  })

  await step('closing the project: key stops working, compactor deletes everything it stored', async () => {
    await api('DELETE', P(), null)
    await waitFor('closed flag published', () => JSON.parse(storeRead('control/projects.json')).projects[boot.project_id]?.closed === true, 10_000)
    await waitFor('query refuses the closed project', async () => {
      const res = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { Authorization: `Bearer ${boot.secret}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ ...RANGE, sql: 'SELECT 1' }) })
      return res.status == 401 || res.status == 403
    }, 15_000)
    const r = run('compact', SHIPPER, ['compact', '--once', '--no-lease'], engineEnv)
    assert.equal(r.status, 0)
    for (const prefix of ['data', 'replay', 'identity', 'persons', 'tombstones']) assert.deepEqual(storeKeys(`${prefix}/p=${boot.project_id}`), [], prefix)
  })
} catch {
  fail.push(1)
} finally {
  await browser?.close().catch(() => {})
  site.close()
  for (const p of [...procs].reverse()) {
    const code = await stop(p)
    if (p.name != 'ingest' && code !== 0 && code !== 'SIGTERM') console.log(`  note  ${p.name} exited with ${code}`)
  }
}

const failed = steps.filter((s) => !s.ok)
console.log(`\n${steps.filter((s) => s.ok && !s.skipped).length} passed, ${steps.filter((s) => s.skipped).length} skipped, ${failed.length} failed`)
if (failed.length || fail.length) {
  for (const p of procs) console.log(`\n── ${p.name}.log (tail)\n${tail(join(LOGS, p.name + '.log'))}`)
  console.log(`\nwork dir kept: ${WORK}`)
  process.exit(1)
}
if (KEEP) console.log(`work dir kept: ${WORK}`)
else rmSync(WORK, { recursive: true, force: true })
