import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  addChild, condComplete, countConds, countIncomplete, formatScalar, fromExpr, newCond, newGroup, parseScalar, splitList, summarize, toExpr, updateNode,
} from '../../src/lib/filter'
import type { Expr } from '../../src/lib/types'

const roundTrip = (e: Expr) => assert.deepEqual(toExpr(fromExpr(e)), e)

test('JSON → model → JSON is lossless for well-formed expressions', () => {
  roundTrip({ and: [{ prop: 'platform', op: 'eq', value: 'ios' }, { prop: 'props.amount', op: 'gte', value: 12.5 }] })
  roundTrip({ or: [{ prop: 'country', op: 'in', value: ['TR', 'DE'] }, { prop: 'person.plan', op: 'ne', value: 'free' }] })
  roundTrip({ not: { and: [{ prop: 'event', op: 'eq', value: 'purchase' }, { prop: 'props.coupon', op: 'is_set', value: true }] } })
  roundTrip({
    and: [
      { prop: 'platform', op: 'eq', value: 'ios' },
      { or: [{ prop: 'app_version', op: 'semver_gte', value: '3.2.0' }, { not: { or: [{ prop: 'a', op: 'lt', value: 1 }, { prop: 'b', op: 'contains', value: 'x y' }] } }] },
    ],
  })
  // every operator, including the flag-only ones
  for (const op of ['eq', 'ne', 'gt', 'gte', 'lt', 'lte', 'contains', 'regex', 'semver_gte', 'semver_lt']) roundTrip({ and: [{ prop: 'p', op, value: 'v1' }, { prop: 'q', op, value: 2 }] })
  for (const op of ['in', 'not_in']) roundTrip({ and: [{ prop: 'p', op, value: ['a', 2, true] }, { prop: 'q', op, value: ['x'] }] })
})

test('scalar types survive: numbers, booleans, numeric-looking strings, commas, quotes', () => {
  roundTrip({ and: [{ prop: 'a', op: 'eq', value: 42 }, { prop: 'b', op: 'eq', value: '42' }] })
  roundTrip({ and: [{ prop: 'a', op: 'eq', value: true }, { prop: 'b', op: 'eq', value: 'true' }] })
  roundTrip({ and: [{ prop: 'a', op: 'eq', value: 'İstanbul, TR' }, { prop: 'b', op: 'in', value: ['a,b', 'c "d"', ' pad '] }] })
  roundTrip({ and: [{ prop: 'a', op: 'eq', value: -0.5 }, { prop: 'b', op: 'eq', value: '3.0' }] })
  assert.equal(parseScalar('42'), 42)
  assert.equal(parseScalar('"42"'), '42')
  assert.equal(parseScalar('3.0'), '3.0', 'not canonical → stays text')
  assert.equal(parseScalar(' ios '), 'ios')
  assert.equal(parseScalar('false'), false)
  assert.equal(formatScalar('42'), '"42"')
  assert.equal(formatScalar(42), '42')
  assert.equal(formatScalar('ios'), 'ios')
  assert.deepEqual(splitList('a, "b,c" ,, 3'), ['a', '"b,c"', '3'])
})

test('a single condition or a negated condition keeps its shape', () => {
  roundTrip({ prop: 'event', op: 'eq', value: 'signup' })
  roundTrip({ not: { prop: 'plan', op: 'is_set', value: true } })
  // a one-element group collapses to its child
  assert.deepEqual(toExpr(fromExpr({ and: [{ prop: 'a', op: 'eq', value: 1 }] })), { prop: 'a', op: 'eq', value: 1 })
})

test('is_set ignores the value and always emits one', () => {
  const m = fromExpr({ prop: 'plan', op: 'is_set' })
  assert.deepEqual(toExpr(m), { prop: 'plan', op: 'is_set', value: true })
  assert.equal(condComplete(newCond('plan', 'is_set', '')), true)
})

test('incomplete rows are kept in the model but not emitted', () => {
  const root = newGroup('and', [newCond('platform', 'eq', 'ios'), newCond('country', 'eq', ''), newCond('', 'eq', ''), newCond('x', 'in', ' , ')])
  assert.deepEqual(toExpr(root), { prop: 'platform', op: 'eq', value: 'ios' })
  assert.equal(countConds(root), 1)
  assert.equal(countIncomplete(root), 2, 'blank rows are not counted as mistakes')
  assert.equal(toExpr(newGroup()), undefined)
  assert.equal(toExpr(newGroup('or', [newGroup('and', []), newCond()])), undefined)
  assert.equal(toExpr(newGroup('and', [], true)), undefined, 'not of nothing is nothing')
})

test('empty / missing input gives an empty root group', () => {
  for (const e of [undefined, null]) {
    const m = fromExpr(e)
    assert.equal(m.kind, 'group')
    assert.equal(m.children.length, 0)
  }
})

test('unknown operators degrade to eq instead of breaking the editor', () => {
  const m = fromExpr({ prop: 'a', op: 'between', value: 1 })
  assert.deepEqual(toExpr(m), { prop: 'a', op: 'eq', value: 1 })
})

test('immutable edits', () => {
  const a = newCond('a', 'eq', '1')
  const inner = newGroup('or', [newCond('b', 'eq', '2')])
  const root = newGroup('and', [a, inner])
  const added = addChild(root, inner.id, newCond('c', 'eq', '3'))
  assert.deepEqual(toExpr(added), { and: [{ prop: 'a', op: 'eq', value: 1 }, { or: [{ prop: 'b', op: 'eq', value: 2 }, { prop: 'c', op: 'eq', value: 3 }] }] })
  assert.equal(inner.children.length, 1, 'the original is untouched')
  const removed = updateNode(added, a.id, () => null)
  assert.deepEqual(toExpr(removed), { or: [{ prop: 'b', op: 'eq', value: 2 }, { prop: 'c', op: 'eq', value: 3 }] })
  const flipped = updateNode(root, inner.id, (n) => ({ ...n, negate: true }) as typeof inner)
  assert.deepEqual(toExpr(flipped), { and: [{ prop: 'a', op: 'eq', value: 1 }, { not: { prop: 'b', op: 'eq', value: 2 } }] })
})

test('summaries read like a sentence', () => {
  const words = { and: 've', or: 'veya', not: 'değil', ops: { eq: '=', in: '∈', is_set: 'tanımlı' } }
  assert.equal(
    summarize({ and: [{ prop: 'platform', op: 'eq', value: 'ios' }, { or: [{ prop: 'country', op: 'in', value: ['TR', 'DE'] }, { not: { prop: 'plan', op: 'is_set' } }] }] }, words),
    'platform = ios ve (country ∈ TR, DE veya değil (plan tanımlı))',
  )
  assert.equal(summarize(undefined, words), '')
})
