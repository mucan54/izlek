import assert from 'node:assert/strict'
import { test } from 'node:test'
import { newCond } from '../../src/lib/filter'
import { KEY_PATTERN, bpToPct, emptyFlagForm, evenWeights, flagToForm, formToFlag, newRule, pctToBp, validateFlagForm, weightTotal, type FlagForm } from '../../src/lib/flagform'
import type { Rule } from '../../src/lib/types'

const codes = (form: FlagForm) => validateFlagForm(form).map((e) => `${e.field}:${e.code}`)
const valid = (patch: Partial<FlagForm> = {}): FlagForm => ({ ...emptyFlagForm(), key: 'new_checkout', ...patch })

test('key pattern', () => {
  for (const ok of ['a', '0', 'new_checkout', 'a-b_c-9', 'x'.repeat(64)]) assert.ok(KEY_PATTERN.test(ok), ok)
  for (const bad of ['', '_a', '-a', 'New', 'a b', 'ş', 'a.b', 'x'.repeat(65)]) assert.ok(!KEY_PATTERN.test(bad), bad)
  assert.deepEqual(codes(valid()), [])
  assert.deepEqual(codes(valid({ key: 'Bad Key' })), ['key:key_pattern'])
  assert.deepEqual(codes(valid({ key: '  spaced_ok  ' })), [], 'surrounding spaces are trimmed')
})

test('percent ↔ basis points', () => {
  assert.equal(pctToBp('25'), 2500)
  assert.equal(pctToBp('33.34'), 3334)
  assert.equal(pctToBp('33,33'), 3333, 'decimal comma')
  assert.equal(pctToBp('0'), 0)
  assert.equal(pctToBp('100'), 10000)
  assert.equal(pctToBp('0.07'), 7)
  for (const bad of ['', 'abc', '-1', '100.01', '12.345', '1e2', '101']) assert.equal(pctToBp(bad), null, bad)
  assert.equal(bpToPct(3334), '33.34')
  assert.equal(bpToPct(10000), '100')
  assert.equal(bpToPct(7), '0.07')
})

test('variant weights must sum to 100%', () => {
  const rule = (pcts: string[]) => ({ ...newRule(), variants: pcts.map((pct, i) => ({ key: 'v' + i, pct })) })
  assert.deepEqual(codes(valid({ rules: [rule(['50', '50'])] })), [])
  assert.deepEqual(codes(valid({ rules: [rule(['33.33', '33.33', '33.34'])] })), [])
  assert.deepEqual(codes(valid({ rules: [rule(['100'])] })), [])
  assert.deepEqual(codes(valid({ rules: [rule([])] })), [], 'no variants is fine: the flag is boolean')

  const off = validateFlagForm(valid({ rules: [rule(['50', '40'])] }))
  assert.deepEqual(off.map((e) => [e.field, e.code, e.total]), [['rules.0.variants', 'weights_sum', 9000]])
  assert.deepEqual(codes(valid({ rules: [rule(['33.33', '33.33', '33.33'])] })), ['rules.0.variants:weights_sum'], '99.99 is not 100')
  assert.deepEqual(codes(valid({ rules: [rule(['60', '60'])] })), ['rules.0.variants:weights_sum'])
  assert.deepEqual(codes(valid({ rules: [rule(['50', 'x'])] })), ['rules.0.variants:variant_weight'])
  // errors point at the right rule
  assert.deepEqual(codes(valid({ rules: [rule(['50', '50']), rule(['10'])] })), ['rules.1.variants:weights_sum'])
  assert.equal(weightTotal(rule(['12.5', '87.5']).variants), 10000)
})

test('even split always sums to exactly 10000', () => {
  for (let n = 1; n <= 12; n++) {
    const w = evenWeights(n)
    assert.equal(w.length, n)
    assert.equal(w.reduce((a, b) => a + b, 0), 10000, `n=${n}`)
    assert.ok(Math.max(...w) - Math.min(...w) <= n, 'near-even')
  }
  assert.deepEqual(evenWeights(3), [3333, 3333, 3334])
  assert.deepEqual(evenWeights(0), [])
})

test('variant keys, rollout, default and conditions', () => {
  const r = newRule()
  assert.deepEqual(codes(valid({ rules: [{ ...r, variants: [{ key: 'a', pct: '50' }, { key: ' ', pct: '50' }] }] })), ['rules.0.variants:variant_key_empty'])
  assert.deepEqual(codes(valid({ rules: [{ ...r, variants: [{ key: 'a', pct: '50' }, { key: 'a', pct: '50' }] }] })), ['rules.0.variants:variant_key_duplicate'])
  assert.deepEqual(codes(valid({ rules: [{ ...r, rolloutPct: '120' }] })), ['rules.0.rollout:rollout_range'])
  assert.deepEqual(codes(valid({ rules: [{ ...r, rolloutPct: '' }] })), ['rules.0.rollout:rollout_range'])
  assert.deepEqual(codes(valid({ defaultKind: 'variant', defaultVariant: '' })), ['default:default_variant_empty'])

  const half = { ...r, when: { ...r.when, children: [newCond('platform', 'eq', '')] } }
  assert.deepEqual(codes(valid({ rules: [half] })), ['rules.0.when:condition_incomplete'])
  const badRegex = { ...r, when: { ...r.when, children: [newCond('email', 'regex', '([a-z')] } }
  assert.deepEqual(codes(valid({ rules: [badRegex] })), ['rules.0.when:regex_invalid'])
  const goodRegex = { ...r, when: { ...r.when, children: [newCond('email', 'regex', '@example\\.com$')] } }
  assert.deepEqual(codes(valid({ rules: [goodRegex] })), [])
})

test('API shape ↔ form round trip (the example from spec/flags.md)', () => {
  const rules: Rule[] = [
    {
      when: { and: [{ prop: 'platform', op: 'eq', value: 'ios' }, { prop: 'app_version', op: 'semver_gte', value: '3.2.0' }] },
      rollout: 2500,
      variants: [{ key: 'control', w: 5000 }, { key: 'b', w: 5000 }],
      stable_on: 'anon_id',
    },
    { rollout: 10000 },
  ]
  const flag = { key: 'new_checkout', name: 'Yeni ödeme', description: '', enabled: true, rules, default: null }
  const form = flagToForm(flag)
  assert.deepEqual(validateFlagForm(form), [])
  assert.deepEqual(formToFlag(form), flag)
  assert.equal(form.rules[0].rolloutPct, '25')
  assert.equal(form.rules[0].stableOnAnon, true)
})

test('defaults: null, true and a variant; false is the same as null', () => {
  const base = { key: 'k', name: '', description: '', enabled: false, rules: [] }
  assert.equal(formToFlag(flagToForm({ ...base, default: null })).default, null)
  assert.equal(formToFlag(flagToForm({ ...base, default: false })).default, null)
  assert.equal(formToFlag(flagToForm({ ...base, default: true })).default, true)
  assert.equal(formToFlag(flagToForm({ ...base, default: 'legacy' })).default, 'legacy')
  // a rule without rollout means 100%
  assert.equal(formToFlag(flagToForm({ ...base, default: null, rules: [{}] })).rules[0].rollout, 10000)
})
