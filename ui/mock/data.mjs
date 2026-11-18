// Deterministic demo dataset and an in-memory implementation of the query API
// (spec/query.md) over it. Small on purpose: correctness of shapes and of the
// documented semantics matters here, not speed.
import { gzipSync, zstdCompressSync } from 'node:zlib'
import { createHash } from 'node:crypto'
import { match, str, toNum } from './expr.mjs'
import { demoRecording, encodeChunk } from './lar1.mjs'

const DAY = 86_400_000
const HISTORY_DAYS = 60

const mulberry = (seed) => {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}
const pick = (rnd, weighted) => {
  const total = weighted.reduce((a, [, w]) => a + w, 0)
  let x = rnd() * total
  for (const [v, w] of weighted) if ((x -= w) < 0) return v
  return weighted[weighted.length - 1][0]
}
const hex = (rnd, n) => Array.from({ length: n }, () => Math.floor(rnd() * 16).toString(16)).join('')

const NAMES = ['Ayşe', 'Mehmet', 'Elif', 'Can', 'Zeynep', 'Emre', 'Deniz', 'Selin', 'Burak', 'Ece', 'Kerem', 'Defne', 'Onur', 'İpek', 'Mert', 'Naz']
const CITIES = ['İstanbul', 'Ankara', 'İzmir', 'Bursa', 'Antalya', 'Berlin', 'Londra', 'Amsterdam']
const CATEGORIES = [['mutfak', 5], ['tekstil', 3], ['dekorasyon', 2], ['bahçe', 1]]
const PAGES = ['/', '/urunler', '/urunler/mutfak', '/kampanyalar', '/sepet', '/hesabim']
const SCREENS = ['Ana sayfa', 'Ürünler', 'Kampanyalar', 'Sepet', 'Profil']

export const REPLAY_SESSION = '0193f1a2b3c47d5e8a9b0c1d2e3f4a5c'
export const REPLAY_SESSION_ZSTD = '0193f1a2b3c47d5e8a9b0c1d2e3f4a5d'

/** `ts` in the first 12 hex digits (UUIDv7 layout), the rest pseudo-random. */
const uuid = (ts, rnd) => {
  const head = ts.toString(16).padStart(12, '0')
  return head + '7' + hex(rnd, 3) + (8 + Math.floor(rnd() * 4)).toString(16) + hex(rnd, 15)
}

/**
 * Builds the dataset for the 60 days up to `now`. Per-person, per-day
 * randomness is seeded by (person, calendar day), so a given day looks the
 * same no matter when the server was started.
 */
export const buildDataset = (now) => {
  const today = Math.floor(now / DAY)
  const persons = []
  const events = []

  for (let p = 0; p < 90; p++) {
    const rnd = mulberry(1000 + p)
    const platform = pick(rnd, [['web', 5], ['ios', 3], ['android', 2]])
    const identified = p < 2 || rnd() < 0.72
    const userId = identified ? String(1001 + p) : null
    const anonId = hex(rnd, 8) + '-' + hex(rnd, 4) + '-4' + hex(rnd, 3) + '-a' + hex(rnd, 3) + '-' + hex(rnd, 12)
    const person = {
      person_key: userId ? 'u:' + userId : 'a:' + anonId,
      user_id: userId,
      anon_id: anonId,
      platform,
      os: platform == 'web' ? pick(rnd, [['Windows', 5], ['macOS', 3], ['Linux', 1]]) : platform == 'ios' ? 'iOS' : 'Android',
      os_version: platform == 'ios' ? pick(rnd, [['18.1', 3], ['17.6', 2]]) : platform == 'android' ? pick(rnd, [['15', 3], ['14', 2]]) : '',
      device_model: platform == 'ios' ? pick(rnd, [['iPhone16,1', 2], ['iPhone15,2', 3]]) : platform == 'android' ? pick(rnd, [['Pixel 9', 2], ['SM-S921B', 3]]) : '',
      device_type: platform == 'web' ? pick(rnd, [['desktop', 4], ['mobile', 2]]) : 'phone',
      app_version: platform == 'web' ? '' : pick(rnd, [['3.2.0', 5], ['3.1.4', 3], ['3.0.9', 1]]),
      locale: pick(rnd, [['tr-TR', 7], ['en-GB', 2], ['de-DE', 1]]),
      country: pick(rnd, [['TR', 11], ['DE', 3], ['US', 2], ['GB', 2], ['NL', 1], ['FR', 1]]),
      sdk: platform == 'web' ? 'web/0.1.0' : 'kmp/0.1.0',
      signupAge: p < 2 ? 21 + p * 9 : Math.floor(rnd() * 44),
      keenness: 0.35 + rnd() * 0.5,
      props: {},
    }
    if (identified) {
      person.props = {
        name: p == 0 ? 'Ayşe' : NAMES[Math.floor(rnd() * NAMES.length)],
        plan: pick(rnd, [['free', 7], ['pro', 3]]),
        city: CITIES[Math.floor(rnd() * CITIES.length)],
        signup_method: pick(rnd, [['email', 5], ['google', 3], ['apple', 2]]),
      }
    }
    persons.push(person)

    for (let age = person.signupAge; age >= 0; age--) {
      const day = today - age
      const r = mulberry(p * 100003 + day)
      const first = age == person.signupAge
      const sinceSignup = person.signupAge - age
      const active = first || r() < person.keenness * (0.14 + 0.86 * Math.exp(-sinceSignup / 9))
      if (!active) continue
      const sessionId = hex(r, 32)
      let ts = day * DAY + Math.floor((7 + r() * 15) * 3_600_000)
      const add = (event, props = {}, extra = {}) => {
        ts += 1500 + Math.floor(r() * 40_000)
        events.push({
          uuid: uuid(ts, r),
          ts,
          recv_ts: ts + 80 + Math.floor(r() * 900),
          event,
          person_key: person.person_key,
          anon_id: person.anon_id,
          user_id: person.user_id,
          session_id: sessionId,
          platform: person.platform,
          os: person.os,
          os_version: person.os_version,
          device_model: person.device_model,
          device_type: person.device_type,
          app_version: person.app_version,
          locale: person.locale,
          country: person.country,
          sdk: person.sdk,
          url_path: null,
          referrer_host: null,
          screen: null,
          flags: 0,
          props,
          ...extra,
        })
      }
      const view = () =>
        person.platform == 'web'
          ? add('pageview', { title: 'Mağaza' }, { url_path: PAGES[Math.floor(r() * PAGES.length)], referrer_host: r() < 0.3 ? 'google.com' : null })
          : add('screen', {}, { screen: SCREENS[Math.floor(r() * SCREENS.length)] })
      if (person.platform != 'web') add('app_opened')
      view()
      if (first && person.user_id) add('signup', { method: person.props.signup_method })
      const views = Math.floor(r() * 3)
      for (let i = 0; i < views; i++) view()
      if (r() < 0.4) add('search', { query_length: 3 + Math.floor(r() * 12), results: Math.floor(r() * 40) })
      if (r() < 0.62) {
        const price = Math.round((120 + r() * 1400) * 100) / 100
        const item = { product_id: 'p-' + (100 + Math.floor(r() * 40)), category: pick(r, CATEGORIES), price }
        add('product_viewed', item)
        if (r() < 0.5) {
          add('add_to_cart', item)
          if (r() < 0.62) {
            add('checkout_started', { items: 1 })
            if (r() < 0.7) add('purchase', { amount: price, currency: 'TRY', items: 1, coupon: r() < 0.2 ? 'HOSGELDIN' : null })
          }
        }
      }
    }
  }

  // Scripted replay sessions for the first two persons, a couple of hours ago.
  const chunkBodies = new Map() // key suffix → { body, encoding }
  const replay = (person, sessionId, startTs, zstd) => {
    const rec = demoRecording(sessionId, startTs)
    const r = mulberry(startTs % 100000)
    const base = { ...events.find((e) => e.person_key == person.person_key), session_id: sessionId, url_path: null, referrer_host: null, props: {} }
    for (const [offset, event, screen, props] of rec.moments) {
      const ts = startTs + offset
      events.push({ ...base, uuid: uuid(ts, r), ts, recv_ts: ts + 300, event, screen, props })
    }
    for (const chunk of rec.chunks) {
      const raw = encodeChunk(chunk)
      const body = zstd ? zstdCompressSync(raw) : gzipSync(raw)
      const suffix = `d=${new Date(chunk.start_ts).toISOString().slice(0, 10)}/${sessionId}/${String(chunk.seq).padStart(6, '0')}.bin.${zstd ? 'zst' : 'gz'}`
      chunkBodies.set(suffix, { body, encoding: zstd ? 'zstd' : 'gzip', seq: chunk.seq })
      events.push({
        ...base,
        uuid: createHash('sha256').update('la-replay' + sessionId + chunk.seq).digest('hex').slice(0, 32),
        ts: chunk.start_ts,
        recv_ts: chunk.start_ts + chunk.duration_ms + 400,
        event: 'session_replay_chunk',
        screen: null,
        // `key` is completed with the project prefix at query time
        props: { seq: chunk.seq, bytes: body.length, duration_ms: chunk.duration_ms, key: suffix, encoding: zstd ? 1 : 2 },
      })
    }
  }
  const twoHoursAgo = Math.floor((now - 2 * 3_600_000) / 60_000) * 60_000
  replay(persons[0], REPLAY_SESSION, twoHoursAgo, false)
  if (typeof zstdCompressSync == 'function') replay(persons[1], REPLAY_SESSION_ZSTD, twoHoursAgo - 3_600_000, true)

  events.sort((a, b) => a.ts - b.ts || (a.uuid < b.uuid ? -1 : 1))
  return { persons, events, chunkBodies, byKey: new Map(persons.map((p) => [p.person_key, p])) }
}

// ---- query engine ----

export class QueryError extends Error {
  constructor(status, code, message) {
    super(message)
    this.status = status
    this.code = code
  }
}
const bad = (message) => new QueryError(400, 'bad_request', message)

const COLUMNS = new Set([
  'event', 'person_key', 'anon_id', 'user_id', 'session_id', 'platform', 'os', 'os_version', 'device_model',
  'device_type', 'app_version', 'locale', 'country', 'sdk', 'url_path', 'referrer_host', 'screen',
])

const isDay = (s) => typeof s == 'string' && /^\d{4}-\d{2}-\d{2}$/.test(s)

/** → [fromMs, toMs) */
export const parseBounds = (q) => {
  if (typeof q?.from != 'string' || typeof q?.to != 'string') throw bad('"from" ve "to" zorunludur')
  const from = Date.parse(isDay(q.from) ? q.from + 'T00:00:00Z' : q.from)
  const to = isDay(q.to) ? Date.parse(q.to + 'T00:00:00Z') + DAY : Date.parse(q.to)
  if (Number.isNaN(from) || Number.isNaN(to)) throw bad('"from"/"to" YYYY-MM-DD ya da RFC 3339 olmalıdır')
  if (to <= from) throw bad('"to", "from"dan sonra olmalıdır')
  if (to - from > 400 * DAY) throw bad('Tarih aralığı en çok 400 gün olabilir')
  return [from, to]
}

const checkExpr = (e, where = 'filter') => {
  if (e === undefined || e === null) return
  const walk = (x, depth) => {
    if (!x || typeof x != 'object' || Array.isArray(x) || depth > 32) throw bad(`${where}: geçersiz ifade`)
    if ('and' in x || 'or' in x) {
      const list = x.and ?? x.or
      if (!Array.isArray(list)) throw bad(`${where}: "and"/"or" bir dizi olmalıdır`)
      return list.forEach((c) => walk(c, depth + 1))
    }
    if ('not' in x) return walk(x.not, depth + 1)
    if (typeof x.prop != 'string') throw bad(`${where}: "prop" eksik`)
    if (!COLUMNS.has(x.prop) && !x.prop.startsWith('props.') && !x.prop.startsWith('person.')) throw bad(`${where}: bilinmeyen sütun "${x.prop}"`)
    if (!['eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'contains', 'is_set'].includes(x.op)) throw bad(`${where}: bilinmeyen operatör "${x.op}"`)
    if ((x.op == 'in' || x.op == 'not_in') && !Array.isArray(x.value)) throw bad(`${where}: "${x.op}" bir dizi ister`)
  }
  walk(e, 0)
}

const checkRef = (ref, where) => {
  if (typeof ref != 'string' || !(COLUMNS.has(ref) || ref.startsWith('props.') || ref.startsWith('person.'))) throw bad(`${where}: bilinmeyen sütun "${ref}"`)
}

export const createEngine = (dataset, nowFn) => {
  const getter = (ev) => (ref) => {
    if (ref.startsWith('props.')) return ev.props[ref.slice(6)] ?? ev[ref.slice(6)] ?? undefined
    if (ref.startsWith('person.')) return dataset.byKey.get(ev.person_key)?.props[ref.slice(7)]
    return ev[ref]
  }
  const passes = (ev, expr) => !expr || match(expr, getter(ev))
  const stepMatch = (ev, step) => (step.event == null || ev.event == step.event) && passes(ev, step.filter)

  /** Events inside the bounds that pass the global filter and are already "in a manifest". */
  const select = (q) => {
    checkExpr(q.filter)
    const [from, to] = parseBounds(q)
    const visible = nowFn()
    return dataset.events.filter((e) => e.ts >= from && e.ts < to && e.ts <= visible && passes(e, q.filter))
  }

  const floor = (ms, unit) => {
    const d = new Date(ms)
    if (unit == 'hour') return Math.floor(ms / 3_600_000) * 3_600_000
    if (unit == 'day') return Math.floor(ms / DAY) * DAY
    if (unit == 'week') {
      const day = Math.floor(ms / DAY) * DAY
      return day - ((new Date(day).getUTCDay() + 6) % 7) * DAY // Monday start
    }
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1)
  }
  const next = (ms, unit) => {
    if (unit == 'hour') return ms + 3_600_000
    if (unit == 'day') return ms + DAY
    if (unit == 'week') return ms + 7 * DAY
    const d = new Date(ms)
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1)
  }
  const iso = (ms) => new Date(ms).toISOString().replace('.000Z', 'Z')

  const trends = (q) => {
    if (!['hour', 'day', 'week', 'month'].includes(q.interval)) throw bad('"interval": hour | day | week | month')
    if (!Array.isArray(q.series) || !q.series.length) throw bad('"series" en az bir seri içermelidir')
    q.series.forEach((s, i) => {
      if (!['count', 'unique', 'sum', 'avg', 'min', 'max'].includes(s.agg)) throw bad(`series[${i}].agg geçersiz`)
      if (['sum', 'avg', 'min', 'max'].includes(s.agg)) checkRef(s.prop, `series[${i}].prop`)
      checkExpr(s.filter, `series[${i}].filter`)
    })
    if (q.breakdown != null) checkRef(q.breakdown, 'breakdown')
    const [from, to] = parseBounds(q)
    const starts = []
    for (let b = floor(from, q.interval); b < to; b = next(b, q.interval)) starts.push(b)
    if (starts.length > 2000) throw bad('Bu aralık için kova sayısı çok fazla; daha büyük bir "interval" seçin')
    const index = new Map(starts.map((b, i) => [b, i]))
    const rows = select(q)
    const limit = q.breakdown_limit ?? 25
    const out = []
    q.series.forEach((s, si) => {
      const groups = new Map() // breakdown value → per-bucket accumulators
      for (const ev of rows) {
        if (!stepMatch(ev, s)) continue
        const get = getter(ev)
        let key = '\u0000none'
        if (q.breakdown != null) {
          const v = get(q.breakdown)
          key = v === undefined || v === null ? null : str(v)
        }
        let acc = groups.get(key)
        if (!acc) groups.set(key, (acc = starts.map(() => ({ n: 0, sum: 0, min: Infinity, max: -Infinity, set: new Set() }))))
        const a = acc[index.get(floor(ev.ts, q.interval))]
        if (s.agg == 'count') a.n++
        else if (s.agg == 'unique') a.set.add(ev.person_key)
        else {
          const x = toNum(get(s.prop))
          if (x === null) continue
          a.n++
          a.sum += x
          a.min = Math.min(a.min, x)
          a.max = Math.max(a.max, x)
        }
      }
      const value = (a) =>
        s.agg == 'count' ? a.n : s.agg == 'unique' ? a.set.size : s.agg == 'sum' ? Math.round(a.sum * 100) / 100
        : !a.n ? null : s.agg == 'avg' ? Math.round((a.sum / a.n) * 100) / 100 : s.agg == 'min' ? a.min : a.max
      let series = [...groups.entries()].map(([key, acc]) => ({ key, values: acc.map(value) }))
      const total = (x) => x.values.reduce((a, v) => a + (v ?? 0), 0)
      series.sort((a, b) => total(b) - total(a))
      if (q.breakdown != null && series.length > limit) {
        const rest = series.slice(limit)
        series = series.slice(0, limit)
        if (s.agg == 'count' || s.agg == 'sum') {
          series.push({ key: '$other', values: starts.map((_, i) => Math.round(rest.reduce((a, x) => a + (x.values[i] ?? 0), 0) * 100) / 100) })
        }
      }
      if (!series.length && q.breakdown == null) {
        series = [{ key: '\u0000none', values: starts.map(() => (['avg', 'min', 'max'].includes(s.agg) ? null : 0)) }]
      }
      const label = s.label ?? (s.event ?? '*') + (s.agg == 'count' ? '' : ` · ${s.agg}` + (s.prop ? `(${s.prop})` : ''))
      for (const x of series) {
        const entry = { index: si, label, values: x.values }
        if (q.breakdown != null) entry.breakdown = x.key
        out.push(entry)
      }
    })
    return { buckets: starts.map(iso), series: out }
  }

  const median = (xs) => {
    if (!xs.length) return null
    const s = [...xs].sort((a, b) => a - b)
    const m = s.length >> 1
    return Math.round((s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2) / 100) / 10
  }

  /** Per-person funnel depth and step-to-step delays for one definition of "step 1". */
  const funnelRun = (byPerson, steps, order, windowMs, firstStep) => {
    const n = steps.length
    const counts = Array(n).fill(0)
    const delays = steps.map(() => [])
    for (const list of byPerson.values()) {
      let depth = 0
      const gap = Array(n).fill(null)
      if (order == 'any') {
        for (let a = 0; a < list.length; a++) {
          if (!steps.some((s, j) => (j == 0 ? firstStep(list[a]) : stepMatch(list[a], s)))) continue
          const seen = new Set()
          for (let i = a; i < list.length && list[i].ts <= list[a].ts + windowMs; i++) {
            steps.forEach((s, j) => {
              if (j == 0 ? firstStep(list[i]) : stepMatch(list[i], s)) seen.add(j)
            })
          }
          depth = Math.max(depth, seen.size)
        }
      } else if (order == 'strict') {
        for (let a = 0; a < list.length; a++) {
          if (!firstStep(list[a])) continue
          let k = 1
          const g = []
          while (k < n && a + k < list.length && stepMatch(list[a + k], steps[k]) && list[a + k].ts - list[a].ts <= windowMs) {
            g.push(list[a + k].ts - list[a + k - 1].ts)
            k++
          }
          if (k > depth) {
            for (let j = depth; j < k; j++) if (j > 0) gap[j] = g[j - 1]
            depth = k
          }
        }
      } else {
        // start[j] = latest chain start among chains that reached step j+1; prev[j] = ts of that step
        const start = Array(n).fill(null)
        const prev = Array(n).fill(null)
        for (const ev of list) {
          for (let j = n - 1; j >= 1; j--) {
            if (start[j - 1] !== null && ev.ts - start[j - 1] <= windowMs && stepMatch(ev, steps[j])) {
              if (start[j] === null || start[j - 1] > start[j]) {
                if (start[j] === null) gap[j] = ev.ts - prev[j - 1]
                start[j] = start[j - 1]
                prev[j] = ev.ts
              }
            }
          }
          if (firstStep(ev)) {
            start[0] = ev.ts
            prev[0] = ev.ts
          }
        }
        while (depth < n && start[depth] !== null) depth++
      }
      for (let j = 0; j < depth; j++) {
        counts[j]++
        if (j > 0 && gap[j] !== null) delays[j].push(gap[j])
      }
    }
    return steps.map((s, j) => ({ event: s.event ?? null, count: counts[j], median_seconds: j == 0 ? null : median(delays[j]) }))
  }

  const funnel = (q) => {
    if (!Array.isArray(q.steps) || q.steps.length < 2 || q.steps.length > 20) throw bad('Huni 2 ile 20 arasında adım içermelidir')
    q.steps.forEach((s, i) => checkExpr(s.filter, `steps[${i}].filter`))
    const order = q.order ?? 'ordered'
    if (!['ordered', 'strict', 'any'].includes(order)) throw bad('"order": ordered | strict | any')
    const windowMs = (q.window_seconds ?? 604800) * 1000
    if (!(windowMs > 0)) throw bad('"window_seconds" pozitif olmalıdır')
    if (q.breakdown != null) checkRef(q.breakdown, 'breakdown')
    const byPerson = new Map()
    for (const ev of select(q)) {
      const list = byPerson.get(ev.person_key)
      if (list) list.push(ev)
      else byPerson.set(ev.person_key, [ev])
    }
    const first = (ev) => stepMatch(ev, q.steps[0])
    const result = { steps: funnelRun(byPerson, q.steps, order, windowMs, first) }
    if (q.breakdown != null) {
      const bucket = (ev) => {
        const v = getter(ev)(q.breakdown)
        return v === undefined || v === null ? null : str(v)
      }
      const values = new Set()
      for (const list of byPerson.values()) for (const ev of list) if (first(ev)) values.add(bucket(ev))
      result.breakdowns = [...values]
        .map((value) => ({ value, steps: funnelRun(byPerson, q.steps, order, windowMs, (ev) => first(ev) && bucket(ev) === value) }))
        .sort((a, b) => b.steps[0].count - a.steps[0].count)
    }
    return result
  }

  const retention = (q) => {
    if (!['day', 'week', 'month'].includes(q.period)) throw bad('"period": day | week | month')
    const periods = q.periods ?? 8
    if (!(periods >= 1 && periods <= 64)) throw bad('"periods" 1 ile 64 arasında olmalıdır')
    if (!q.start || !q.return) throw bad('"start" ve "return" zorunludur')
    checkExpr(q.start.filter, 'start.filter')
    checkExpr(q.return.filter, 'return.filter')
    const [from, to] = parseBounds(q)
    const cohortStarts = []
    for (let b = floor(from, q.period); b < to; b = next(b, q.period)) cohortStarts.push(b)
    const index = new Map(cohortStarts.map((b, i) => [b, i]))
    const cohorts = cohortStarts.map((b, i) => ({
      period_start: iso(b),
      size: 0,
      returned: Array(Math.min(periods, cohortStarts.length - i)).fill(0),
    }))
    const byPerson = new Map()
    for (const ev of select(q)) {
      const list = byPerson.get(ev.person_key)
      if (list) list.push(ev)
      else byPerson.set(ev.person_key, [ev])
    }
    for (const list of byPerson.values()) {
      const t0 = list.find((ev) => stepMatch(ev, q.start))
      if (!t0) continue
      const c = index.get(floor(t0.ts, q.period))
      cohorts[c].size++
      const seen = new Set()
      for (const ev of list) {
        if (ev.ts < t0.ts || !stepMatch(ev, q.return)) continue
        const k = index.get(floor(ev.ts, q.period)) - c
        if (k >= 0 && k < cohorts[c].returned.length) seen.add(k)
      }
      for (const k of seen) cohorts[c].returned[k]++
    }
    return { cohorts }
  }

  const events = (q, pid) => {
    const limit = q.limit ?? 100
    if (!(limit >= 1 && limit <= 1000)) throw bad('"limit" 1 ile 1000 arasında olmalıdır')
    const order = q.order ?? 'desc'
    if (order != 'asc' && order != 'desc') throw bad('"order": asc | desc')
    let rows = select(q)
    if (order == 'desc') rows = rows.reverse()
    let offset = 0
    if (q.cursor != null) {
      const at = rows.findIndex((e) => e.ts + ':' + e.uuid == Buffer.from(String(q.cursor), 'base64url').toString())
      if (at < 0) throw bad('Geçersiz "cursor"')
      offset = at + 1
    }
    const page = rows.slice(offset, offset + limit)
    const last = page[page.length - 1]
    return {
      events: page.map((e) =>
        e.event == 'session_replay_chunk' ? { ...e, props: { ...e.props, key: `replay/p=${pid}/${e.props.key}` } } : e,
      ),
      next_cursor: last && offset + limit < rows.length ? Buffer.from(last.ts + ':' + last.uuid).toString('base64url') : null,
    }
  }

  /** Not a SQL engine: recognises the handful of query shapes the UI and the demo use. */
  const sql = (q) => {
    if (typeof q.sql != 'string' || !q.sql.trim()) throw bad('"sql" boş olamaz')
    const text = q.sql.trim().replace(/;\s*$/, '')
    const lower = text.toLowerCase().replace(/\s+/g, ' ')
    if (!/^(select|with)\b/.test(lower) || text.includes(';')) throw bad('Yalnızca tek bir SELECT sorgusu çalıştırılabilir')
    if (/\b(insert|update|delete|drop|create|alter|copy|set|attach)\b/.test(lower)) throw bad('DDL/DML ifadelerine izin verilmez')
    if (/information_schema|pg_catalog/.test(lower)) throw bad('Katalog tablolarına erişilemez')
    const limit = Math.min(10000, Number(/\blimit (\d+)/.exec(lower)?.[1] ?? 100))
    const rows = select(q)

    if (/\bfrom persons\b/.test(lower)) {
      const key = /person_key\s*=\s*'((?:[^']|'')*)'/i.exec(text)?.[1]?.replace(/''/g, "'")
      const list = dataset.persons.filter((p) => (key === undefined ? true : p.person_key == key))
      return {
        columns: [{ name: 'person_key', type: 'Utf8' }, { name: 'props', type: 'Utf8' }],
        rows: list.slice(0, limit).map((p) => [p.person_key, JSON.stringify(p.props)]),
      }
    }
    if (/group by/.test(lower) && /person_key/.test(lower.split(' from ')[0])) {
      const agg = new Map()
      for (const e of rows) {
        const a = agg.get(e.person_key) ?? { n: 0, last: 0 }
        a.n++
        a.last = Math.max(a.last, e.ts)
        agg.set(e.person_key, a)
      }
      return {
        columns: [{ name: 'person_key', type: 'Utf8' }, { name: 'events', type: 'Int64' }, { name: 'last_seen', type: 'Timestamp(Millisecond, Some("UTC"))' }],
        rows: [...agg.entries()].sort((a, b) => b[1].last - a[1].last).slice(0, limit).map(([k, a]) => [k, a.n, a.last]),
      }
    }
    if (/group by/.test(lower)) {
      const col = [...COLUMNS].find((c) => new RegExp(`\\b${c}\\b`).test(lower.split(' from ')[0])) ?? 'event'
      const agg = new Map()
      for (const e of rows) agg.set(e[col], (agg.get(e[col]) ?? 0) + 1)
      return {
        columns: [{ name: col, type: 'Utf8' }, { name: 'count(*)', type: 'Int64' }],
        rows: [...agg.entries()].sort((a, b) => b[1] - a[1]).slice(0, limit),
      }
    }
    if (/count\(/.test(lower)) return { columns: [{ name: 'count(*)', type: 'Int64' }], rows: [[rows.length]] }
    return {
      columns: [
        { name: 'ts', type: 'Timestamp(Millisecond, Some("UTC"))' }, { name: 'event', type: 'Utf8' }, { name: 'person_key', type: 'Utf8' },
        { name: 'platform', type: 'Utf8' }, { name: 'country', type: 'Utf8' }, { name: 'props', type: 'Utf8' },
      ],
      rows: rows.slice(-limit).reverse().map((e) => [e.ts, e.event, e.person_key, e.platform, e.country, JSON.stringify(e.props)]),
    }
  }

  const schema = () => {
    const now = nowFn()
    const recent = dataset.events.filter((e) => e.ts > now - 7 * DAY && e.ts <= now)
    const names = new Map()
    const props = new Map()
    for (const e of recent) {
      names.set(e.event, (names.get(e.event) ?? 0) + 1)
      for (const [k, v] of Object.entries(e.props)) {
        if (v === null) continue
        const p = props.get(k) ?? { n: 0, type: typeof v == 'number' ? (Number.isInteger(v) ? 'int' : 'float') : typeof v == 'boolean' ? 'bool' : 'string' }
        if (p.type == 'int' && typeof v == 'number' && !Number.isInteger(v)) p.type = 'float'
        p.n++
        props.set(k, p)
      }
    }
    const personProps = new Set()
    for (const p of dataset.persons) for (const k of Object.keys(p.props)) personProps.add(k)
    return {
      events: [...names.entries()].sort((a, b) => b[1] - a[1]).map(([name]) => name),
      props: [...props.entries()].sort((a, b) => b[1].n - a[1].n).map(([key, p]) => ({ key, type: p.type, promoted: p.n / Math.max(1, recent.length) >= 0.05 })),
      person_props: [...personProps].sort(),
    }
  }

  const seenRequests = new Set()
  /** Runs one endpoint and attaches a plausible `meta`. */
  const run = (endpoint, body, pid) => {
    const started = performance.now()
    const q = body ?? {}
    const result =
      endpoint == 'trends' ? trends(q) : endpoint == 'funnel' ? funnel(q) : endpoint == 'retention' ? retention(q)
      : endpoint == 'events' ? events(q, pid) : endpoint == 'sql' ? sql(q) : endpoint == 'schema' ? schema() : null
    if (!result) throw new QueryError(404, 'not_found', 'Bilinmeyen sorgu ucu')
    if (endpoint == 'schema') return result
    const signature = createHash('sha256').update(pid + endpoint + JSON.stringify(q)).digest('hex')
    const cached = seenRequests.has(signature)
    seenRequests.add(signature)
    const [from, to] = parseBounds(q)
    const days = Math.max(1, Math.round((to - from) / DAY))
    const manifests = {}
    for (let d = Math.max(from, to - 3 * DAY); d < to; d += DAY) manifests[new Date(d).toISOString().slice(0, 10)] = 3 + (Math.floor(d / DAY) % 40)
    return {
      ...result,
      meta: {
        elapsed_ms: cached ? 1 : Math.max(3, Math.round(performance.now() - started) + 6 + (parseInt(signature.slice(0, 2), 16) % 40)),
        bytes_scanned: cached ? 0 : days * 262_144 + (parseInt(signature.slice(2, 6), 16) % 65536),
        files: cached ? 0 : days,
        cached,
        manifests,
      },
    }
  }

  /** `key` must start with the project's own prefix; returns { body, encoding, seq } or throws. */
  const replayChunk = (key, pid) => {
    const prefix = `replay/p=${pid}/`
    if (typeof key != 'string' || !key.startsWith(prefix)) throw new QueryError(403, 'forbidden', 'Anahtar bu projeye ait değil')
    const found = dataset.chunkBodies.get(key.slice(prefix.length))
    if (!found) throw new QueryError(404, 'not_found', 'Replay parçası bulunamadı')
    return found
  }

  return { run, replayChunk }
}
