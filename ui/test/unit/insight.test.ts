import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cleanQuery, defaultQuery, requestBody, validateQuery } from '../../src/lib/insight'
import { addItem, moveItem, ordered, pack, removeItem, resizeItem } from '../../src/lib/layout'
import { can } from '../../src/lib/roles'
import type { FunnelQuery, RetentionQuery, TrendsQuery } from '../../src/lib/types'

const NOW = Date.parse('2026-11-18T12:00:00Z')

test('default queries: trends and SQL run as they are, funnel and retention need events', () => {
  assert.equal(validateQuery('trends', defaultQuery('trends'), NOW), null)
  assert.equal(validateQuery('sql', defaultQuery('sql'), NOW), null)
  assert.deepEqual(validateQuery('funnel', defaultQuery('funnel'), NOW), { code: 'step_event', index: 0 })
  assert.deepEqual(validateQuery('retention', defaultQuery('retention'), NOW), { code: 'retention_start' })
})

test('validation catches what the query service would reject', () => {
  const trends: TrendsQuery = { range: '7d', interval: 'day', series: [{ event: 'purchase', agg: 'sum' }] }
  assert.deepEqual(validateQuery('trends', trends, NOW), { code: 'series_prop', index: 0 })
  assert.equal(validateQuery('trends', { ...trends, series: [{ event: 'purchase', agg: 'sum', prop: 'props.amount' }] }, NOW), null)
  assert.deepEqual(validateQuery('trends', { ...trends, series: [] }, NOW), { code: 'series_empty' })
  assert.deepEqual(validateQuery('trends', { interval: 'day', series: [{ event: null, agg: 'count' }], from: '2026-11-18', to: '2026-11-01' }, NOW), { code: 'range', detail: 'order' })
  assert.deepEqual(validateQuery('trends', { interval: 'day', series: [{ event: null, agg: 'count' }], range: '500d' }, NOW), { code: 'range', detail: 'too_long' })
  const funnel: FunnelQuery = { range: '30d', steps: [{ event: 'a' }], window_seconds: 60, order: 'ordered' }
  assert.deepEqual(validateQuery('funnel', funnel, NOW), { code: 'funnel_steps' })
  assert.deepEqual(validateQuery('funnel', { ...funnel, steps: [{ event: 'a' }, { event: null }], window_seconds: 0 }, NOW), { code: 'window' })
  const retention: RetentionQuery = { range: '60d', period: 'week', periods: 65, start: { event: 'signup' }, return: { event: null } }
  assert.deepEqual(validateQuery('retention', retention, NOW), { code: 'periods' })
  assert.deepEqual(validateQuery('sql', { range: '7d', sql: '  ' }, NOW), { code: 'sql_empty' })
})

test('request body: range resolved, UI-only keys and empty optionals removed', () => {
  const body = requestBody(
    'trends',
    {
      range: '7d',
      interval: 'day',
      series: [{ event: '', agg: 'count', label: '  ', prop: 'props.unused' }, { event: 'purchase', agg: 'sum', prop: ' props.amount ', label: ' Ciro ' }],
      breakdown: ' platform ',
      ui: { chart: 'bar' },
    } as TrendsQuery,
    NOW,
  )
  assert.deepEqual(body, {
    from: '2026-11-12',
    to: '2026-11-18',
    interval: 'day',
    series: [{ event: null, agg: 'count' }, { event: 'purchase', agg: 'sum', prop: 'props.amount', label: 'Ciro' }],
    breakdown: 'platform',
    breakdown_limit: 10,
  })
  assert.equal('range' in body, false)
  assert.equal('ui' in body, false)
})

test('stored query keeps the relative range and the chart type', () => {
  const stored = cleanQuery('trends', { range: '30d', interval: 'week', series: [{ event: null, agg: 'unique' }], ui: { chart: 'bar' } } as TrendsQuery)
  assert.deepEqual(stored, { range: '30d', interval: 'week', series: [{ event: null, agg: 'unique' }], ui: { chart: 'bar' } })
  const line = cleanQuery('trends', { range: '30d', interval: 'week', series: [{ event: null, agg: 'unique' }], ui: { chart: 'line' } } as TrendsQuery)
  assert.equal('ui' in line, false, 'the default chart type is not stored')
  const abs = cleanQuery('funnel', { from: '2026-11-01', to: '2026-11-18', steps: [{ event: 'a' }, { event: '' }], window_seconds: 3600, order: 'strict', breakdown: '' })
  assert.deepEqual(abs, { from: '2026-11-01', to: '2026-11-18', steps: [{ event: 'a' }, { event: null }], window_seconds: 3600, order: 'strict' })
  assert.deepEqual(requestBody('retention', { range: '14d', period: 'day', periods: 7, start: { event: 'signup' }, return: { event: '' } }, NOW), {
    from: '2026-11-05',
    to: '2026-11-18',
    period: 'day',
    periods: 7,
    start: { event: 'signup' },
    return: { event: null },
  })
})

test('dashboard layout: pack, reorder, resize, add, remove', () => {
  const item = (id: number, w: number, h: number) => ({ insight_id: id, x: 0, y: 0, w, h })
  const packed = pack([item(1, 8, 3), item(2, 4, 2), item(3, 6, 3), item(4, 12, 4), item(5, 4, 2)])
  assert.deepEqual(packed.map((p) => [p.insight_id, p.x, p.y]), [[1, 0, 0], [2, 8, 0], [3, 0, 3], [4, 0, 6], [5, 0, 10]])
  assert.deepEqual(ordered([...packed].reverse()).map((p) => p.insight_id), [1, 2, 3, 4, 5], 'reading order is recovered from x/y')
  assert.deepEqual(moveItem(packed, 1, -1).map((p) => [p.insight_id, p.x, p.y]).slice(0, 2), [[2, 0, 0], [1, 4, 0]])
  assert.equal(moveItem(packed, 0, -1), packed, 'moving past the edge is a no-op')
  assert.deepEqual(resizeItem(packed, 0, { w: 12 }).map((p) => [p.insight_id, p.x, p.y]).slice(0, 2), [[1, 0, 0], [2, 0, 3]])
  assert.deepEqual(addItem(packed, 9).at(-1), { insight_id: 9, x: 4, y: 10, w: 6, h: 3 })
  assert.deepEqual(removeItem(packed, 0).map((p) => [p.insight_id, p.x, p.y])[0], [2, 0, 0])
  assert.deepEqual(pack([item(1, 99, 0)])[0], { insight_id: 1, x: 0, y: 0, w: 12, h: 3 }, 'out-of-range sizes are clamped')
})

test('role table', () => {
  const matrix: Record<string, boolean[]> = {
    //            read   insight admin  owner
    viewer: [true, false, false, false],
    member: [true, true, false, false],
    admin: [true, true, true, false],
    owner: [true, true, true, true],
  }
  for (const [role, expect] of Object.entries(matrix)) {
    assert.deepEqual((['read', 'insight.write', 'admin', 'owner'] as const).map((a) => can(role as 'viewer', a)), expect, role)
  }
  assert.equal(can(undefined, 'read'), false)
  assert.equal(can('superuser' as 'owner', 'read'), false)
})
