import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import {
  KEY_PLACEHOLDER, MCP_TOOLS, OAUTH_METADATA_PATH, PAT_DEFAULT_EXPIRY_DAYS, PAT_EXPIRY_DAYS, PAT_PLACEHOLDER, claudeCodeCommand, classifyOAuthMetadata, clientSite, mcpEndpoint, probeMcp, scopesFor,
  summarizeScopes,
} from '../../src/lib/mcp'
import { fakeFetch, json } from './helpers'

const SPA_SHELL = '<!doctype html>\n<html lang="tr"><head><title>İzlek</title></head><body><div id="app"></div></body></html>'

test('OAuth metadata: a document with an issuer means OAuth, 404 means secret keys only', () => {
  assert.deepEqual(classifyOAuthMetadata(200, JSON.stringify({ issuer: 'https://izlek.example.com', token_endpoint: 'https://izlek.example.com/oauth/token' })), {
    state: 'oauth',
    issuer: 'https://izlek.example.com',
  })
  assert.deepEqual(classifyOAuthMetadata(200, '{"issuer":"http://localhost:8090/"}'), { state: 'oauth', issuer: 'http://localhost:8090' }, 'the issuer is reduced to its origin')
  assert.deepEqual(classifyOAuthMetadata(404, '{"error":{"code":"not_found","message":"Kaynak bulunamadı"}}'), { state: 'keys' })
  assert.deepEqual(classifyOAuthMetadata(404, ''), { state: 'keys' })
})

test('OAuth metadata: the SPA shell with status 200 is not mistaken for the document', () => {
  // LA_MCP_ENABLED=false or an older server: /.well-known/* falls through to index.html
  assert.deepEqual(classifyOAuthMetadata(200, SPA_SHELL), { state: 'off' })
  for (const body of ['', 'null', '[]', '"https://izlek.example.com"', '{}', '{"issuer":42}', '{"issuer":""}', '{"issuer":"not a url"}', '{"issuer":"javascript:alert(1)"}'])
    assert.deepEqual(classifyOAuthMetadata(200, body), { state: 'off' }, body)
  for (const status of [500, 502, 503, 401, 403, 204, 301]) assert.deepEqual(classifyOAuthMetadata(status, '{"issuer":"https://izlek.example.com"}'), { state: 'unknown' }, String(status))
})

test('probeMcp asks the well-known path without credentials and survives a failing network', async () => {
  const ok = fakeFetch([json(200, { issuer: 'https://izlek.example.com' })])
  assert.deepEqual(await probeMcp(ok.fn), { state: 'oauth', issuer: 'https://izlek.example.com' })
  assert.equal(ok.calls[0].url, OAUTH_METADATA_PATH)
  assert.equal(OAUTH_METADATA_PATH, '/.well-known/oauth-authorization-server')
  assert.equal(ok.calls[0].init.credentials, 'omit')

  assert.deepEqual(await probeMcp(fakeFetch([json(404, { error: { code: 'not_found' } })]).fn), { state: 'keys' })
  assert.deepEqual(await probeMcp(fakeFetch([new Response(SPA_SHELL, { status: 200, headers: { 'Content-Type': 'text/html' } })]).fn), { state: 'off' })
  assert.deepEqual(await probeMcp(fakeFetch([new TypeError('Failed to fetch')]).fn), { state: 'unknown' })
  const aborted = Object.assign(new Error('aborted'), { name: 'AbortError' })
  await assert.rejects(probeMcp(fakeFetch([aborted]).fn), /aborted/)
})

test('endpoint: the page origin, unless the server says its public address is another one', () => {
  assert.deepEqual(mcpEndpoint('https://izlek.example.com', undefined), { url: 'https://izlek.example.com/mcp' })
  assert.deepEqual(mcpEndpoint('https://izlek.example.com/', { state: 'keys' }), { url: 'https://izlek.example.com/mcp' })
  assert.deepEqual(mcpEndpoint('https://izlek.example.com', { state: 'off' }), { url: 'https://izlek.example.com/mcp' })
  assert.deepEqual(mcpEndpoint('https://izlek.example.com', { state: 'unknown' }), { url: 'https://izlek.example.com/mcp' })
  assert.deepEqual(mcpEndpoint('https://izlek.example.com', { state: 'oauth', issuer: 'https://izlek.example.com' }), { url: 'https://izlek.example.com/mcp' })
  // opened through an internal address: tokens are bound to <LA_PUBLIC_URL>/mcp, so that is the address to hand out
  assert.deepEqual(mcpEndpoint('http://10.0.0.5:8090', { state: 'oauth', issuer: 'https://izlek.example.com' }), {
    url: 'https://izlek.example.com/mcp',
    publicOrigin: 'https://izlek.example.com',
  })
})

test('Claude Code commands are the ones docs/mcp.md gives, with a placeholder for the key', () => {
  const url = 'https://izlek.example.com/mcp'
  assert.equal(claudeCodeCommand(url, 'oauth'), 'claude mcp add --transport http izlek https://izlek.example.com/mcp')
  assert.equal(claudeCodeCommand(url, 'key'), 'claude mcp add --transport http izlek https://izlek.example.com/mcp \\\n  --header "Authorization: Bearer sk_…"')
  assert.equal(claudeCodeCommand(url, 'pat'), 'claude mcp add --transport http izlek https://izlek.example.com/mcp --header "Authorization: Bearer pat_…"')
  assert.equal(KEY_PLACEHOLDER, 'sk_…')
  assert.equal(PAT_PLACEHOLDER, 'pat_…')
  assert.ok(!/sk_[0-9A-Za-z]/.test(claudeCodeCommand(url, 'key')), 'nothing that looks like a real key')
  assert.ok(!/pat_[0-9A-Za-z]/.test(claudeCodeCommand(url, 'pat')), 'nothing that looks like a real token')

  let doc: string
  try {
    doc = readFileSync(`${__UI_ROOT__}/../docs/mcp.md`, 'utf8')
  } catch {
    return // the UI checked out without the rest of the repository
  }
  const blocks = [...doc.matchAll(/```sh\n([\s\S]*?)\n```/g)].map((m) => m[1])
  for (const auth of ['oauth', 'key'] as const) assert.ok(blocks.includes(claudeCodeCommand(url, auth)), `docs/mcp.md has the ${auth} command verbatim`)
  // The access-token section is being added to the doc separately: checked once the doc mentions pat_ at all,
  // and without regard to line continuations (the doc may break the command over two lines).
  if (!doc.includes('pat_')) return
  const oneLine = (cmd: string) => cmd.replace(/\s*\\\n\s*/g, ' ').replace(/[ \t]+/g, ' ').trim()
  const lines = blocks.flatMap((b) => oneLine(b).split('\n').map((l) => l.trim()))
  assert.ok(lines.includes(claudeCodeCommand(url, 'pat')), 'docs/mcp.md has the access-token command')
})

test('tool list: 22 tools in three groups, the writing ones marked', () => {
  const all = Object.values(MCP_TOOLS).flat()
  assert.deepEqual([MCP_TOOLS.setup.length, MCP_TOOLS.flags.length, MCP_TOOLS.analytics.length, all.length], [11, 5, 6, 22])
  assert.equal(new Set(all.map((t) => t.name)).size, all.length, 'names are unique')
  assert.deepEqual(
    all.filter((t) => t.write).map((t) => t.name),
    ['create_project', 'update_project_settings', 'create_write_token', 'update_write_token', 'create_secret_key', 'create_flag', 'update_flag'],
  )
  assert.ok(MCP_TOOLS.analytics.every((t) => !t.write), 'analytics is read-only')
  // what the server deliberately does not offer must not show up as a capability
  assert.ok(all.every((t) => !/^(delete|close|remove)_|member|deletion/.test(t.name)))
})

test('tool list matches the tables of docs/mcp.md', () => {
  let doc: string
  try {
    doc = readFileSync(`${__UI_ROOT__}/../docs/mcp.md`, 'utf8')
  } catch {
    return // the UI checked out without the rest of the repository
  }
  const section = doc.slice(doc.indexOf('\n## Tools'), doc.indexOf('\n## Revoking access'))
  const group = (heading: string) => {
    const from = section.indexOf(`**${heading}**`)
    assert.ok(from >= 0, `docs/mcp.md has a "${heading}" table`)
    const next = section.indexOf('\n**', from + heading.length + 4)
    return [...section.slice(from, next < 0 ? undefined : next).matchAll(/^\| `([a-z_]+)` \| (read|admin) \|/gm)].map((m) => ({ name: m[1], ...(m[2] == 'admin' ? { write: true } : {}) }))
  }
  assert.deepEqual(MCP_TOOLS.setup, group('Setup'))
  assert.deepEqual(MCP_TOOLS.flags, group('Feature flags'))
  assert.deepEqual(MCP_TOOLS.analytics, group('Analytics'))
})

test('scopes: admin implies read; unknown scopes are kept as they are', () => {
  assert.deepEqual(summarizeScopes(['izlek:read']), { level: 'read', other: [] })
  assert.deepEqual(summarizeScopes(['izlek:read', 'izlek:admin']), { level: 'admin', other: [] })
  assert.deepEqual(summarizeScopes(['izlek:admin']), { level: 'admin', other: [] })
  assert.deepEqual(summarizeScopes(['offline_access', 'izlek:read', 'openid']), { level: 'read', other: ['offline_access', 'openid'] })
  assert.deepEqual(summarizeScopes([]), { level: null, other: [] })
  assert.deepEqual(summarizeScopes(null), { level: null, other: [] })
  assert.deepEqual(summarizeScopes(undefined), { level: null, other: [] })
})

test('clientSite: the host of a metadata document, nothing for self-registered clients', () => {
  assert.equal(clientSite({ client_kind: 'cimd', client_id: 'https://chatgpt.com/oauth/client.json' }), 'chatgpt.com')
  assert.equal(clientSite({ client_kind: 'cimd', client_id: 'https://example.com:8443/client.json' }), 'example.com:8443')
  assert.equal(clientSite({ client_kind: 'cimd', client_id: 'oc_abc' }), null)
  assert.equal(clientSite({ client_kind: 'dcr', client_id: 'oc_AbCdEf0123456789AbCdEf01' }), null)
  assert.equal(clientSite({ client_kind: 'dcr', client_id: 'https://evil.example/claims-to-be-a-site' }), null, 'a self-registered client never gets a site')
})

test('access tokens: scopes for the two access levels, and the lifetimes on offer', () => {
  assert.deepEqual(scopesFor('read'), ['izlek:read'])
  assert.deepEqual(scopesFor('admin'), ['izlek:read', 'izlek:admin'])
  assert.deepEqual(summarizeScopes(scopesFor('read')).level, 'read')
  assert.deepEqual(summarizeScopes(scopesFor('admin')).level, 'admin')
  assert.deepEqual(PAT_EXPIRY_DAYS, [30, 90, 365, null])
  assert.ok(PAT_EXPIRY_DAYS.includes(PAT_DEFAULT_EXPIRY_DAYS))
  assert.equal(PAT_DEFAULT_EXPIRY_DAYS, 90)
})
