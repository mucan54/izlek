// A fake browser for unit tests: just the globals the SDK touches, plus a
// scriptable transport. Time is controlled with node:test mock timers.
import { gunzipSync } from 'node:zlib'

import { decodeBatch, propsToObject } from './la1.mjs'

export const T0 = 1795046400000 // 2026-11-18T12:00:00Z, as in the spec vectors

/** In-memory Storage with the Web Storage surface the SDK uses. */
export function makeStorage(initial = {}) {
  const m = new Map(Object.entries(initial))
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => void m.set(k, String(v)),
    removeItem: (k) => void m.delete(k),
    key: (i) => [...m.keys()][i] ?? null,
    get length() {
      return m.size
    },
    /** test-only view */
    map: m,
  }
}

const setGlobal = (name, value) =>
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true, enumerable: false })

/**
 * Install a fresh fake page on globalThis and return handles to drive it.
 * `storage` and `cookies` can be passed on to a later `installEnv` to simulate
 * the next page load of the same browser.
 */
export function installEnv({
  url = 'https://shop.example/home?utm_source=news&utm_campaign=kis&q=1',
  referrer = 'https://search.example/?q=shop',
  ua = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36',
  storage = makeStorage(),
  cookies = new Map(),
  keepalive = true,
} = {}) {
  const loc = new URL(url)
  const listeners = { window: {}, document: {} }
  const target = (name) => ({
    addEventListener: (type, f) => (listeners[name][type] ||= []).push(f),
    removeEventListener: (type, f) => {
      listeners[name][type] = (listeners[name][type] || []).filter((x) => x != f)
    },
  })
  const fire = (name, type, event = {}) => {
    for (const f of listeners[name][type] || []) f({ type, ...event })
  }

  const location = {
    get href() {
      return loc.href
    },
    get pathname() {
      return loc.pathname
    },
    get search() {
      return loc.search
    },
    get protocol() {
      return loc.protocol
    },
  }
  const history = {
    pushState(_state, _title, to) {
      loc.href = new URL(to, loc.href).href
    },
    replaceState(_state, _title, to) {
      if (to != null) loc.href = new URL(to, loc.href).href
    },
  }
  const document = {
    ...target('document'),
    referrer,
    visibilityState: 'visible',
    cookieWrites: [],
    get cookie() {
      return [...cookies].map(([k, v]) => `${k}=${v}`).join('; ')
    },
    set cookie(s) {
      this.cookieWrites.push(s)
      const [pair] = s.split(';')
      const eq = pair.indexOf('=')
      cookies.set(pair.slice(0, eq), pair.slice(eq + 1))
    },
  }

  // transport
  const calls = []
  const beacons = []
  let responder = () => 200
  const record = (via, to, init = {}) => {
    const headers = Object.fromEntries(Object.entries(init.headers || {}).map(([k, v]) => [k.toLowerCase(), v]))
    let body = new Uint8Array(init.body instanceof ArrayBuffer ? init.body : (init.body?.buffer ?? []))
    if (headers['content-encoding'] == 'gzip') body = new Uint8Array(gunzipSync(body))
    const call = { via, url: String(to), headers, keepalive: !!init.keepalive, body, at: Date.now() }
    Object.defineProperty(call, 'batch', { get: () => decodeBatch(call.body, { strict: true }) })
    calls.push(call)
    return call
  }
  const fetch = async (to, init) => {
    const call = record('fetch', to, init)
    const r = await responder(call, calls.length - 1)
    if (r == 'network') throw new TypeError('Failed to fetch')
    const { status, headers = {}, json } = typeof r == 'object' ? r : { status: r }
    return { status, headers: { get: (k) => headers[k.toLowerCase()] ?? null }, json: async () => json }
  }
  const navigator = {
    userAgent: ua,
    language: 'tr-TR',
    sendBeacon(to, body) {
      beacons.push(record('beacon', to, { body }))
      return true
    },
  }

  setGlobal('window', target('window'))
  setGlobal('document', document)
  setGlobal('location', location)
  setGlobal('history', history)
  setGlobal('navigator', navigator)
  setGlobal('localStorage', storage)
  setGlobal('fetch', fetch)
  setGlobal('innerWidth', 1280)
  setGlobal('innerHeight', 720)
  // feature detection target for `'keepalive' in Request.prototype`
  setGlobal('Request', keepalive ? class { get keepalive() { return false } } : class {})

  return {
    storage,
    cookies,
    calls,
    beacons,
    document,
    history,
    loc,
    /** `fn(call, index)` → status | { status, headers, json } | 'network' (a constant works too) */
    respond(fn) {
      responder = typeof fn == 'function' ? fn : () => fn
    },
    fireWindow: (type, e) => fire('window', type, e),
    fireDocument: (type, e) => fire('document', type, e),
    hide() {
      document.visibilityState = 'hidden'
      fire('document', 'visibilitychange')
    },
    /** All records delivered so far, as `{ kind, name, props, batch }`, in send order. */
    records() {
      return calls
        .filter((c) => c.url.includes('/v1/e'))
        .flatMap((c) => c.batch.records.map((r) => ({ ...r, props: propsToObject(r.props), batch: c.batch })))
    },
  }
}

/** Let promise chains (and real I/O such as gzip) settle without advancing the mocked clock. */
export async function settle(until = () => true, rounds = 20) {
  for (let i = 0; i < 2000; i++) {
    await new Promise((r) => setImmediate(r))
    if (i >= rounds && until()) return
  }
  throw new Error('settle: condition not reached')
}
