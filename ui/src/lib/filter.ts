// Editable model of the filter expression tree (spec/query.md §2) and of flag
// conditions (spec/flags.md §4), plus the conversion to and from the JSON
// shape. The model keeps half-typed input; `toExpr` only emits complete parts.
import type { Expr } from './types'

export const QUERY_OPS = ['eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'contains', 'is_set'] as const
export const FLAG_OPS = [...QUERY_OPS, 'regex', 'semver_gte', 'semver_lt'] as const
export type Op = (typeof FLAG_OPS)[number]

/** Fixed columns usable as `Ref` (spec/query.md §2). */
export const COLUMNS = [
  'event', 'person_key', 'anon_id', 'user_id', 'session_id', 'platform', 'os', 'os_version', 'device_model',
  'device_type', 'app_version', 'locale', 'country', 'sdk', 'url_path', 'referrer_host', 'screen',
]

export interface CondModel {
  kind: 'cond'
  id: number
  prop: string
  op: Op
  /** raw text; lists are comma separated */
  value: string
}
export interface GroupModel {
  kind: 'group'
  id: number
  join: 'and' | 'or'
  /** wraps the whole group in `not` */
  negate: boolean
  children: FilterModel[]
}
export type FilterModel = CondModel | GroupModel

let nextId = 1
export const newCond = (prop = '', op: Op = 'eq', value = ''): CondModel => ({ kind: 'cond', id: nextId++, prop, op, value })
export const newGroup = (join: 'and' | 'or' = 'and', children: FilterModel[] = [], negate = false): GroupModel => ({
  kind: 'group',
  id: nextId++,
  join,
  negate,
  children,
})

const NUMERIC = /^-?[0-9]+(\.[0-9]+)?$/

/**
 * Text → JSON scalar. Plain digits become a number, `true`/`false` a boolean,
 * everything else a string. Double quotes force a string: `"42"` → "42".
 */
export const parseScalar = (text: string): string | number | boolean => {
  const s = text.trim()
  if (s.length >= 2 && s.startsWith('"') && s.endsWith('"')) return s.slice(1, -1).replace(/\\(["\\])/g, '$1')
  if (NUMERIC.test(s) && Number.isFinite(Number(s)) && String(Number(s)) == s) return Number(s)
  if (s == 'true') return true
  if (s == 'false') return false
  return s
}

/** JSON scalar → text such that `parseScalar` gives the same value back. */
export const formatScalar = (v: unknown): string => {
  if (typeof v == 'number' || typeof v == 'boolean') return String(v)
  if (v == null) return ''
  const s = typeof v == 'string' ? v : JSON.stringify(v)
  const ambiguous = parseScalar(s) !== s || s != s.trim() || s.includes(',') || s == ''
  return ambiguous ? '"' + s.replace(/(["\\])/g, '\\$1') + '"' : s
}

/** Splits on commas outside double quotes. */
export const splitList = (text: string): string[] => {
  const out: string[] = []
  let cur = ''
  let quoted = false
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (c == '\\' && quoted && i + 1 < text.length) cur += c + text[++i]
    else if (c == '"') {
      quoted = !quoted
      cur += c
    } else if (c == ',' && !quoted) {
      out.push(cur)
      cur = ''
    } else cur += c
  }
  out.push(cur)
  return out.map((s) => s.trim()).filter((s) => s != '')
}

const isList = (op: Op) => op == 'in' || op == 'not_in'
export const needsValue = (op: Op) => op != 'is_set'

/** A condition is complete when it has a property and, if the operator takes one, a value. */
export const condComplete = (c: CondModel): boolean =>
  c.prop.trim() != '' && (!needsValue(c.op) || (isList(c.op) ? splitList(c.value).length > 0 : c.value.trim() != ''))

const condToExpr = (c: CondModel): Expr => {
  const prop = c.prop.trim()
  // `value` is ignored for is_set; `true` keeps strict decoders that require the field happy
  if (c.op == 'is_set') return { prop, op: c.op, value: true }
  if (isList(c.op)) return { prop, op: c.op, value: splitList(c.value).map(parseScalar) }
  return { prop, op: c.op, value: parseScalar(c.value) }
}

/**
 * Model → JSON. Incomplete conditions and empty groups are dropped; a group
 * with a single child collapses to that child. Returns undefined for "no filter".
 */
export const toExpr = (m: FilterModel): Expr | undefined => {
  if (m.kind == 'cond') return condComplete(m) ? condToExpr(m) : undefined
  const parts = m.children.map(toExpr).filter((e): e is Expr => e !== undefined)
  if (!parts.length) return undefined
  const body: Expr = parts.length == 1 ? parts[0] : m.join == 'and' ? { and: parts } : { or: parts }
  return m.negate ? { not: body } : body
}

/** Number of conditions the user started but did not finish. */
export const countIncomplete = (m: FilterModel): number =>
  m.kind == 'cond' ? (condComplete(m) || (m.prop.trim() == '' && m.value.trim() == '') ? 0 : 1) : m.children.reduce((n, c) => n + countIncomplete(c), 0)

export const countConds = (m: FilterModel): number =>
  m.kind == 'cond' ? (condComplete(m) ? 1 : 0) : m.children.reduce((n, c) => n + countConds(c), 0)

const KNOWN = new Set<string>(FLAG_OPS)

const fromAny = (e: Expr): FilterModel => {
  if ('and' in e) return newGroup('and', e.and.map(fromAny))
  if ('or' in e) return newGroup('or', e.or.map(fromAny))
  if ('not' in e) {
    const inner = fromAny(e.not)
    if (inner.kind == 'group' && !inner.negate) return { ...inner, negate: true }
    return newGroup('and', [inner], true)
  }
  const op = (KNOWN.has(e.op) ? e.op : 'eq') as Op
  const value = op == 'is_set' ? '' : Array.isArray(e.value) ? e.value.map(formatScalar).join(', ') : formatScalar(e.value)
  return newCond(e.prop, op, value)
}

/** JSON → model. The root is always a group so the editor has somewhere to add rows. */
export const fromExpr = (e: Expr | undefined | null): GroupModel => {
  if (!e) return newGroup()
  const m = fromAny(e)
  return m.kind == 'group' ? m : newGroup('and', [m])
}

// ---- immutable edits used by the editor ----

export const updateNode = (root: GroupModel, id: number, fn: (n: FilterModel) => FilterModel | null): GroupModel => {
  const walk = (n: FilterModel): FilterModel | null => {
    if (n.id == id) return fn(n)
    if (n.kind == 'cond') return n
    return { ...n, children: n.children.map(walk).filter((c): c is FilterModel => c !== null) }
  }
  return (walk(root) as GroupModel | null) ?? newGroup()
}

export const addChild = (root: GroupModel, groupId: number, child: FilterModel): GroupModel =>
  updateNode(root, groupId, (n) => (n.kind == 'group' ? { ...n, children: [...n.children, child] } : n))

/** Short human-readable form, e.g. `platform = ios ve (country ∈ TR, DE)`. */
export const summarize = (e: Expr | undefined, words: { and: string; or: string; not: string; ops: Record<string, string> }): string => {
  if (!e) return ''
  const walk = (x: Expr, top: boolean): string => {
    if ('and' in x || 'or' in x) {
      const list = 'and' in x ? x.and : x.or
      const s = list.map((c) => walk(c, false)).join(' ' + ('and' in x ? words.and : words.or) + ' ')
      return top || list.length < 2 ? s : '(' + s + ')'
    }
    if ('not' in x) return words.not + ' (' + walk(x.not, true) + ')'
    const op = words.ops[x.op] ?? x.op
    if (x.op == 'is_set') return x.prop + ' ' + op
    return x.prop + ' ' + op + ' ' + (Array.isArray(x.value) ? x.value.map(formatScalar).join(', ') : formatScalar(x.value))
  }
  return walk(e, true)
}
