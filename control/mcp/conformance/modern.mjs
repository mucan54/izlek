// Conformance with @modelcontextprotocol/client 2.x: the stateless
// 2026-07-28 revision (found by the server/discover probe, and pinned), the
// client's legacy mode, an sk_ bearer, and the OAuth flow with a Client ID
// Metadata Document, a read-only grant, scope step-up and token refresh.
import assert from 'node:assert/strict'
import { Client, StreamableHTTPClientTransport, UnauthorizedError } from '@modelcontextprotocol/client'
import { BASE_URL, CIMD_CLIENT_ID, MCP_URL, PROJECT_ID, SECRET_KEY, WRITE_TOKEN, advanceClock, consent, memoryProvider, rawPost, rejection, text } from './lib.mjs'

// Records the protocol headers the client really sends.
const recordingFetch = (seen) => async (url, init = {}) => {
  const h = new Headers(init.headers)
  if (typeof init.body == 'string') {
    const msg = JSON.parse(init.body)
    seen.push({ method: msg.method, version: h.get('mcp-protocol-version'), mcpMethod: h.get('mcp-method'), mcpName: h.get('mcp-name'), session: h.get('mcp-session-id') })
  }
  return fetch(url, init)
}

const connectWithKey = async (mode) => {
  const seen = []
  const transport = new StreamableHTTPClientTransport(new URL(MCP_URL), {
    authProvider: { token: async () => SECRET_KEY },
    fetch: recordingFetch(seen),
  })
  const client = new Client({ name: 'conformance-modern', version: '2.0.0' }, mode ? { versionNegotiation: { mode } } : {})
  await client.connect(transport)
  return { client, seen }
}

// ---- (a) sk_ bearer ----

const keyVersions = []
{
  // 'auto': the client probes with server/discover and stays modern.
  const { client, seen } = await connectWithKey('auto')
  assert.equal(client.getNegotiatedProtocolVersion(), '2026-07-28')
  assert.equal(seen[0].method, 'server/discover', 'the first request is the probe')
  assert.ok(!seen.some((r) => r.method == 'initialize'), 'no handshake in the modern era')
  assert.equal(client.getServerVersion().name, 'izlek')
  assert.match(client.getInstructions(), /write tokens/i)

  const { tools } = await client.listTools()
  const names = tools.map((t) => t.name)
  assert.ok(names.includes('get_sdk_setup') && !names.includes('create_project'))

  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.structuredContent.projects[0].id, PROJECT_ID)
  const setup = await client.callTool({ name: 'get_sdk_setup', arguments: { platform: 'http' } })
  assert.ok(text(setup).includes(`"api_key": "${WRITE_TOKEN}"`))
  const verify = await client.callTool({ name: 'verify_installation', arguments: {} })
  assert.equal(verify.structuredContent.receiving, true)
  const bad = await client.callTool({ name: 'create_flag', arguments: { key: 'Not A Key' } })
  assert.equal(bad.isError, true)
  assert.match(text(bad), /invalid_flag/)
  const unknown = await rejection(() => client.callTool({ name: 'drop_everything', arguments: {} }))
  assert.equal(unknown.code, -32602)

  // Every request carried the per-request headers of 2026-07-28.
  for (const r of seen) {
    assert.equal(r.version, '2026-07-28', r.method)
    assert.equal(r.mcpMethod, r.method)
    assert.equal(r.session, null, 'no sessions')
  }
  assert.ok(seen.filter((r) => r.method == 'tools/call').every((r) => r.mcpName), 'tools/call mirrors the tool name')
  keyVersions.push(client.getNegotiatedProtocolVersion())
  await client.close()
}
{
  const { client } = await connectWithKey({ pin: '2026-07-28' })
  assert.equal(client.getNegotiatedProtocolVersion(), '2026-07-28')
  const schema = await client.callTool({ name: 'get_schema', arguments: {} })
  assert.deepEqual(schema.structuredContent.person_props, ['plan'])
  keyVersions.push(client.getNegotiatedProtocolVersion())
  await client.close()
}
{
  // The 2.x client's default is still the handshake.
  const { client, seen } = await connectWithKey(undefined)
  assert.equal(seen[0].method, 'initialize')
  assert.equal(client.getNegotiatedProtocolVersion(), '2025-11-25')
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.structuredContent.projects[0].id, PROJECT_ID)
  keyVersions.push(client.getNegotiatedProtocolVersion())
  await client.close()
}

// The shape of 2026-07-28 results and errors, without a client library.
{
  const auth = { authorization: `Bearer ${SECRET_KEY}`, 'mcp-protocol-version': '2026-07-28' }
  const meta = { 'io.modelcontextprotocol/protocolVersion': '2026-07-28', 'io.modelcontextprotocol/clientCapabilities': {} }
  const list = await rawPost({ jsonrpc: '2.0', id: 1, method: 'tools/list', params: { _meta: meta } }, { ...auth, 'mcp-method': 'tools/list' })
  assert.equal(list.status, 200)
  const { result } = await list.json()
  assert.equal(result.resultType, 'complete')
  assert.equal(typeof result.ttlMs, 'number')
  assert.equal(result.cacheScope, 'private')
  assert.equal(result._meta['io.modelcontextprotocol/serverInfo'].name, 'izlek')

  const future = await rawPost({ jsonrpc: '2.0', id: 2, method: 'tools/list', params: { _meta: { ...meta, 'io.modelcontextprotocol/protocolVersion': '2099-01-01' } } }, { ...auth, 'mcp-protocol-version': '2099-01-01', 'mcp-method': 'tools/list' })
  assert.equal(future.status, 400)
  const { error } = await future.json()
  assert.equal(error.code, -32022)
  assert.ok(error.data.supported.includes('2026-07-28') && error.data.supported.includes('2025-11-25'))
}

// ---- (b) OAuth with a Client ID Metadata Document ----

const { provider, store } = memoryProvider({
  // The document registers http://127.0.0.1/callback; a native client
  // listens on whatever port is free (RFC 8252 §7.3).
  redirectUrl: 'http://127.0.0.1:45871/callback',
  clientMetadata: {
    client_name: 'Conformance CIMD client',
    redirect_uris: ['http://127.0.0.1:45871/callback'],
    grant_types: ['authorization_code', 'refresh_token'],
    response_types: ['code'],
    token_endpoint_auth_method: 'none',
  },
  overrides: { clientMetadataUrl: CIMD_CLIENT_ID },
})
const newClient = () => new Client({ name: 'conformance-modern', version: '2.0.0' }, { versionNegotiation: { mode: 'auto' } })
const oauthTransport = () => new StreamableHTTPClientTransport(new URL(MCP_URL), { authProvider: provider })

// First the user approves read-only access.
{
  const transport = oauthTransport()
  const err = await rejection(() => newClient().connect(transport))
  assert.ok(err instanceof UnauthorizedError, `connect without tokens: ${err}`)
  const url = store.authorizationUrl
  assert.equal(url.searchParams.get('client_id'), CIMD_CLIENT_ID, 'the client identifies itself by its metadata URL')
  assert.equal(url.searchParams.get('resource'), MCP_URL)
  const { params, html } = await consent(url, { expectClient: 'Conformance CIMD client', grantAdmin: false })
  assert.match(html, /read from/, 'the page says where the client metadata came from')
  assert.ok(html.includes('example.com'))
  assert.ok(!/registered itself/.test(html), 'not marked as self-registered')
  assert.equal(params.get('iss'), BASE_URL)
  await transport.finishAuth(params) // code and iss; the client validates iss (RFC 9207)
  assert.equal(store.tokens.scope, 'izlek:read', 'the user unticked "allow changes"')
}

let client = newClient()
await client.connect(oauthTransport())
const oauthVersion = client.getNegotiatedProtocolVersion()
{
  const { tools } = await client.listTools()
  const names = tools.map((t) => t.name)
  assert.ok(names.includes('query_trends') && names.includes('list_flags'))
  assert.ok(!names.some((n) => n.startsWith('create_') || n.startsWith('update_')), 'a read-only grant does not see tools that change things')
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.structuredContent.organisations[0].name, 'Acme')
}

// Calling an admin tool anyway: 403 insufficient_scope, and the client
// starts a step-up authorization that asks for the missing scope.
let readOnlyStepUp = false
{
  store.authorizationUrl = undefined
  const err = await rejection(() => client.callTool({ name: 'create_write_token', arguments: { project_id: PROJECT_ID, name: 'from-oauth' } }))
  const raw = await rawPost(
    { jsonrpc: '2.0', id: 9, method: 'tools/call', params: { name: 'create_write_token', arguments: { name: 'x' }, _meta: { 'io.modelcontextprotocol/protocolVersion': '2026-07-28', 'io.modelcontextprotocol/clientCapabilities': {} } } },
    { authorization: `Bearer ${store.tokens.access_token}`, 'mcp-protocol-version': '2026-07-28', 'mcp-method': 'tools/call', 'mcp-name': 'create_write_token' },
  )
  assert.equal(raw.status, 403)
  assert.match(raw.headers.get('www-authenticate'), /error="insufficient_scope"/)
  assert.match(raw.headers.get('www-authenticate'), /scope="izlek:read izlek:admin"/)
  assert.ok(err, 'the call did not go through')
  await client.close()

  // The official client turned the 403 challenge into a new authorization
  // request that asks for the missing scope on top of what it had.
  assert.ok(store.authorizationUrl, `the client did not start a step-up authorization: ${err}`)
  assert.equal(store.authorizationUrl.searchParams.get('scope'), 'izlek:read izlek:admin')
  readOnlyStepUp = true
  const transport = oauthTransport()
  const { params } = await consent(store.authorizationUrl, { expectClient: 'Conformance CIMD client' })
  await transport.finishAuth(params) // code and iss; the client validates iss (RFC 9207)
  assert.equal(store.tokens.scope, 'izlek:read izlek:admin')
}

client = newClient()
await client.connect(oauthTransport())
{
  const made = await client.callTool({ name: 'create_write_token', arguments: { project_id: PROJECT_ID, name: 'from-oauth', replay: true } })
  assert.equal(made.isError, false, text(made))
  assert.match(made.structuredContent.token.token, /^pk_/)
  const setup = await client.callTool({ name: 'get_sdk_setup', arguments: { project_id: PROJECT_ID, platform: 'web', token: made.structuredContent.token.token } })
  assert.match(text(setup), /izlekReplay\.startReplay/)
}

// The access token expires; the client refreshes and carries on.
const before = store.tokens
await advanceClock(3700)
{
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.isError, false)
  assert.notEqual(store.tokens.access_token, before.access_token)
  assert.notEqual(store.tokens.refresh_token, before.refresh_token)
}
await client.close()

console.log(JSON.stringify({ key_versions: keyVersions, oauth_version: oauthVersion, registration: 'cimd', refreshed: true, read_only_step_up: readOnlyStepUp }))
