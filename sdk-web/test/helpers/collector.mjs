// A tiny local stand-in for ingestion + flag CDN, used by the browser smoke test.
// It checks the HTTP contract of spec/wire.md §4 and spec/replay.md §4 and
// decodes every body, so a request that the real server would reject fails here too.
import { createServer } from 'node:http'
import { gunzipSync } from 'node:zlib'

import { decodeChunk } from '../../dist/replay-decode.js'
import { decodeBatch } from './la1.mjs'

export const TOKEN = 'pk_smoke'

export async function startCollector({ ruleset }) {
  const state = {
    /** every POST /v1/e: { status, batch, auth: 'header' | 'query', gzip, error? } */
    posts: [],
    /** every POST /v1/r: { status, query, chunk, raw, error? } */
    chunks: [],
    /** flag downloads: { status, ifNoneMatch } */
    flagGets: [],
    preflights: 0,
    /** status to answer event posts with (200 = accept) */
    eventStatus: 200,
    etag: '"rules-1"',
    ruleset,
  }

  const cors = (req, res) => {
    res.setHeader('Access-Control-Allow-Origin', req.headers.origin || '*')
    res.setHeader('Access-Control-Expose-Headers', 'ETag, Retry-After')
    res.setHeader('Vary', 'Origin')
  }
  const body = (req) =>
    new Promise((resolve, reject) => {
      const parts = []
      req.on('data', (d) => parts.push(d))
      req.on('end', () => resolve(Buffer.concat(parts)))
      req.on('error', reject)
    })
  const auth = (req, url) => {
    const h = req.headers.authorization
    if (h) return h == `Bearer ${TOKEN}` ? 'header' : null
    return url.searchParams.get('t') == TOKEN ? 'query' : null
  }

  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://collector')
    cors(req, res)
    try {
      if (req.method == 'OPTIONS') {
        state.preflights++
        res.setHeader('Access-Control-Allow-Methods', 'POST, GET')
        res.setHeader('Access-Control-Allow-Headers', req.headers['access-control-request-headers'] || '')
        res.setHeader('Access-Control-Max-Age', '86400')
        return res.writeHead(204).end()
      }

      if (req.method == 'POST' && url.pathname == '/v1/e') {
        const raw = await body(req)
        const entry = { status: state.eventStatus, auth: auth(req, url), gzip: req.headers['content-encoding'] == 'gzip' }
        state.posts.push(entry)
        if (!entry.auth) entry.status = 401
        try {
          entry.batch = decodeBatch(new Uint8Array(entry.gzip ? gunzipSync(raw) : raw), { strict: true })
        } catch (e) {
          entry.error = String(e)
          entry.status = 400
        }
        if (entry.status == 503) res.setHeader('Retry-After', '1')
        return res.writeHead(entry.status).end()
      }

      if (req.method == 'POST' && url.pathname == '/v1/r') {
        const raw = await body(req)
        const entry = { status: 200, query: Object.fromEntries(url.searchParams), encoding: req.headers['content-encoding'] }
        state.chunks.push(entry)
        try {
          if (!auth(req, url)) throw Object.assign(new Error('unauthorized'), { status: 401 })
          if (entry.encoding != 'gzip') throw new Error('Content-Encoding: gzip is required')
          if (raw.length > 1 << 20) throw Object.assign(new Error('too large'), { status: 413 })
          entry.raw = new Uint8Array(gunzipSync(raw))
          entry.chunk = decodeChunk(entry.raw, true)
        } catch (e) {
          entry.error = String(e)
          entry.status = e.status || 400
        }
        return res.writeHead(entry.status).end()
      }

      if (req.method == 'GET' && url.pathname == `/flags/${TOKEN}.json`) {
        const ifNoneMatch = req.headers['if-none-match']
        const status = ifNoneMatch == state.etag ? 304 : 200
        state.flagGets.push({ status, ifNoneMatch })
        res.setHeader('ETag', state.etag)
        res.setHeader('Cache-Control', 'max-age=60')
        if (status == 304) return res.writeHead(304).end()
        res.setHeader('Content-Type', 'application/json')
        return res.writeHead(200).end(JSON.stringify(state.ruleset))
      }
      res.writeHead(404).end()
    } catch (e) {
      res.writeHead(500).end(String(e))
    }
  })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  return {
    state,
    origin: `http://127.0.0.1:${server.address().port}`,
    close: () => new Promise((r) => (server.closeAllConnections(), server.close(r))),
    /** accepted records, flattened: { kind, name, props (typed list), batch, post } */
    accepted() {
      return state.posts.filter((p) => p.status == 200).flatMap((p) => p.batch.records.map((r) => ({ ...r, batch: p.batch, post: p })))
    },
  }
}
