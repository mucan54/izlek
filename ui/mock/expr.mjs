// Loose-typed condition matching shared by query filters (spec/query.md §2)
// and feature-flag rules (spec/flags.md §3–4), plus murmur3 and flag evaluation.

/** MurmurHash3 x86_32, seed 0, over the UTF-8 bytes of `text`; unsigned result. */
export const murmur3 = (text) => {
  const data = new TextEncoder().encode(text)
  const c1 = 0xcc9e2d51
  const c2 = 0x1b873593
  let h = 0
  const blocks = data.length >> 2
  for (let i = 0; i < blocks; i++) {
    let k = data[i * 4] | (data[i * 4 + 1] << 8) | (data[i * 4 + 2] << 16) | (data[i * 4 + 3] << 24)
    k = Math.imul(k, c1)
    k = (k << 15) | (k >>> 17)
    k = Math.imul(k, c2)
    h ^= k
    h = (h << 13) | (h >>> 19)
    h = (Math.imul(h, 5) + 0xe6546b64) | 0
  }
  let k = 0
  const tail = blocks * 4
  switch (data.length & 3) {
    case 3:
      k ^= data[tail + 2] << 16
    // fallthrough
    case 2:
      k ^= data[tail + 1] << 8
    // fallthrough
    case 1:
      k ^= data[tail]
      k = Math.imul(k, c1)
      k = (k << 15) | (k >>> 17)
      k = Math.imul(k, c2)
      h ^= k
  }
  h ^= data.length
  h ^= h >>> 16
  h = Math.imul(h, 0x85ebca6b)
  h ^= h >>> 13
  h = Math.imul(h, 0xc2b2ae35)
  h ^= h >>> 16
  return h >>> 0
}

const NUMERIC = /^-?[0-9]+(\.[0-9]+)?$/

/** Number for numbers and numeric strings; null otherwise (booleans are not numbers). */
export const toNum = (v) => (typeof v == 'number' ? v : typeof v == 'string' && NUMERIC.test(v) ? Number(v) : null)

export const str = (x) => {
  if (typeof x == 'string') return x
  if (typeof x == 'boolean') return x ? 'true' : 'false'
  if (typeof x == 'number') return Number.isInteger(x) ? BigInt(x).toString() : String(x)
  return JSON.stringify(x)
}

const eq = (p, v) => (typeof p == 'number' && typeof v == 'number' ? p == v : str(p) == str(v))

const semver = (x) => {
  const m = /^v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?(?:[-+].*)?$/.exec(str(x))
  return m ? [Number(m[1]), Number(m[2] ?? 0), Number(m[3] ?? 0)] : null
}
const cmp3 = (a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2]

export const OPS = ['eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'contains', 'regex', 'is_set', 'semver_gte', 'semver_lt']

const test = (op, p, v) => {
  const missing = p === undefined || p === null
  if (op == 'is_set') return !missing
  if (missing) return op == 'ne' || op == 'not_in'
  switch (op) {
    case 'eq':
      return eq(p, v)
    case 'ne':
      return !eq(p, v)
    case 'in':
      return Array.isArray(v) && v.some((x) => eq(p, x))
    case 'not_in':
      return !(Array.isArray(v) && v.some((x) => eq(p, x)))
    case 'gt':
    case 'gte':
    case 'lt':
    case 'lte': {
      const a = toNum(p)
      const b = toNum(v)
      if (a === null || b === null) return false
      return op == 'gt' ? a > b : op == 'gte' ? a >= b : op == 'lt' ? a < b : a <= b
    }
    case 'contains':
      return str(p).includes(str(v))
    case 'regex':
      try {
        return new RegExp(str(v)).test(str(p))
      } catch {
        return false
      }
    case 'semver_gte':
    case 'semver_lt': {
      const a = semver(p)
      const b = semver(v)
      if (!a || !b) return false
      return op == 'semver_gte' ? cmp3(a, b) >= 0 : cmp3(a, b) < 0
    }
  }
  return false
}

/** `get(prop)` resolves a property reference to a value (undefined = missing). */
export const match = (cond, get) => {
  if (!cond || typeof cond != 'object') return true
  if (Array.isArray(cond.and)) return cond.and.every((c) => match(c, get))
  if (Array.isArray(cond.or)) return cond.or.some((c) => match(c, get))
  if ('not' in cond) return !match(cond.not, get)
  return test(cond.op, get(cond.prop), cond.value)
}

/** Structural validation; returns an error message or null. `ops` limits the operators. */
export const validate = (cond, ops = OPS, depth = 0) => {
  if (!cond || typeof cond != 'object' || Array.isArray(cond)) return 'koşul bir nesne olmalı'
  if (depth > 16) return 'koşul çok derin'
  for (const k of ['and', 'or']) {
    if (k in cond) {
      if (!Array.isArray(cond[k])) return `"${k}" bir dizi olmalı`
      for (const c of cond[k]) {
        const e = validate(c, ops, depth + 1)
        if (e) return e
      }
      return null
    }
  }
  if ('not' in cond) return validate(cond.not, ops, depth + 1)
  if (typeof cond.prop != 'string' || !cond.prop) return '"prop" boş olamaz'
  if (!ops.includes(cond.op)) return `bilinmeyen operatör: ${cond.op}`
  if ((cond.op == 'in' || cond.op == 'not_in') && !Array.isArray(cond.value)) return `"${cond.op}" bir dizi ister`
  if (cond.op == 'regex') {
    try {
      new RegExp(String(cond.value))
    } catch {
      return 'regex derlenemedi: ' + cond.value
    }
  }
  return null
}

/** spec/flags.md §3. Returns null, true or a variant key (or the flag's default as stored). */
export const evalFlag = (flag, props, userId, anonId) => {
  if (!flag.enabled) return flag.default ?? null
  const get = (k) => (k == 'user_id' ? userId || undefined : k == 'anon_id' ? anonId || undefined : props?.[k])
  for (const rule of flag.rules ?? []) {
    if (rule.when && !match(rule.when, get)) continue
    const id = rule.stable_on == 'anon_id' ? anonId : userId ? userId : anonId
    const rollout = rule.rollout ?? 10000
    const b = murmur3(flag.salt + ':' + flag.key + ':' + id) % 10000
    if (b >= rollout) return flag.default ?? null
    if (!rule.variants?.length) return true
    const v = murmur3(flag.salt + ':v:' + flag.key + ':' + id) % 10000
    let cumulative = 0
    for (const variant of rule.variants) {
      cumulative += variant.w
      if (v < cumulative) return variant.key
    }
    return rule.variants[rule.variants.length - 1].key
  }
  return flag.default ?? null
}
