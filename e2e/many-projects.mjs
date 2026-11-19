// Many small projects (docs/design.md §19 assumption 11): what do N projects
// with a handful of events each cost in objects, requests and time?
//
//   node e2e/many-projects.mjs [--projects 10000] [--events 5] [--keep]
//
// Every project gets one PostHog-format batch per round; two rounds, so that
// each project has two L0 files and compaction has something to do. The store
// is the repository's devs3 server, which counts requests per kind.
// LA_E2E_SHIPPER / LA_E2E_QUERY select the binaries (a release build of the
// shipper is preferred when present).
import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { createServer as netServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createHash } from 'node:crypto'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const argv = process.argv.slice(2)
const opt = (name, def) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : def)
const N = Number(opt('--projects', 10000))
const K = Number(opt('--events', 5))
const KEEP = argv.includes('--keep')

const firstExisting = (...c) => c.find((p) => p && existsSync(p))
const SHIPPER = firstExisting(process.env.LA_E2E_SHIPPER, join(ROOT, 'engine/target/release/la-shipper'), join(ROOT, 'engine/target/debug/la-shipper'))
const QUERY = firstExisting(process.env.LA_E2E_QUERY, join(ROOT, 'engine/target/release/la-query'), join(ROOT, 'engine/target/debug/la-query'), join(ROOT, 'engine/target-query/debug/la-query'))
assert.ok(SHIPPER && QUERY, 'build la-shipper and la-query first')

const WORK = mkdtempSync(join(tmpdir(), 'izlek-many-'))
const BIN = join(WORK, 'bin')
const LOGS = join(WORK, 'logs')
for (const d of [BIN, LOGS]) mkdirSync(d, { recursive: true })
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
async function waitFor(what, cond, ms = 60_000, every = 250) {
  const t0 = Date.now()
  for (;;) {
    try {
      const v = await cond()
      if (v) return v
    } catch {}
    if (Date.now() - t0 > ms) throw new Error(`timed out waiting for ${what}`)
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
// Runs a command to completion; returns wall seconds and peak RSS (VmHWM polled from /proc).
async function timed(name, cmd, args, env) {
  const t0 = Date.now()
  const child = spawn(cmd, args, { env: { ...process.env, ...env }, stdio: ['ignore', 'ignore', 'pipe'] })
  let err = ''
  child.stderr.on('data', (d) => (err = (err + d).slice(-4000)))
  let peakKb = 0
  const poll = setInterval(() => {
    try {
      const m = /VmHWM:\s+(\d+) kB/.exec(readFileSync(`/proc/${child.pid}/status`, 'utf8'))
      if (m) peakKb = Math.max(peakKb, Number(m[1]))
    } catch {}
  }, 50)
  const code = await new Promise((r) => child.once('exit', (c, sig) => r(c ?? sig)))
  clearInterval(poll)
  if (code !== 0) throw new Error(`${name} exited ${code}\n${err}`)
  return { seconds: Math.round((Date.now() - t0) / 100) / 10, peak_rss_mb: peakKb ? Math.round(peakKb / 1024) : null }
}

let ok = false
try {
  const build = spawnSync('go', ['build', '-o', BIN + '/', './ingest/cmd/la-ingest', './golib/devs3/cmd/devs3'], { cwd: ROOT, env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'inherit' })
  assert.equal(build.status, 0)

  const [pS3, pIngest, pQuery] = [await freePort(), await freePort(), await freePort()]
  const S3 = `http://127.0.0.1:${pS3}`
  const INGEST = `http://127.0.0.1:${pIngest}`
  const QUERY_URL = `http://127.0.0.1:${pQuery}`
  const storeEnv = { LA_STORE_URL: 's3://many', LA_S3_ENDPOINT: S3, LA_S3_ACCESS_KEY: 'x', LA_S3_SECRET_KEY: 'x', LA_S3_REGION: 'us-east-1' }
  const engineEnv = { ...storeEnv, LA_METRICS_ADDR: 'off' }
  start('devs3', join(BIN, 'devs3'), ['-listen', `127.0.0.1:${pS3}`], {})
  await waitFor('devs3', async () => (await fetch(S3 + '/_devs3/stats')).ok, 10_000)
  const stats = async () => (await fetch(S3 + '/_devs3/stats')).json()
  const delta = (a, b) => Object.fromEntries(Object.keys(b).map((k) => [k, b[k] - (a[k] || 0)]).filter(([k, v]) => v !== 0 || k == 'objects'))
  const put = async (key, body) => assert.ok((await fetch(`${S3}/many/${key}`, { method: 'PUT', body: JSON.stringify(body) })).ok, key)

  // Control snapshots written by hand: N projects, one token each, one query key for a few of them.
  const token = (i) => `pk_many_${String(i).padStart(6, '0')}`
  const SECRET = 'sk_' + 'm'.repeat(32)
  const sampled = [1, Math.ceil(N / 2), N]
  await put('control/tokens.json', { version: 1, tokens: Object.fromEntries(Array.from({ length: N }, (_, i) => [token(i + 1), { project_id: i + 1, status: 'active', rate_limit: 0, allowed_origins: [], replay: false }])) })
  await put('control/projects.json', { version: 1, projects: Object.fromEntries(Array.from({ length: N }, (_, i) => [String(i + 1), { name: `p${i + 1}`, retention_days: 365, replay_retention_days: 30, drop_keys: [], scan_limit_bytes: 0, max_concurrent_queries: 4 }])) })
  const keyOf = (pid) => SECRET.slice(0, -6) + String(pid).padStart(6, '0')
  await put('control/keys.json', { version: 1, keys: Object.fromEntries(sampled.map((pid) => [createHash('sha256').update(keyOf(pid)).digest('hex'), { project_id: pid, scopes: ['query'] }])) })

  const report = { projects: N, events_per_project_per_round: K, shipper_binary: SHIPPER.includes('/release/') ? 'release' : 'debug' }

  async function round(n) {
    const ingest = start(`ingest${n}`, join(BIN, 'la-ingest'), [], { ...storeEnv, LA_LISTEN: `127.0.0.1:${pIngest}`, LA_NODE_ID: 'many-01', LA_WAL_DIR: join(WORK, 'wal'), LA_WAL_SEGMENT_SECONDS: '2', LA_RATE_IP: '0', LA_DRAIN_SECONDS: '0', LA_MAX_INFLIGHT: '4096' })
    await waitFor('ingest', async () => (await fetch(INGEST + '/readyz')).status == 200, 60_000)
    const t0 = Date.now()
    let next = 1
    const worker = async () => {
      for (;;) {
        const pid = next++
        if (pid > N) return
        const batch = Array.from({ length: K }, (_, j) => ({ event: j == 0 ? 'signup' : 'view', distinct_id: `user-${pid}-${j % 2}`, uuid: `0193${String(n).padStart(4, '0')}-0000-7000-8000-${(pid * 100 + j).toString(16).padStart(12, '0')}`, properties: { n, j, plan: 'free' } }))
        const res = await fetch(`${INGEST}/batch/`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ api_key: token(pid), batch }) })
        await res.arrayBuffer()
        assert.equal(res.status, 200, `project ${pid}`)
      }
    }
    await Promise.all(Array.from({ length: 64 }, worker))
    const sendSeconds = (Date.now() - t0) / 1000
    ingest.child.kill('SIGTERM')
    await waitFor('ingest exit', () => ingest.code !== undefined, 120_000)
    assert.equal(ingest.code, 0)
    return Math.round(sendSeconds * 10) / 10
  }

  console.log(`round 1: ${N} projects × ${K} events …`)
  report.send_seconds_round1 = await round(1)
  let before = await stats()
  report.ship_round1 = await timed('ship', SHIPPER, ['ship', '--once'], engineEnv)
  let after = await stats()
  report.ship_round1.requests = delta(before, after)
  report.objects_after_round1 = after.objects
  report.bytes_after_round1 = after.bytes

  console.log('round 2 …')
  report.send_seconds_round2 = await round(2)
  before = await stats()
  report.ship_round2 = await timed('ship', SHIPPER, ['ship', '--once'], engineEnv)
  after = await stats()
  report.ship_round2.requests = delta(before, after)
  report.objects_after_round2 = after.objects
  report.objects_per_project_after_round2 = Math.round((after.objects / N) * 10) / 10

  // An idle shipper pass: nothing new in the WAL.
  before = await stats()
  report.ship_idle = await timed('ship idle', SHIPPER, ['ship', '--once'], engineEnv)
  report.ship_idle.requests = delta(before, await stats())

  // A compaction cycle that has work in every project, then an idle one.
  before = await stats()
  report.compact_full = await timed('compact', SHIPPER, ['compact', '--once', '--no-lease'], { ...engineEnv, LA_COMPACTOR_L0_FILES: '1' })
  after = await stats()
  report.compact_full.requests = delta(before, after)
  before = after
  report.compact_idle = await timed('compact idle', SHIPPER, ['compact', '--once', '--no-lease'], { ...engineEnv, LA_COMPACTOR_L0_FILES: '1' })
  after = await stats()
  report.compact_idle.requests = delta(before, after)
  report.objects_after_compaction = after.objects
  report.bytes_after_compaction = after.bytes

  // The compactor as a service: the first cycle is a full scan, the following ones look at hot days only.
  const settle = async () => {
    let last = -1
    for (let quiet = 0; quiet < 6; ) {
      await sleep(500)
      const now = (await stats()).LIST
      quiet = now == last ? quiet + 1 : 0
      last = now
    }
    return stats()
  }
  const loop = start('compactor', SHIPPER, ['compact', '--no-lease'], { ...engineEnv, LA_COMPACTOR_L0_FILES: '1', LA_COMPACTOR_INTERVAL_SECS: '30' })
  await waitFor('first cycle to start', async () => (await stats()).LIST > after.LIST, 60_000)
  before = await settle()
  await waitFor('second cycle to start', async () => (await stats()).LIST > before.LIST, 120_000)
  const tHot = Date.now()
  after = await settle()
  report.compact_hot_cycle = { seconds_upper_bound: Math.round((Date.now() - tHot - 3000) / 100) / 10, requests: delta(before, after) }
  loop.child.kill('SIGTERM')
  await waitFor('compactor exit', () => loop.code !== undefined, 60_000)

  // Correctness on a sample, and what a query on one small project costs.
  const query = start('query', QUERY, [], { ...storeEnv, LA_QUERY_LISTEN: `127.0.0.1:${pQuery}` })
  await waitFor('query', async () => (await fetch(QUERY_URL + '/readyz')).status == 200, 60_000)
  const day = (o) => new Date(Date.now() + o * 86_400_000).toISOString().slice(0, 10)
  const times = []
  for (const pid of sampled) {
    before = await stats()
    const t0 = performance.now()
    const res = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { Authorization: `Bearer ${keyOf(pid)}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ from: day(-1), to: day(1), sql: 'SELECT count(*), count(DISTINCT person_key) FROM events' }) })
    const body = await res.json()
    times.push(Math.round(performance.now() - t0))
    assert.equal(res.status, 200, JSON.stringify(body))
    assert.deepEqual(body.rows, [[2 * K, 2]], `project ${pid}: every event exactly once, isolated from the others`)
    report.query_requests_one_project = delta(before, await stats())
  }
  report.query_ms_first_per_sampled_project = times
  assert.equal(query.code, undefined)

  console.log(JSON.stringify(report, null, 2))
  ok = true
} catch (e) {
  console.error('FAILED:', e?.stack || e)
} finally {
  for (const p of [...procs].reverse()) if (p.code === undefined) p.child.kill('SIGTERM')
  await sleep(1000)
  for (const p of procs) if (p.code === undefined) p.child.kill('SIGKILL')
  if (ok && !KEEP) rmSync(WORK, { recursive: true, force: true })
  else console.log(`work dir kept: ${WORK}`)
}
process.exit(ok ? 0 : 1)
