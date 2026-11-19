// Conformance with @modelcontextprotocol/sdk 1.x: the initialize handshake
// (2025-11-25, and 2025-06-18 / 2025-03-26 by rewriting the version the
// client asks for), an sk_ bearer, and the OAuth flow with dynamic client
// registration, token refresh and refresh-token reuse.
import assert from 'node:assert/strict'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js'
import { UnauthorizedError } from '@modelcontextprotocol/sdk/client/auth.js'
import { LATEST_PROTOCOL_VERSION } from '@modelcontextprotocol/sdk/types.js'
import { BASE_URL, MCP_URL, PROJECT_ID, SECRET_KEY, WRITE_TOKEN, advanceClock, consent, memoryProvider, rawPost, rejection, text } from './lib.mjs'

assert.equal(LATEST_PROTOCOL_VERSION, '2025-11-25', 'this script expects the 2025-11-25 generation of the SDK')

// A fetch that makes the client ask for an older revision in initialize and
// records the MCP-Protocol-Version header of every later request.
const recordingFetch = (askFor, seen) => async (url, init = {}) => {
  let body = init.body
  if (typeof body == 'string' && askFor) {
    const msg = JSON.parse(body)
    if (msg.method == 'initialize') {
      msg.params.protocolVersion = askFor
      body = JSON.stringify(msg)
    }
  }
  const sent = new Headers(init.headers).get('mcp-protocol-version')
  if (sent) seen.add(sent)
  return fetch(url, { ...init, body })
}

const connectWithKey = async (askFor) => {
  const seen = new Set()
  const transport = new StreamableHTTPClientTransport(new URL(MCP_URL), {
    requestInit: { headers: { Authorization: `Bearer ${SECRET_KEY}` } },
    fetch: recordingFetch(askFor, seen),
  })
  const client = new Client({ name: 'conformance-legacy', version: '1.0.0' })
  await client.connect(transport)
  return { client, transport, seen }
}

// ---- (a) sk_ bearer ----

const keyVersions = []
{
  const { client, transport, seen } = await connectWithKey(undefined)
  assert.equal(transport.protocolVersion, '2025-11-25')
  assert.equal(transport.sessionId, undefined, 'the server is stateless: no session id')
  assert.equal(client.getServerVersion().name, 'izlek')
  assert.deepEqual(client.getServerCapabilities().tools, { listChanged: false })
  assert.match(client.getInstructions(), /UTC/)
  await client.ping()

  const { tools } = await client.listTools()
  const names = tools.map((t) => t.name)
  assert.ok(names.includes('verify_installation') && names.includes('run_sql'))
  assert.ok(!names.includes('create_project'), 'a key belongs to one project and cannot create others')
  for (const t of tools) {
    assert.equal(t.inputSchema.type, 'object', t.name)
    assert.ok(t.description.length > 40, `${t.name} has a description`)
    assert.equal(typeof t.annotations.readOnlyHint, 'boolean', t.name)
    assert.equal(t.annotations.openWorldHint, false, t.name)
  }
  assert.equal(tools.find((t) => t.name == 'run_sql').annotations.readOnlyHint, true)
  assert.equal(tools.find((t) => t.name == 'update_flag').annotations.destructiveHint, true)

  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.isError, false)
  assert.equal(projects.structuredContent.projects[0].id, PROJECT_ID)
  assert.deepEqual(JSON.parse(text(projects)), projects.structuredContent, 'the text block is the JSON of the structured content')

  const setup = await client.callTool({ name: 'get_sdk_setup', arguments: { platform: 'web_module' } })
  assert.equal(setup.structuredContent.token, WRITE_TOKEN)
  assert.equal(setup.structuredContent.ingest_host_is_placeholder, false)
  assert.ok(text(setup).includes(`init({ token: '${WRITE_TOKEN}', host: 'https://in.example.com' })`))
  assert.match(text(setup), /not published to npm/)

  const created = await client.callTool({ name: 'create_flag', arguments: { key: 'conformance_key', enabled: true, rules: [{ rollout: 10000 }] } })
  assert.equal(created.isError, false, text(created))
  assert.equal(created.structuredContent.flag.version, 1)
  const evaluated = await client.callTool({ name: 'evaluate_flag', arguments: { key: 'conformance_key', anon_id: 'a1' } })
  assert.equal(evaluated.structuredContent.value, true)
  const updated = await client.callTool({ name: 'update_flag', arguments: { key: 'conformance_key', enabled: false, expected_version: 1 } })
  assert.equal(updated.structuredContent.flag.enabled, false)
  assert.equal(updated.structuredContent.flag.version, 2)

  const verify = await client.callTool({ name: 'verify_installation', arguments: { minutes: 15 } })
  assert.equal(verify.structuredContent.receiving, true)
  assert.equal(verify.structuredContent.total_events, 15)
  const schema = await client.callTool({ name: 'get_schema', arguments: {} })
  assert.deepEqual(schema.structuredContent.events, ['pageview', 'purchase'])
  const trends = await client.callTool({
    name: 'query_trends',
    arguments: { from: '2026-11-01', to: '2026-11-18', series: [{ event: 'purchase', agg: 'count' }] },
  })
  assert.deepEqual(trends.structuredContent.series[0].values, [3])

  // A tool that fails says so in a result the model can read…
  const bad = await client.callTool({ name: 'update_write_token', arguments: { token: WRITE_TOKEN, status: 'paused' } })
  assert.equal(bad.isError, true)
  assert.match(text(bad), /bad_request/)
  const typo = await client.callTool({ name: 'get_flag', arguments: { key: 'conformance_key', projekt: 1 } })
  assert.equal(typo.isError, true)
  assert.match(text(typo), /Unknown argument/)
  const foreign = await client.callTool({ name: 'get_project', arguments: { project_id: PROJECT_ID + 1 } })
  assert.equal(foreign.isError, true)
  assert.match(text(foreign), /not_found/)
  // …an unknown tool is a protocol error.
  const unknown = await rejection(() => client.callTool({ name: 'drop_everything', arguments: {} }))
  assert.equal(unknown.code, -32602)

  assert.deepEqual([...seen], ['2025-11-25'], 'later requests carry the negotiated revision')
  keyVersions.push(transport.protocolVersion)
  await client.close()
}
for (const version of ['2025-06-18', '2025-03-26']) {
  const { client, transport, seen } = await connectWithKey(version)
  assert.equal(transport.protocolVersion, version, 'the server answers with the revision it was asked for')
  const { tools } = await client.listTools()
  assert.ok(tools.length > 15)
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.structuredContent.projects[0].id, PROJECT_ID)
  assert.deepEqual([...seen], [version])
  keyVersions.push(transport.protocolVersion)
  await client.close()
}

// Without credentials: the 401 that starts the OAuth flow.
{
  const res = await rawPost({ jsonrpc: '2.0', id: 1, method: 'tools/list' })
  assert.equal(res.status, 401)
  const challenge = res.headers.get('www-authenticate')
  assert.match(challenge, /^Bearer /)
  assert.ok(challenge.includes(`resource_metadata="${BASE_URL}/.well-known/oauth-protected-resource/mcp"`), challenge)
  assert.match(challenge, /scope="izlek:read izlek:admin"/)
}

// ---- (b) OAuth: discovery → dynamic registration → consent → token → tools → refresh ----

const { provider, store } = memoryProvider({
  redirectUrl: 'http://127.0.0.1:33418/callback',
  clientMetadata: {
    client_name: 'Conformance legacy client',
    redirect_uris: ['http://127.0.0.1:33418/callback'],
    grant_types: ['authorization_code', 'refresh_token'],
    response_types: ['code'],
    token_endpoint_auth_method: 'none',
  },
})
const oauthTransport = () => new StreamableHTTPClientTransport(new URL(MCP_URL), { authProvider: provider })

{
  const transport = oauthTransport()
  const client = new Client({ name: 'conformance-legacy', version: '1.0.0' })
  const err = await rejection(() => client.connect(transport))
  assert.ok(err instanceof UnauthorizedError, `connect without tokens: ${err}`)

  // The client discovered the metadata, registered itself and built the URL.
  assert.match(store.client.client_id, /^oc_/)
  assert.equal(store.client.client_secret, undefined, 'public client: no secret')
  const url = store.authorizationUrl
  assert.equal(url.origin + url.pathname, `${BASE_URL}/oauth/authorize`)
  assert.equal(url.searchParams.get('code_challenge_method'), 'S256')
  assert.equal(url.searchParams.get('resource'), MCP_URL)
  assert.equal(url.searchParams.get('scope'), 'izlek:read izlek:admin')

  const { params, html } = await consent(url, { expectClient: 'Conformance legacy client' })
  assert.match(html, /registered itself/, 'a dynamically registered client is marked as unverified')
  assert.match(html, /on your own computer/, 'a loopback redirect carries a warning')
  assert.equal(params.get('state'), store.state)
  assert.equal(params.get('iss'), BASE_URL, 'RFC 9207 issuer on the authorization response')
  await transport.finishAuth(params.get('code'))
  assert.match(store.tokens.access_token, /^oat_/)
  assert.match(store.tokens.refresh_token, /^ort_/)
  assert.equal(store.tokens.scope, 'izlek:read izlek:admin')
  assert.equal(store.tokens.expires_in, 3600)
}

const client = new Client({ name: 'conformance-legacy', version: '1.0.0' })
const transport = oauthTransport()
await client.connect(transport)
const oauthVersion = transport.protocolVersion
{
  const { tools } = await client.listTools()
  assert.ok(tools.some((t) => t.name == 'create_project'), 'a user can create projects')
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.structuredContent.organisations[0].role, 'owner')
  const flag = await client.callTool({ name: 'create_flag', arguments: { project_id: PROJECT_ID, key: 'conformance_oauth' } })
  assert.equal(flag.isError, false, text(flag))
}

// The access token expires; the client refreshes on the 401 and carries on.
const first = store.tokens
await advanceClock(3700)
{
  const projects = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.equal(projects.isError, false)
  assert.notEqual(store.tokens.access_token, first.access_token, 'a new access token was issued')
  assert.notEqual(store.tokens.refresh_token, first.refresh_token, 'the refresh token rotated')
}
await client.close()

// The old refresh token is spent. Presenting it again ends the grant.
{
  const res = await fetch(`${BASE_URL}/oauth/token`, {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: first.refresh_token, client_id: store.client.client_id }),
  })
  assert.equal(res.status, 400)
  assert.equal((await res.json()).error, 'invalid_grant')
  const after = await rawPost({ jsonrpc: '2.0', id: 1, method: 'tools/list' }, { authorization: `Bearer ${store.tokens.access_token}` })
  assert.equal(after.status, 401, 'the newest access token died with the grant')
  assert.match(after.headers.get('www-authenticate'), /error="invalid_token"/)
}

console.log(JSON.stringify({ key_versions: keyVersions, oauth_version: oauthVersion, registration: 'dcr', refreshed: true, reuse_revoked: true }))
