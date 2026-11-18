// Object storage outage drill (docs/design.md §17.5, docs/runbooks/object-storage-outage.md):
// the store answers 503 (or hangs) for N seconds while clients keep sending.
// Ingestion must keep acknowledging, and afterwards every acknowledged event
// must be visible in the query service exactly once.
//
//   node e2e/outage.mjs [--seconds 600] [--mode fail|pause] [--rate 50] [--keep]
//
// Needs the same binaries as e2e/run.mjs. Uses the repository's devs3 server
// and its fault switch; this is a drill of our own retry and buffering logic,
// not of a real provider's failure modes.
import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { createServer as netServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const argv = process.argv.slice(2)
const opt = (name, def) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : def)
const SECONDS = Number(opt('--seconds', 600))
const MODE = opt('--mode', 'fail')
const RATE = Number(opt('--rate', 50)) // events per second, in batches of 5
const KEEP = argv.includes('--keep')

const firstExisting = (...c) => c.find((p) => p && existsSync(p))
const SHIPPER = firstExisting(process.env.LA_E2E_SHIPPER, join(ROOT, 'engine/target/release/la-shipper'), join(ROOT, 'engine/target/debug/la-shipper'))
const QUERY = firstExisting(process.env.LA_E2E_QUERY, join(ROOT, 'engine/target/release/la-query'), join(ROOT, 'engine/target/debug/la-query'), join(ROOT, 'engine/target-query/debug/la-query'))
if (!SHIPPER || !QUERY) {
  console.error('la-shipper / la-query not found. Build them first: (cd engine && cargo build -p la-shipper -p la-query)')
  process.exit(2)
}

const WORK = mkdtempSync(join(tmpdir(), 'izlek-outage-'))
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
async function waitFor(what, cond, ms = 60_000, every = 500) {
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
const metric = async (url, name) => {
  const text = await (await fetch(url + '/metrics')).text()
  const line = text.split('\n').find((l) => l.startsWith(name + ' ') || l.startsWith(name + '{'))
  return line ? Number(line.trim().split(' ').pop()) : NaN
}

let ok = false
try {
  const build = spawnSync('go', ['build', '-o', BIN + '/', './ingest/cmd/la-ingest', './control/cmd/la-control', './golib/devs3/cmd/devs3'], { cwd: ROOT, env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'inherit' })
  assert.equal(build.status, 0)

  const [pS3, pIngest, pQuery] = [await freePort(), await freePort(), await freePort()]
  const S3 = `http://127.0.0.1:${pS3}`
  const INGEST = `http://127.0.0.1:${pIngest}`
  const QUERY_URL = `http://127.0.0.1:${pQuery}`
  const storeEnv = { LA_STORE_URL: 's3://drill', LA_S3_ENDPOINT: S3, LA_S3_ACCESS_KEY: 'x', LA_S3_SECRET_KEY: 'x', LA_S3_REGION: 'us-east-1' }
  start('devs3', join(BIN, 'devs3'), ['-listen', `127.0.0.1:${pS3}`], {})
  await waitFor('devs3', async () => (await fetch(S3 + '/_devs3/fault')).ok, 10_000)

  const b = spawnSync(join(BIN, 'la-control'), ['bootstrap', '--email', 'drill@example.com', '--password', 'drill-password-1', '--org', 'Drill', '--project', 'Drill', '--json'], { env: { ...process.env, ...storeEnv, LA_CONTROL_DB: join(WORK, 'control.db') }, encoding: 'utf8' })
  assert.equal(b.status, 0, b.stderr)
  const boot = JSON.parse(b.stdout.trim().split('\n').pop())

  const ingest = start('ingest', join(BIN, 'la-ingest'), [], { ...storeEnv, LA_LISTEN: `127.0.0.1:${pIngest}`, LA_NODE_ID: 'drill-01', LA_WAL_DIR: join(WORK, 'wal'), LA_WAL_SEGMENT_SECONDS: '2', LA_RATE_IP: '0', LA_DRAIN_SECONDS: '0' })
  const shipper = start('shipper', SHIPPER, ['ship'], { ...storeEnv, LA_METRICS_ADDR: 'off', LA_SHIPPER_POLL_SECS: '1', LA_SHIPPER_BATCH_SECS: '0' })
  const query = start('query', QUERY, [], { ...storeEnv, LA_QUERY_LISTEN: `127.0.0.1:${pQuery}`, LA_QUERY_MANIFEST_TTL_MS: '500' })
  await waitFor('ingest', async () => (await fetch(INGEST + '/readyz')).status == 200, 30_000)
  await waitFor('query', async () => (await fetch(QUERY_URL + '/readyz')).status == 200, 30_000)

  const day = (o) => new Date(Date.now() + o * 86_400_000).toISOString().slice(0, 10)
  const sql = async (text) => {
    const res = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { Authorization: `Bearer ${boot.secret}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ from: day(-1), to: day(1), sql: text }), signal: AbortSignal.timeout(20_000) })
    if (res.status != 200) throw new Error(`query ${res.status}`)
    return (await res.json()).rows
  }
  const visible = async () => (await sql("SELECT count(*), count(DISTINCT prop_num(props, 'seq')) FROM events WHERE event = 'drill'"))[0]

  // ── sender: batches of 5, each event with a deterministic uuid and seq ──
  let seq = 0
  let acked = 0
  let sending = true
  const refused = {}
  const latencies = []
  const uuid = (n) => `01930000-0000-7000-8000-${n.toString(16).padStart(12, '0')}`
  const sender = (async () => {
    const gap = 5000 / RATE
    while (sending) {
      const batch = Array.from({ length: 5 }, () => ({ event: 'drill', distinct_id: 'drill-' + (seq % 50), uuid: uuid(seq), properties: { seq: seq++ }, timestamp: new Date().toISOString() }))
      const t0 = performance.now()
      for (;;) {
        // A client retries the same batch until it is acknowledged.
        try {
          const res = await fetch(`${INGEST}/batch/`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ api_key: boot.token, batch }), signal: AbortSignal.timeout(10_000) })
          await res.arrayBuffer()
          if (res.status == 200) break
          refused[res.status] = (refused[res.status] || 0) + 1
        } catch (e) {
          refused.network = (refused.network || 0) + 1
        }
        await sleep(500)
      }
      latencies.push(performance.now() - t0)
      acked += 5
      await sleep(gap)
    }
  })()

  const fault = async (mode) => assert.ok((await fetch(`${S3}/_devs3/fault?mode=${mode}`, { method: 'POST' })).ok)
  const pct = (a, p) => [...a].sort((x, y) => x - y)[Math.min(a.length - 1, Math.floor(a.length * p))]

  console.log(`warm-up 10 s at ${RATE} events/s …`)
  await sleep(10_000)
  await waitFor('warm-up events visible', async () => (await visible())[0] > 0, 60_000)
  const warmP99 = pct(latencies, 0.99)

  console.log(`store outage (${MODE}) for ${SECONDS} s …`)
  await fault(MODE)
  const tOut = Date.now()
  const ackedAtStart = acked
  latencies.length = 0
  const refusedBefore = JSON.stringify(refused)
  let peakLocal = 0
  let readyFailures = 0
  let queryDuring = {}
  while (Date.now() - tOut < SECONDS * 1000) {
    await sleep(Math.min(5000, SECONDS * 1000))
    peakLocal = Math.max(peakLocal, await metric(INGEST, 'la_ingest_wal_local_bytes').catch(() => 0))
    if ((await fetch(INGEST + '/readyz')).status != 200) readyFailures++
    const qs = await fetch(`${QUERY_URL}/q/sql`, { method: 'POST', headers: { Authorization: `Bearer ${boot.secret}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ from: day(-1), to: day(1), sql: 'SELECT count(*) FROM events' }), signal: AbortSignal.timeout(30_000) }).then((r) => r.status, () => 'timeout')
    queryDuring[qs] = (queryDuring[qs] || 0) + 1
  }
  const ackedDuring = acked - ackedAtStart
  const outP99 = pct(latencies, 0.99)
  for (const p of [ingest, shipper, query]) assert.equal(p.code, undefined, `${p.name} died during the outage`)
  assert.ok(ackedDuring >= RATE * SECONDS * 0.5, `ingestion kept acknowledging during the outage (${ackedDuring} events)`)
  assert.equal(JSON.stringify(refused), refusedBefore, 'no request was refused during the outage')
  assert.equal(readyFailures, 0, '/readyz stayed green (local WAL below its limit)')

  console.log('store back; waiting for catch-up …')
  await fault('none')
  const tBack = Date.now()
  await sleep(5000)
  sending = false
  await sender
  const [total, distinct] = await waitFor('every acknowledged event visible', async () => {
    const [c, d] = await visible()
    return d >= acked ? [c, d] : null
  }, 600_000, 1000)
  const catchUp = (Date.now() - tBack) / 1000
  assert.equal(distinct, acked, 'no acknowledged event is missing')
  assert.equal(total, acked, 'no event is counted twice')
  for (const p of [ingest, shipper, query]) assert.equal(p.code, undefined, `${p.name} died`)

  ingest.child.kill('SIGTERM')
  await waitFor('ingest exit', () => ingest.code !== undefined, 90_000)
  assert.equal(ingest.code, 0, 'ingestion drained cleanly')

  console.log(
    JSON.stringify(
      {
        outage_seconds: SECONDS,
        mode: MODE,
        events_acknowledged_total: acked,
        events_acknowledged_during_outage: ackedDuring,
        ack_p99_ms_before: Math.round(warmP99 * 10) / 10,
        ack_p99_ms_during: Math.round(outP99 * 10) / 10,
        peak_local_wal_bytes: peakLocal,
        query_status_during_outage: queryDuring,
        catch_up_seconds: Math.round(catchUp * 10) / 10,
        visible: total,
        distinct,
      },
      null,
      2,
    ),
  )
  console.log('outage drill passed')
  ok = true
} catch (e) {
  console.error('outage drill FAILED:', e?.stack || e)
  for (const p of procs) {
    try {
      console.error(`── ${p.name}.log (tail)\n` + readFileSync(join(LOGS, p.name + '.log'), 'utf8').trimEnd().split('\n').slice(-15).join('\n'))
    } catch {}
  }
} finally {
  for (const p of [...procs].reverse()) if (p.code === undefined) p.child.kill('SIGTERM')
  await sleep(1500)
  for (const p of procs) if (p.code === undefined) p.child.kill('SIGKILL')
  if (ok && !KEEP) rmSync(WORK, { recursive: true, force: true })
  else console.log(`work dir kept: ${WORK}`)
}
process.exit(ok ? 0 : 1)
