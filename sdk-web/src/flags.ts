// Feature flags: ruleset download + local evaluation (spec/flags.md).

import { client as defaultClient, type Client, type Props } from './index'
import { murmur3 } from './murmur3'

export { murmur3 }

export type Cond =
  | { and: Cond[] }
  | { or: Cond[] }
  | { not: Cond }
  | { prop: string; op: string; value?: unknown }

export interface Variant {
  key: string
  w: number
}

export interface Rule {
  when?: Cond | null
  rollout?: number
  variants?: Variant[] | null
  stable_on?: string
}

export interface Flag {
  key: string
  enabled: boolean
  salt: string
  rules?: Rule[]
  default?: null | boolean | string
}

export interface Ruleset {
  v: number
  flags: Flag[]
}

/** `null` = off / no match, `true` = on without variants, string = variant key. */
export type FlagValue = null | true | string

const NUMERIC = /^-?[0-9]+(\.[0-9]+)?$/

const isNum = (x: unknown): x is number => typeof x == 'number'

/** `str(x)` of spec/flags.md §4. */
const toStr = (x: unknown): string =>
  typeof x == 'string'
    ? x
    : isNum(x)
      ? // integer-valued numbers print without exponent or fraction, even past 1e21
        Number.isInteger(x) && Math.abs(x) >= 1e21
        ? BigInt(x) + ''
        : x + ''
      : typeof x == 'boolean'
        ? x + ''
        : JSON.stringify(x ?? null)

const toNum = (x: unknown): number | null => (isNum(x) ? x : typeof x == 'string' && NUMERIC.test(x) ? +x : null)

const looseEq = (p: unknown, v: unknown) => (isNum(p) && isNum(v) ? p == v : toStr(p) == toStr(v))

/** MAJOR[.MINOR[.PATCH]] → comparable triple, or null. */
const semver = (x: unknown): number[] | null => {
  const parts = toStr(x)
    .trim()
    .replace(/^v/i, '')
    .split(/[-+]/)[0]
    .split('.')
  if (parts.length > 3 || !parts.every((p) => /^[0-9]+$/.test(p))) return null
  return [0, 1, 2].map((i) => +(parts[i] || 0))
}

const cmp = (a: number[], b: number[]) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2]

export const matchCond = (cond: Cond, props: Props): boolean => {
  const c = cond as any
  if ('and' in c) return c.and.every((x: Cond) => matchCond(x, props))
  if ('or' in c) return c.or.some((x: Cond) => matchCond(x, props))
  if ('not' in c) return !matchCond(c.not, props)
  const p = props[c.prop]
  const op: string = c.op
  const v = c.value
  if (p == null) return op == 'ne' || op == 'not_in'
  const any = () => Array.isArray(v) && v.some((x) => looseEq(p, x))
  switch (op) {
    case 'is_set':
      return true
    case 'eq':
      return looseEq(p, v)
    case 'ne':
      return !looseEq(p, v)
    case 'in':
      return any()
    case 'not_in':
      return !any()
    case 'gt':
    case 'gte':
    case 'lt':
    case 'lte': {
      const a = toNum(p)
      const b = toNum(v)
      if (a == null || b == null) return false
      return op == 'gt' ? a > b : op == 'gte' ? a >= b : op == 'lt' ? a < b : a <= b
    }
    case 'contains':
      return toStr(p).includes(toStr(v))
    case 'regex':
      try {
        return new RegExp(toStr(v)).test(toStr(p))
      } catch {
        return false
      }
    case 'semver_gte':
    case 'semver_lt': {
      const a = semver(p)
      const b = semver(v)
      if (!a || !b) return false
      return op == 'semver_gte' ? cmp(a, b) >= 0 : cmp(a, b) < 0
    }
  }
  return false
}

/** Evaluate one flag exactly as spec/flags.md §3. `userId: ''` = anonymous. */
export const evalFlag = (flag: Flag, props: Props, userId: string, anonId: string): FlagValue => {
  const def = flag.default === false || flag.default == null ? null : flag.default
  if (!flag.enabled) return def
  const all = { ...props, user_id: userId || null, anon_id: anonId }
  for (const rule of flag.rules || []) {
    if (rule.when != null && !matchCond(rule.when, all)) continue
    const id = rule.stable_on == 'anon_id' ? anonId : userId || anonId
    // the first matching rule decides, in or out
    if (murmur3(flag.salt + ':' + flag.key + ':' + id) % 10000 >= (rule.rollout ?? 10000)) return def
    const variants = rule.variants || []
    if (!variants.length) return true
    const v = murmur3(flag.salt + ':v:' + flag.key + ':' + id) % 10000
    let acc = 0
    for (const variant of variants) if (v < (acc += variant.w)) return variant.key
    return variants[variants.length - 1].key
  }
  return def
}

export interface FlagsOptions {
  /** Poll interval while the page is visible. Default 5 min. */
  refreshMs?: number
}

export interface Flags {
  /**
   * Start after `init()`: loads the last good ruleset from localStorage
   * synchronously, then refreshes in the background. The promise resolves
   * after the first refresh attempt, whatever its outcome.
   */
  start(options?: FlagsOptions): Promise<void>
  /**
   * Value of a flag: `true`, a variant key, or `fallback` when the flag is
   * off, unknown, or no ruleset is available. Records `flag_exposed` once per
   * session (again only if the value changes within the session).
   */
  flag<T = null>(key: string, fallback?: T): true | string | T
  /** All flag values, without recording exposures. */
  all(): Record<string, FlagValue>
  /** Refresh now. */
  reload(): Promise<void>
  /** Called whenever a new ruleset was downloaded. Returns an unsubscribe function. */
  onChange(listener: () => void): () => void
  /** The ruleset in use, if any. */
  ruleset(): Ruleset | undefined
  stop(): void
}

export const createFlags = (client: Client): Flags => {
  const core = client._
  let K = '' // storage key prefix; non-empty once started
  let url: string
  let rules: Ruleset | undefined
  let etag: string | null | undefined
  let person: Props = {} // person props seen by this SDK (identify / setPersonProps)
  let seen: { s?: string; e: Record<string, string> } = { e: {} } // exposures of the current session
  let lastLoad = 0
  let every = 3e5
  let timer: ReturnType<typeof setInterval> | undefined
  const listeners: Array<() => void> = []

  const read = (k: string): any => {
    try {
      return JSON.parse(localStorage.getItem(K + k)!)
    } catch {}
  }
  const write = (k: string, v: unknown) => {
    try {
      localStorage.setItem(K + k, JSON.stringify(v))
    } catch {}
  }

  const evaluate = (f: Flag) => {
    const s = core.s()
    // precedence: automatic context → super props → person props
    return evalFlag(f, { ...core.x(), ...s.p, ...person }, s.u, s.a)
  }

  const load = async () => {
    lastLoad = Date.now()
    try {
      const res = await fetch(url, etag ? { headers: { 'if-none-match': etag } } : undefined)
      // 304: unchanged. Anything but a well-formed 200 keeps the last good ruleset.
      if (res.status == 200) {
        const r = await res.json()
        if (r && Array.isArray(r.flags)) {
          rules = r
          etag = res.headers.get('etag')
          write('_f', { e: etag, r })
          for (const l of listeners) {
            try {
              l()
            } catch {}
          }
        }
      }
    } catch {}
  }

  const refreshIfVisible = () => {
    if (document.visibilityState == 'visible' && Date.now() - lastLoad >= every) load()
  }

  return {
    start(options = {}) {
      try {
        const c = core.c()
        if (!c || K) return Promise.resolve()
        K = 'la_' + c.token
        url = (c.flagsHost || c.host).replace(/\/+$/, '') + '/flags/' + c.token + '.json'
        every = options.refreshMs || every
        const cached = read('_f')
        if (cached && cached.r) {
          rules = cached.r
          etag = cached.e
        }
        person = read('_p') || {}
        seen = read('_x') || seen
        core.h.push((kind, _name, props) => {
          if (kind == 1 || kind == 2) {
            person = { ...person, ...props }
            delete person.user_id
          } else if (kind == 3) person = {}
          else return
          write('_p', person)
        })
        timer = setInterval(refreshIfVisible, every)
        document.addEventListener('visibilitychange', refreshIfVisible)
        return load()
      } catch {
        return Promise.resolve()
      }
    },
    flag(key: string, fallback: any = null) {
      try {
        const f = rules && rules.flags.find((x) => x.key == key)
        if (!f) return fallback
        const value = evaluate(f)
        if (!core.s().o) {
          const sid = core.i()
          if (seen.s != sid) seen = { s: sid, e: {} }
          const variant = value == null ? 'false' : value + ''
          if (seen.e[key] !== variant) {
            seen.e[key] = variant
            write('_x', seen)
            client.capture('flag_exposed', { flag: key, variant, ruleset_v: rules!.v })
          }
        }
        return value == null ? fallback : value
      } catch {
        return fallback
      }
    },
    all() {
      const out: Record<string, FlagValue> = {}
      try {
        for (const f of rules ? rules.flags : []) out[f.key] = evaluate(f)
      } catch {}
      return out
    },
    reload: () => (K ? load() : Promise.resolve()),
    onChange(listener) {
      listeners.push(listener)
      return () => {
        const i = listeners.indexOf(listener)
        if (i >= 0) listeners.splice(i, 1)
      }
    },
    ruleset: () => rules,
    stop() {
      clearInterval(timer)
      try {
        document.removeEventListener('visibilitychange', refreshIfVisible)
      } catch {}
    },
  }
}

/** Flags bound to the default client. */
export const flags: Flags = /* @__PURE__ */ createFlags(defaultClient)

export const initFlags = (options?: FlagsOptions): Promise<void> => flags.start(options)
export const flag = <T = null>(key: string, fallback?: T): true | string | T => flags.flag(key, fallback)
export const allFlags = (): Record<string, FlagValue> => flags.all()
export const reloadFlags = (): Promise<void> => flags.reload()
export const onFlagsChange = (listener: () => void): (() => void) => flags.onChange(listener)
