// Izlek web SDK core: event capture, identity, queueing and delivery.
//
// Design notes (docs/design.md §3.5, §4.6, §5):
//  - A batch header carries exactly one identity (anon_id, user_id, session_id),
//    so any identity or session change closes the open batch.
//  - Sealed batches live in memory and are mirrored to localStorage, one key
//    per batch, named after the batch_id. A resend (same page or next page
//    load) therefore always carries the same batch_id; only `sent_at` is
//    restamped so that server-side clock correction stays right.
//  - Nothing here touches `window` at module load, and no public method throws.

import { batch, headerTail, hex, put, record, toProps, unhex, utf8, type Bytes, type Props } from './wire'

export type { Props } from './wire'

export const SDK = 'web/0.1.0'

export interface Config {
  /** Public write token (`pk_…`). */
  token: string
  /** Ingestion origin, e.g. `https://in.example.com`. */
  host: string
  /** Seal and send the open batch at this many records. Default 20. */
  flushAt?: number
  /** Seal and send the open batch after this long. Default 5000 ms. */
  flushIntervalMs?: number
  /** Cap for queued batches, in memory and in localStorage. Default 200 000. */
  maxQueueBytes?: number
  /** gzip request bodies when `CompressionStream` exists. Default true. */
  gzip?: boolean
  /** Cookie `domain` for the anon_id cookie, e.g. `.example.com`. */
  cookieDomain?: string
  /** Automatic `pageview` / `pageleave`, SPA navigation included. Default true. */
  autoPageview?: boolean
  /** Click tracking for elements marked with `data-track`. Default true. */
  autoTrack?: boolean
  /** Origin serving `flags/<token>.json` (flags module). Defaults to `host`. */
  flagsHost?: string
}

/** Persisted under `la_<token>`. Keys are short because they are stored, not minified. */
export interface State {
  /** anon_id */
  a: string
  /** user_id ('' = anonymous) */
  u: string
  /** session_id (hex32) */
  s?: string
  /** last activity (unix ms) */
  t?: number
  /** super props */
  p: Props
  /** opted out */
  o?: number
  /** sending blocked until (unix ms), after 401/403 */
  b?: number
}

/** Called for every accepted record: kind (0 event · 1 identify · 2 set_person · 3 reset). */
export type Hook = (kind: number, name: string, props: Props) => void

/** Private surface for the flags and replay modules. Not a public API. */
export interface Internals {
  /** config; undefined until `init` succeeded */
  c(): Config | undefined
  /** persisted state (live object) */
  s(): State
  /** automatic context of the current page */
  x(): Props
  /** current session id (hex32), rolled over if it expired */
  i(): string
  /** record hooks */
  h: Hook[]
  /** gzip helper */
  z(b: Uint8Array<ArrayBuffer>): Promise<ArrayBuffer>
}

export interface Client {
  init(config: Config): void
  capture(event: string, props?: Props): void
  /** Record a `pageview` for the current URL (automatic unless `autoPageview: false`). */
  page(props?: Props): void
  identify(userId: string, personProps?: Props): void
  setPersonProps(props: Props): void
  /** Super props: merged into every event. */
  register(props: Props): void
  unregister(key: string): void
  /** Log out: new anon_id, user_id cleared. */
  reset(): void
  optOut(): void
  optIn(): void
  /** Send everything queued; resolves when the queue is empty or sending is blocked. */
  flush(): Promise<void>
  /** @internal */
  _: Internals
}

interface Batch {
  /** batch_id */
  i: ArrayLike<number>
  /** header tail (sdk … context) */
  h: Bytes
  /** framed records */
  r: Bytes
  /** record count */
  n: number
  /** identity key the batch was opened with */
  k?: string
  /** page URL of the header context */
  u?: string
  /** a request is in flight */
  s?: number
}

const BOT = /bot[/;)-]|crawl|spider|slurp|headless|lighthouse|prerender|phantom|pingdom|preview|externalhit/i
const SESSION_MS = 18e5 // 30 min
const BLOCK_MS = 36e5 // 1 h after 401/403
const MAX_AGE_MS = 6048e5 // stored batches older than 7 days are discarded

const gzip = (b: Uint8Array<ArrayBuffer>): Promise<ArrayBuffer> =>
  new Response(new Blob([b]).stream().pipeThrough(new CompressionStream('gzip'))).arrayBuffer()

export const create = (): Client => {
  let cfg: Config | undefined
  let K = '' // storage and cookie key; non-empty once init succeeded
  let url: string
  let S = {} as State
  let P: { url: string; path: string; referrer: string } // page the next events belong to
  let B: Batch | undefined // open batch
  let Q: Batch[] = [] // sealed batches, oldest first
  let until = 0 // no sending before this time
  let fails = 0 // consecutive transient failures
  let lost = 0 // dropped records, reported as `dropped_count`
  let flushTimer: ReturnType<typeof setTimeout>
  let retryTimer: ReturnType<typeof setTimeout> | 0
  let loop: Promise<void> | 0
  const hooks: Hook[] = []

  const now = () => Date.now()

  /** Run only after init, never throw. */
  const safe =
    <A extends any[], R>(f: (...a: A) => R) =>
    (...a: A): R | undefined => {
      try {
        if (K) return f(...a)
      } catch {}
    }

  const uuid = () => {
    const b = crypto.getRandomValues(new Uint8Array(16))
    // 48-bit big-endian unix ms; typed array stores truncate and wrap mod 256
    for (let i = 6, t = now(); i--; t /= 256) b[i] = t
    b[6] = (b[6] & 15) | 112
    b[8] = (b[8] & 63) | 128
    return b
  }
  const anon = () => hex(uuid()).replace(/(.{8})(.{4})(.{4})(.{4})/, '$1-$2-$3-$4-')

  /** localStorage get (`v` omitted), set (string) or remove (`0`); storage errors are swallowed. */
  const ls = (k: string, v?: string | 0): string | null | undefined => {
    try {
      return v == null ? localStorage.getItem(k) : ((v ? localStorage.setItem(k, v) : localStorage.removeItem(k)), '')
    } catch {}
  }

  const save = () => {
    ls(K, JSON.stringify(S))
    try {
      document.cookie =
        K +
        '=' +
        S.a +
        ';path=/;max-age=31536000;samesite=lax' +
        (cfg!.cookieDomain ? ';domain=' + cfg!.cookieDomain : '') +
        (location.protocol == 'https:' ? ';secure' : '')
    } catch {}
  }

  /** Storage keys of persisted batches (`la_<token>_<batch_id hex>`); UUIDv7 ids sort oldest first. */
  const keys = () => {
    const r: string[] = []
    try {
      for (let i = 0, k; (k = localStorage.key(i)); i++) if (k.length == K.length + 33 && k.startsWith(K + '_')) r.push(k)
    } catch {}
    return r.sort()
  }

  const size = (b: Batch) => b.h.length + b.r.length
  const cap = () => cfg!.maxQueueBytes || 2e5

  /** Persist a sealed batch, evicting the oldest stored batches to stay under the cap. */
  const store = (b: Batch) => {
    // one UTF-16 unit per byte, shifted by 256 to stay clear of NUL and control characters
    let v = ''
    for (const x of [b.h.length >> 8, b.h.length & 255, b.n >> 8, b.n & 255].concat(b.h, b.r)) v += String.fromCharCode(x + 256)
    const ks = keys()
    let n = v.length
    for (const k of ks) n += (ls(k) || '').length
    for (; n > cap() && ks.length; ls(ks.shift()!, 0)) n -= (ls(ks[0]) || '').length
    if (n <= cap()) ls(K + '_' + hex(b.i), v)
  }

  const rm = (b: Batch, keepStored?: unknown) => {
    Q = Q.filter((x) => x != b)
    keepStored || ls(K + '_' + hex(b.i), 0)
  }
  const drop = (b: Batch) => {
    rm(b)
    lost += b.n
  }

  const ctx = (): Props => {
    const x: Props = {
      platform: 'web',
      ...P,
      locale: navigator.language,
      viewport_w: innerWidth,
      viewport_h: innerHeight,
      ua: navigator.userAgent,
    }
    new URL(P.url).searchParams.forEach((v, k) => {
      if (/^utm_/.test(k)) x[k] = v
    })
    return x
  }

  const session = () => {
    // 30 min without an event ends the session
    if (!S.s || now() - S.t! > SESSION_MS) {
      S.s = hex(uuid())
      S.t = now()
    }
    return S.s
  }

  /** Close the open batch and queue it. */
  const seal = () => {
    clearTimeout(flushTimer)
    if (B && B.n) {
      Q.push(B)
      for (let n = Q.reduce((a, b) => a + size(b), 0); n > cap() && Q[1]; drop(Q[0])) n -= size(Q[0])
      store(B)
    }
    B = undefined
    save()
  }

  /** 413: resend the records as two batches with fresh ids. */
  const split = (b: Batch) => {
    rm(b)
    if (b.n < 2) return lost++
    const r = b.r
    const m = b.n >> 1
    let i = 0
    for (let n = 0; n < m; n++) {
      // skip one framed record: varint(len) + len bytes
      let len = 0
      let c
      let mul = 1
      do {
        c = r[i++]
        len += (c & 127) * mul
        mul *= 128
      } while (c > 127)
      i += len
    }
    const halves = [
      { ...b, i: uuid(), r: r.slice(0, i), n: m },
      { ...b, i: uuid(), r: r.slice(i), n: b.n - m },
    ]
    Q.unshift(...halves)
    halves.map(store)
  }

  /** One delivery attempt, then act on the status code (design §3.5). */
  const send = async (b: Batch, unload?: unknown) => {
    b.s = 1
    let status = 0
    let res: Response | undefined
    let wait: any
    try {
      let body: Uint8Array<ArrayBuffer> | ArrayBuffer = batch(b.i, now(), b.h, b.r)
      if (unload) {
        // no custom headers: a "simple" CORS request needs no preflight while the page is going away
        if ('keepalive' in Request.prototype)
          res = await fetch(url + '?t=' + cfg!.token, { method: 'POST', body, keepalive: true })
        else status = navigator.sendBeacon(url + '?t=' + cfg!.token, body) ? -1 : 0
      } else {
        const headers: Record<string, string> = {
          authorization: 'Bearer ' + cfg!.token,
          'content-type': 'application/octet-stream',
        }
        if (cfg!.gzip !== false && body.length > 1e3 && typeof CompressionStream != 'undefined') {
          body = await gzip(body)
          headers['content-encoding'] = 'gzip'
        }
        res = await fetch(url, { method: 'POST', body, headers })
      }
      if (res) {
        status = res.status
        wait = res.headers.get('retry-after')
      }
    } catch {}
    b.s = 0
    if (Q.includes(b)) {
      const t = now()
      // -1: handed to sendBeacon, outcome unknown. Forget it here but keep the stored
      // copy, which the next page load resends under the same batch_id.
      if (status < 0) rm(b, 1)
      else if (status > 199 && status < 300) {
        rm(b)
        fails = 0
      } else if (status == 400) drop(b)
      else if (status == 401 || status == 403) {
        drop(b)
        until = S.b = t + BLOCK_MS
        save()
      } else if (status == 413) split(b)
      else
        until =
          t +
          // the server sends Retry-After with 429 and 503 (spec/wire.md §4)
          ((status == 429 || status == 503) && wait > 0
            ? wait * 1e3
            : Math.min(3e5, 1e3 * 2 ** fails++) * (0.5 + Math.random() / 2))
    }
  }

  /** Send queued batches one at a time until the queue is empty or sending is blocked. */
  const drain = (): Promise<void> =>
    (loop ||= (async () => {
      for (let b; now() >= until && (b = Q.find((x) => !x.s)); ) await send(b)
    })()
      .catch(() => {})
      .then(() => {
        loop = 0
        if (Q.some((x) => !x.s)) {
          // a batch may have been queued after the loop ended but before this callback ran
          if (now() >= until) return drain()
          retryTimer ||= setTimeout(
            safe(() => {
              retryTimer = 0
              drain()
            }),
            until - now(),
          )
        }
      }))

  const flush = () => {
    seal()
    return drain()
  }

  /** Page is being hidden or unloaded: hand everything to the browser with keepalive. */
  const hide = () => {
    seal()
    let n = 0
    // keepalive bodies share a 64 KiB budget; whatever does not fit stays persisted
    if (now() >= until) for (const b of Q) b.s || (n += size(b)) > 6e4 || send(b, 1).then(drain)
  }

  const emit = (kind: number, name: string, props?: Props | null) => {
    if (S.o) return
    const key = S.a + S.u + session()
    const t = (S.t = now())
    if (B && B.k != key) seal()
    if (!B) {
      const x = ctx()
      if (lost) {
        x.dropped_count = lost
        lost = 0
      }
      B = { i: uuid(), h: headerTail(SDK, S.a, S.u, unhex(S.s!), toProps(x)), r: [], n: 0, k: key, u: P.url }
      flushTimer = setTimeout(safe(flush), cfg!.flushIntervalMs || 5e3)
    }
    // the header context describes the page the batch was opened on
    if (!kind && P.url != B.u) props = { ...P, ...props }
    const r = record(kind, uuid(), t, name, toProps(props))
    // limits that would make the server reject the record (or the whole batch)
    if (r.length > 65e3 || utf8(name).length > 128) lost++
    else {
      put(B.r, r)
      if (++B.n >= (cfg!.flushAt || 20) || B.r.length > 5e4) flush()
    }
    for (const h of hooks) h(kind, name, props || {})
  }

  const capture = (name: string, props?: Props) => {
    if (name) emit(0, name, { ...S.p, ...props })
  }

  const view = (props?: Props) => {
    if (location.href != P.url) P = { url: location.href, path: location.pathname, referrer: P.url }
    capture('pageview', { ...P, ...props })
  }
  const leave = () => capture('pageleave', P)
  const nav = safe(() => {
    if (location.href.split('#')[0] != P.url.split('#')[0]) {
      leave()
      view()
    }
  })

  const init = (c: Config) => {
    try {
      if (K || typeof window == 'undefined' || !c.token || BOT.test(navigator.userAgent)) return
      cfg = c
      K = 'la_' + c.token
      url = c.host.replace(/\/+$/, '') + '/v1/e'
      try {
        S = JSON.parse(ls(K)!) || S
      } catch {}
      // the cookie is shared across subdomains, so it wins over this origin's localStorage
      const cookie = document.cookie.match('(^|; )' + K + '=([^;]+)')
      if (cookie && cookie[2] != S.a) {
        S.a = cookie[2]
        S.u = ''
      }
      S.a = S.a || anon()
      S.u = S.u || ''
      S.p = S.p || {}
      until = S.b || 0
      P = { url: location.href, path: location.pathname, referrer: document.referrer }

      // batches left behind by earlier page loads
      for (const k of keys()) {
        const id = k.slice(-32)
        const v = ls(k)
        if (!v || parseInt(id.slice(0, 12), 16) < now() - MAX_AGE_MS) ls(k, 0)
        else {
          const a = Array.from(v, (ch) => ch.charCodeAt(0) - 256)
          const n = (a[0] << 8) | a[1]
          Q.push({ i: unhex(id), h: a.slice(4, 4 + n), r: a.slice(4 + n), n: (a[2] << 8) | a[3] })
        }
      }

      const on = (target: EventTarget, type: string, f: (e: any) => void, capturePhase?: boolean) =>
        target.addEventListener(type, safe(f), capturePhase)
      const auto = c.autoPageview !== false

      on(window, 'pagehide', () => {
        if (auto) leave()
        hide()
      })
      on(document, 'visibilitychange', () => {
        if (document.visibilityState == 'hidden') hide()
      })
      // another tab changed identity, super props or consent
      on(window, 'storage', (e) => {
        if (e.key == K && e.newValue) {
          S = JSON.parse(e.newValue)
          if (S.o) {
            B = undefined
            Q = []
          }
        }
      })
      if (c.autoTrack !== false)
        on(
          document,
          'click',
          (e) => {
            const el: Element | null = e.target.closest && e.target.closest('[data-track]')
            if (el) {
              const p: Props = {}
              for (const a of el.attributes) if (/^data-track-/.test(a.name)) p[a.name.slice(11)] = a.value
              capture(el.getAttribute('data-track') || 'click', p)
            }
          },
          true,
        )
      save()
      if (auto) {
        for (const m of ['pushState', 'replaceState'] as const) {
          const orig = history[m]
          history[m] = function (this: History, ...a) {
            const r = orig.apply(this, a)
            nav()
            return r
          }
        }
        on(window, 'popstate', nav)
        on(window, 'pageshow', (e) => {
          if (e.persisted) view()
        })
        view()
      }
      drain()
    } catch {
      K = ''
      cfg = undefined
    }
  }

  return {
    init,
    capture: safe(capture),
    page: safe(view),
    identify: safe((id: string, props?: Props) => {
      if (id && utf8((id += '')).length <= 200) {
        if (id != S.u) {
          // emit() sees the identity change and closes the anonymous batch first
          S.u = id
          emit(1, '', { ...props, user_id: id })
        } else if (props) emit(2, '', props)
        save()
      }
    }),
    setPersonProps: safe((props: Props) => emit(2, '', props)),
    register: safe((props: Props) => {
      Object.assign(S.p, props)
      save()
    }),
    unregister: safe((key: string) => {
      delete S.p[key]
      save()
    }),
    reset: safe(() => {
      // the reset record still belongs to the old identity; sealing right away keeps it there
      emit(3, '', {})
      S.a = anon()
      S.u = S.s = ''
      seal()
    }),
    optOut: safe(() => {
      S.o = 1
      B = undefined
      Q = []
      keys().map((k) => ls(k, 0))
      save()
    }),
    optIn: safe(() => {
      S.o = 0
      save()
    }),
    flush: () => Promise.resolve(safe(flush)()),
    _: { c: () => cfg, s: () => S, x: ctx, i: session, h: hooks, z: gzip },
  }
}

/** Default client behind the named exports. Creating it touches no browser API. */
export const client: Client = /* @__PURE__ */ create()

export const { init, capture, page, identify, setPersonProps, register, unregister, reset, optOut, optIn, flush } = client
