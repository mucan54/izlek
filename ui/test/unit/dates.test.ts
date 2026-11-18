import assert from 'node:assert/strict'
import { test } from 'node:test'
import { boundsOf, checkBounds, parseRelative, resolveBounds, setBounds, spanDays, utcDay, withBounds } from '../../src/lib/dates'

const NOW = Date.parse('2026-11-18T14:30:20.500Z')

test('relative day ranges include today (UTC)', () => {
  assert.deepEqual(resolveBounds({ range: '7d' }, NOW), { from: '2026-11-12', to: '2026-11-18' })
  assert.deepEqual(resolveBounds({ range: '1d' }, NOW), { from: '2026-11-18', to: '2026-11-18' })
  assert.deepEqual(resolveBounds({ range: '30d' }, NOW), { from: '2026-10-20', to: '2026-11-18' })
  assert.deepEqual(resolveBounds({ range: '90d' }, NOW), { from: '2026-08-21', to: '2026-11-18' })
  // crosses a year and a leap day
  assert.deepEqual(resolveBounds({ range: '60d' }, Date.parse('2028-03-01T00:00:00Z')), { from: '2028-01-02', to: '2028-03-01' })
})

test('the UTC day is used, not the local one', () => {
  assert.equal(utcDay(Date.parse('2026-11-18T23:59:59.999Z')), '2026-11-18')
  assert.equal(utcDay(Date.parse('2026-11-19T00:00:00.000Z')), '2026-11-19')
  assert.deepEqual(resolveBounds({ range: '1d' }, Date.parse('2026-11-18T23:59:59.999Z')), { from: '2026-11-18', to: '2026-11-18' })
})

test('hour ranges are RFC 3339 instants snapped to the minute', () => {
  assert.deepEqual(resolveBounds({ range: '24h' }, NOW), { from: '2026-11-17T14:31:00Z', to: '2026-11-18T14:31:00Z' })
  assert.deepEqual(resolveBounds({ range: '1h' }, NOW), { from: '2026-11-18T13:31:00Z', to: '2026-11-18T14:31:00Z' })
  // two calls in the same minute resolve identically (result cache key)
  assert.deepEqual(resolveBounds({ range: '1h' }, NOW + 30_000), resolveBounds({ range: '1h' }, NOW))
})

test('concrete bounds pass through; garbage ranges fall back to them', () => {
  assert.deepEqual(resolveBounds({ from: '2026-01-01', to: '2026-01-31' }, NOW), { from: '2026-01-01', to: '2026-01-31' })
  assert.deepEqual(resolveBounds({ range: 'soon', from: '2026-01-01', to: '2026-01-31' }, NOW), { from: '2026-01-01', to: '2026-01-31' })
  assert.equal(parseRelative('7d')?.n, 7)
  assert.equal(parseRelative('0d'), null)
  assert.equal(parseRelative('7w'), null)
  assert.equal(parseRelative('d'), null)
})

test('withBounds replaces range and keeps the rest of the query', () => {
  const body = withBounds({ range: '7d', interval: 'day', series: [{ event: null, agg: 'count' }] }, NOW)
  assert.deepEqual(body, { interval: 'day', series: [{ event: null, agg: 'count' }], from: '2026-11-12', to: '2026-11-18' })
  assert.equal('range' in body, false)
})

test('bounds validation', () => {
  assert.equal(checkBounds({ from: '2026-11-01', to: '2026-11-18' }), null)
  assert.equal(checkBounds({ from: '2026-11-18', to: '2026-11-18' }), null, 'a single inclusive day is fine')
  assert.equal(checkBounds({ from: '2026-11-19', to: '2026-11-18' }), 'order')
  assert.equal(checkBounds({ from: '', to: '2026-11-18' }), 'invalid')
  assert.equal(checkBounds({ from: '2026-13-01', to: '2026-11-18' }), 'invalid')
  assert.equal(checkBounds({ from: '2025-10-15', to: '2026-11-18' }), null, '400 days exactly')
  assert.equal(checkBounds({ from: '2025-10-14', to: '2026-11-18' }), 'too_long')
  assert.equal(checkBounds({ from: '2026-11-18T10:00:00Z', to: '2026-11-18T10:00:00Z' }), 'order', 'timestamp `to` is exclusive')
  assert.equal(spanDays({ from: '2026-11-12', to: '2026-11-18' }), 7)
})

test('boundsOf / setBounds swap the time part only', () => {
  const q = { range: '7d', sql: 'select 1' }
  assert.deepEqual(boundsOf(q), { range: '7d' })
  assert.deepEqual(setBounds(q, { from: '2026-01-01', to: '2026-01-02' }), { sql: 'select 1', from: '2026-01-01', to: '2026-01-02' })
  assert.deepEqual(setBounds({ from: 'a', to: 'b', sql: 's' }, { range: '30d' }), { sql: 's', range: '30d' })
})
