import assert from 'node:assert/strict'
import { test } from 'node:test'
import { axisLabels, band, compact, heat, nearestIndex, niceAxis, niceStep, pickLabels, scaleLinear, SEQUENTIAL } from '../../src/lib/chart'

test('niceStep picks 1 / 2 / 2.5 / 5 × 10^k', () => {
  assert.equal(niceStep(100, 5), 20)
  assert.equal(niceStep(23, 5), 5)
  assert.equal(niceStep(7, 5), 2)
  assert.equal(niceStep(1, 5), 0.2)
  assert.equal(niceStep(12000, 5), 2500)
  assert.equal(niceStep(0.03, 3), 0.01)
  assert.equal(niceStep(0, 5), 1)
})

test('niceAxis covers the data with round ticks and includes zero', () => {
  assert.deepEqual(niceAxis(0, 23).ticks, [0, 5, 10, 15, 20, 25])
  assert.deepEqual(niceAxis(3, 97).ticks, [0, 20, 40, 60, 80, 100])
  assert.deepEqual(niceAxis(0, 100).ticks, [0, 20, 40, 60, 80, 100], 'a max on a tick does not add another one')
  assert.deepEqual(niceAxis(-12, 30).ticks, [-20, -10, 0, 10, 20, 30])
  const a = niceAxis(0, 14321)
  assert.deepEqual([a.min, a.max, a.step], [0, 15000, 5000])
  assert.deepEqual(a.ticks, [0, 5000, 10000, 15000])
})

test('niceAxis edge cases: empty, flat, fractional, integer-only', () => {
  assert.deepEqual(niceAxis(0, 0).ticks, [0, 0.2, 0.4, 0.6, 0.8, 1])
  assert.deepEqual(niceAxis(0, 0, 5, { integer: true }).ticks, [0, 1])
  assert.deepEqual(niceAxis(0, 3, 5, { integer: true }).ticks, [0, 1, 2, 3], 'counts never get 0.5 ticks')
  assert.deepEqual(niceAxis(0, 0.37).ticks, [0, 0.1, 0.2, 0.3, 0.4], 'no floating-point dust in tick values')
  assert.deepEqual(niceAxis(NaN, Infinity).ticks, [0, 0.2, 0.4, 0.6, 0.8, 1])
  assert.deepEqual(niceAxis(40, 60, 5, { zero: false }).ticks, [40, 45, 50, 55, 60])
  assert.deepEqual(niceAxis(9, 2).ticks, niceAxis(2, 9).ticks, 'swapped bounds')
})

test('scaleLinear maps the domain onto pixels', () => {
  const y = scaleLinear(0, 100, 200, 0)
  assert.equal(y(0), 200)
  assert.equal(y(100), 0)
  assert.equal(y(25), 150)
  assert.equal(scaleLinear(5, 5, 10, 90)(5), 10, 'zero-width domain')
})

test('bands, label thinning and hit testing', () => {
  const b = band(4, 100, 500)
  assert.equal(b.step, 100)
  assert.deepEqual([0, 1, 2, 3].map(b.center), [150, 250, 350, 450])
  assert.deepEqual(pickLabels(7, 10), [0, 1, 2, 3, 4, 5, 6])
  assert.deepEqual(pickLabels(30, 8), [0, 4, 8, 12, 16, 20, 24, 28])
  assert.deepEqual(pickLabels(0, 8), [])
  assert.ok(pickLabels(365, 9).length <= 9)
  assert.equal(nearestIndex(100, 4, 100, 500), 0)
  assert.equal(nearestIndex(260, 4, 100, 500), 1)
  assert.equal(nearestIndex(499, 4, 100, 500), 3)
  assert.equal(nearestIndex(-50, 4, 100, 500), 0)
  assert.equal(nearestIndex(9999, 4, 100, 500), 3)
  assert.equal(nearestIndex(50, 1, 0, 100), 0)
})

test('compact numbers and one style per axis', () => {
  const tr = { k: ' B', m: ' Mn', b: ' Mr' }
  assert.equal(compact(950, tr, ','), '950')
  assert.equal(compact(9999, tr, ','), '9999')
  assert.equal(compact(12500, tr, ','), '12,5 B')
  assert.equal(compact(250000, tr, ','), '250 B')
  assert.equal(compact(3400000, tr, ','), '3,4 Mn')
  assert.equal(compact(2e9, tr, ','), '2 Mr')
  assert.equal(compact(0.25, tr, ','), '0,25')
  assert.equal(compact(-12500), '-12.5K')
  assert.deepEqual(axisLabels([0, 2000, 4000, 6000], tr, ','), ['0', '2000', '4000', '6000'])
  assert.deepEqual(axisLabels([0, 5000, 10000, 15000], tr, ','), ['0', '5 B', '10 B', '15 B'], 'never 5000 next to 10 B')
  assert.deepEqual(axisLabels([0, 0.5, 1], tr, ','), ['0', '0,5', '1'])
})

test('heat ramp: monotone, clamped, readable text', () => {
  assert.equal(heat(0).index, 0)
  assert.equal(heat(1).index, SEQUENTIAL.length - 1)
  assert.equal(heat(-3).index, 0)
  assert.equal(heat(7).index, SEQUENTIAL.length - 1)
  assert.equal(heat(NaN).index, 0)
  let prev = -1
  for (let r = 0; r <= 1.0001; r += 0.05) {
    const h = heat(r)
    assert.ok(h.index >= prev)
    prev = h.index
  }
  assert.equal(heat(0.1).lightText, false)
  assert.equal(heat(0.9).lightText, true)
})
