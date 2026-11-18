import assert from 'node:assert/strict'
import { test } from 'node:test'
import { ApiError, CSRF_HEADER, asList, createApi } from '../../src/lib/api'
import { fakeFetch, json } from './helpers'

const headers = (init: RequestInit) => init.headers as Record<string, string>

test('CSRF header is sent on mutating requests only, once the token is known', async () => {
  const f = fakeFetch([json(200, {}), json(200, {}), json(200, {}), json(200, {}), json(200, {}), json(200, {})])
  const api = createApi({ fetch: f.fn })
  await api.post('/api/auth/login', { email: 'a', password: 'b' })
  assert.equal(headers(f.calls[0].init)[CSRF_HEADER], undefined, 'no token yet')

  api.setCsrf('tok')
  await api.get('/api/me')
  await api.post('/api/projects', { name: 'x' })
  await api.put('/api/x', {})
  await api.patch('/api/x', {})
  await api.del('/api/x')
  assert.equal(headers(f.calls[1].init)[CSRF_HEADER], undefined, 'GET carries no CSRF header')
  for (const i of [2, 3, 4, 5]) assert.equal(headers(f.calls[i].init)[CSRF_HEADER], 'tok', `${f.calls[i].init.method} carries it`)
  assert.deepEqual(f.calls.map((c) => c.init.method), ['POST', 'GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
  assert.equal(f.calls[2].init.credentials, 'same-origin')
  assert.equal(f.calls[2].init.body, JSON.stringify({ name: 'x' }))
  assert.equal(headers(f.calls[2].init)['Content-Type'], 'application/json')
  assert.equal(f.calls[5].init.body, undefined, 'DELETE has no body')

  api.setCsrf(null)
  const g = fakeFetch([json(200, {})])
  const api2 = createApi({ fetch: g.fn })
  api2.setCsrf('a')
  api2.setCsrf(null)
  await api2.post('/api/x')
  assert.equal(headers(g.calls[0].init)[CSRF_HEADER], undefined, 'cleared token is not sent')
})

test('401 reports a lost session, except on credential checks', async () => {
  let lost = 0
  const f = fakeFetch([
    json(401, { error: { code: 'unauthorized', message: 'Oturum açmanız gerekiyor' } }),
    json(401, { error: { code: 'invalid_credentials', message: 'E-posta ya da parola hatalı' } }),
    json(401, { error: { code: 'totp_required', message: 'Doğrulama kodu gerekli' } }),
    json(403, { error: { code: 'forbidden', message: 'Yetkiniz yok' } }),
  ])
  const api = createApi({ fetch: f.fn, onUnauthorized: () => lost++ })
  await assert.rejects(api.get('/api/projects'), (e: unknown) => e instanceof ApiError && e.status == 401 && e.code == 'unauthorized')
  assert.equal(lost, 1)
  await assert.rejects(api.post('/api/auth/login', {}, { credentialCheck: true }), (e: unknown) => e instanceof ApiError && e.code == 'invalid_credentials')
  await assert.rejects(api.post('/api/auth/login', {}, { credentialCheck: true }), (e: unknown) => e instanceof ApiError && e.code == 'totp_required')
  assert.equal(lost, 1, 'login failures are not a lost session')
  await assert.rejects(api.get('/api/x'), (e: unknown) => e instanceof ApiError && e.status == 403)
  assert.equal(lost, 1, '403 is not a lost session')
})

test('errors map to ApiError with the API message, or a fallback', async () => {
  const f = fakeFetch([
    json(400, { error: { code: 'bad_request', message: 'Tarih aralığı en çok 400 gün olabilir' } }),
    new Response('<html>Bad Gateway</html>', { status: 502 }),
    json(500, { error: {} }),
    new TypeError('fetch failed'),
    new Response('not json', { status: 200 }),
  ])
  const api = createApi({ fetch: f.fn, fallbackMessage: (s) => (s ? `HTTP-${s}` : 'AĞ') })
  const fail = async (p: Promise<unknown>): Promise<ApiError> => {
    try {
      await p
    } catch (e) {
      assert.ok(e instanceof ApiError)
      return e
    }
    throw new Error('expected a rejection')
  }
  let e = await fail(api.post('/q', {}))
  assert.deepEqual([e.status, e.code, e.message], [400, 'bad_request', 'Tarih aralığı en çok 400 gün olabilir'])
  e = await fail(api.get('/x'))
  assert.deepEqual([e.status, e.code, e.message], [502, 'http_502', 'HTTP-502'])
  e = await fail(api.get('/x'))
  assert.deepEqual([e.status, e.code, e.message], [500, 'http_500', 'HTTP-500'])
  e = await fail(api.get('/x'))
  assert.deepEqual([e.status, e.code, e.message], [0, 'network', 'AĞ'])
  e = await fail(api.get('/x'))
  assert.equal(e.code, 'bad_response')
})

test('aborted requests reject with AbortError, not ApiError', async () => {
  const abort = new Error('aborted')
  abort.name = 'AbortError'
  const api = createApi({ fetch: fakeFetch([abort]).fn })
  await assert.rejects(api.get('/x'), (e: unknown) => (e as Error).name == 'AbortError' && !(e instanceof ApiError))
})

test('empty bodies and binary bodies', async () => {
  const f = fakeFetch([new Response(null, { status: 204 }), new Response('', { status: 200 }), new Response(new Uint8Array([1, 2, 3]), { status: 200 })])
  const api = createApi({ fetch: f.fn })
  assert.equal(await api.del('/x'), undefined)
  assert.equal(await api.get('/x'), undefined)
  assert.deepEqual([...(await api.bytes('/q/replay?key=k'))], [1, 2, 3])
  assert.equal(f.calls[2].init.method, 'GET')
})

test('asList accepts a bare array or a single-key wrapper', () => {
  assert.deepEqual(asList([1, 2]), [1, 2])
  assert.deepEqual(asList({ items: [1] }), [1])
  assert.deepEqual(asList({ a: [1], b: [2] }), [])
  assert.deepEqual(asList(null), [])
  assert.deepEqual(asList('x'), [])
})
