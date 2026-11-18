// Flag editor form: model, conversion to/from the API shape, validation
// (spec/control.md §5, spec/flags.md).
import { countIncomplete, fromExpr, toExpr, type CondModel, type FilterModel, type GroupModel } from './filter'
import type { Flag, FlagDefault, Rule } from './types'

export const KEY_PATTERN = /^[a-z0-9][a-z0-9_-]{0,63}$/
export const TOTAL_BP = 10000

export interface VariantForm {
  key: string
  /** percent as typed, e.g. "33.34" */
  pct: string
}
export interface RuleForm {
  id: number
  when: GroupModel
  rolloutPct: string
  variants: VariantForm[]
  stableOnAnon: boolean
}
export interface FlagForm {
  key: string
  name: string
  description: string
  enabled: boolean
  /** `off` → null, `on` → true, `variant` → the string in defaultVariant */
  defaultKind: 'off' | 'on' | 'variant'
  defaultVariant: string
  rules: RuleForm[]
}

let nextRule = 1

/** "12.5" → 1250 basis points; null when not a number with ≤ 2 decimals in 0–100. */
export const pctToBp = (text: string): number | null => {
  const s = text.trim().replace(',', '.')
  if (!/^[0-9]{1,3}(\.[0-9]{1,2})?$/.test(s)) return null
  const bp = Math.round(Number(s) * 100)
  return bp >= 0 && bp <= TOTAL_BP ? bp : null
}

export const bpToPct = (bp: number): string => String(Math.round(bp) / 100)

/** Even split in basis points that sums to exactly 10000 (remainder goes to the last). */
export const evenWeights = (n: number): number[] => {
  if (n <= 0) return []
  const base = Math.floor(TOTAL_BP / n)
  return Array.from({ length: n }, (_, i) => (i == n - 1 ? TOTAL_BP - base * (n - 1) : base))
}

export const newRule = (): RuleForm => ({ id: nextRule++, when: fromExpr(undefined), rolloutPct: '100', variants: [], stableOnAnon: false })

export const emptyFlagForm = (): FlagForm => ({
  key: '',
  name: '',
  description: '',
  enabled: false,
  defaultKind: 'off',
  defaultVariant: '',
  rules: [newRule()],
})

export const flagToForm = (flag: Pick<Flag, 'key' | 'name' | 'description' | 'enabled' | 'rules' | 'default'>): FlagForm => ({
  key: flag.key,
  name: flag.name ?? '',
  description: flag.description ?? '',
  enabled: !!flag.enabled,
  // `false` is equivalent to null (spec/flags.md §2)
  defaultKind: typeof flag.default == 'string' ? 'variant' : flag.default === true ? 'on' : 'off',
  defaultVariant: typeof flag.default == 'string' ? flag.default : '',
  rules: (flag.rules ?? []).map((r) => ({
    id: nextRule++,
    when: fromExpr(r.when),
    rolloutPct: bpToPct(r.rollout ?? TOTAL_BP),
    variants: (r.variants ?? []).map((v) => ({ key: v.key, pct: bpToPct(v.w) })),
    stableOnAnon: r.stable_on == 'anon_id',
  })),
})

export interface FlagPayload {
  key: string
  name: string
  description: string
  enabled: boolean
  rules: Rule[]
  default: FlagDefault
}

/** Form → request body. Call only after `validateFlagForm` returned no errors. */
export const formToFlag = (form: FlagForm): FlagPayload => ({
  key: form.key.trim(),
  name: form.name.trim(),
  description: form.description.trim(),
  enabled: form.enabled,
  default: form.defaultKind == 'variant' ? form.defaultVariant.trim() : form.defaultKind == 'on' ? true : null,
  rules: form.rules.map((r) => {
    const rule: Rule = {}
    const when = toExpr(r.when)
    if (when) rule.when = when
    rule.rollout = pctToBp(r.rolloutPct) ?? TOTAL_BP
    if (r.variants.length) rule.variants = r.variants.map((v) => ({ key: v.key.trim(), w: pctToBp(v.pct) ?? 0 }))
    if (r.stableOnAnon) rule.stable_on = 'anon_id'
    return rule
  }),
})

export type FlagErrorCode =
  | 'key_pattern'
  | 'default_variant_empty'
  | 'rollout_range'
  | 'variant_key_empty'
  | 'variant_key_duplicate'
  | 'variant_weight'
  | 'weights_sum'
  | 'condition_incomplete'
  | 'regex_invalid'

export interface FlagError {
  /** `key`, `default`, or `rules.<index>.<part>` */
  field: string
  code: FlagErrorCode
  /** for weights_sum: the actual total in basis points */
  total?: number
}

const conds = (m: FilterModel): CondModel[] => (m.kind == 'cond' ? [m] : m.children.flatMap(conds))

export const validateFlagForm = (form: FlagForm): FlagError[] => {
  const errors: FlagError[] = []
  if (!KEY_PATTERN.test(form.key.trim())) errors.push({ field: 'key', code: 'key_pattern' })
  if (form.defaultKind == 'variant' && form.defaultVariant.trim() == '') errors.push({ field: 'default', code: 'default_variant_empty' })
  form.rules.forEach((r, i) => {
    const at = 'rules.' + i
    if (pctToBp(r.rolloutPct) === null) errors.push({ field: at + '.rollout', code: 'rollout_range' })
    if (countIncomplete(r.when) > 0) errors.push({ field: at + '.when', code: 'condition_incomplete' })
    for (const c of conds(r.when)) {
      if (c.op != 'regex' || c.value.trim() == '') continue
      try {
        new RegExp(c.value)
      } catch {
        errors.push({ field: at + '.when', code: 'regex_invalid' })
      }
    }
    if (!r.variants.length) return
    const keys = r.variants.map((v) => v.key.trim())
    if (keys.some((k) => k == '')) errors.push({ field: at + '.variants', code: 'variant_key_empty' })
    else if (new Set(keys).size != keys.length) errors.push({ field: at + '.variants', code: 'variant_key_duplicate' })
    const weights = r.variants.map((v) => pctToBp(v.pct))
    if (weights.some((w) => w === null)) errors.push({ field: at + '.variants', code: 'variant_weight' })
    else {
      const total = weights.reduce<number>((a, w) => a + (w ?? 0), 0)
      if (total != TOTAL_BP) errors.push({ field: at + '.variants', code: 'weights_sum', total })
    }
  })
  return errors
}

/** Sum of the variant weights of one rule in basis points (invalid entries count as 0). */
export const weightTotal = (variants: VariantForm[]): number => variants.reduce((a, v) => a + (pctToBp(v.pct) ?? 0), 0)
