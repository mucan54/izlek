// Conformance with a personal access token: one static bearer, sent by the
// official clients of both generations, reaches the projects of two
// organisations of the same user.
import assert from 'node:assert/strict'
import { Client as ModernClient, StreamableHTTPClientTransport as ModernTransport } from '@modelcontextprotocol/client'
import { Client as LegacyClient } from '@modelcontextprotocol/sdk/client/index.js'
import { StreamableHTTPClientTransport as LegacyTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js'
import { MCP_URL, PROJECT_ID, env, rawPost, text } from './lib.mjs'

const PAT = env('IZLEK_PAT')
const SECOND_PROJECT_ID = Number(env('IZLEK_SECOND_PROJECT_ID'))
assert.match(PAT, /^pat_[0-9A-Za-z]{32}$/)

const exercise = async (client, suffix) => {
  const { tools } = await client.listTools()
  assert.ok(tools.some((t) => t.name == 'create_project'), 'an account-wide token with the admin scope can create projects')
  const listed = await client.callTool({ name: 'list_projects', arguments: {} })
  assert.deepEqual(listed.structuredContent.projects.map((p) => p.id).sort(), [PROJECT_ID, SECOND_PROJECT_ID].sort())
  assert.equal(listed.structuredContent.organisations.length, 2)
  // Two projects are visible, so the project has to be named…
  const ambiguous = await client.callTool({ name: 'list_flags', arguments: {} })
  assert.equal(ambiguous.isError, true)
  assert.match(text(ambiguous), /project_id is required/)
  // …and the same token works in both.
  for (const id of [PROJECT_ID, SECOND_PROJECT_ID]) {
    const flag = await client.callTool({ name: 'create_flag', arguments: { project_id: id, key: `pat_${suffix}` } })
    assert.equal(flag.isError, false, text(flag))
    const flags = await client.callTool({ name: 'list_flags', arguments: { project_id: id } })
    assert.ok(flags.structuredContent.flags.some((f) => f.key == `pat_${suffix}`))
    const setup = await client.callTool({ name: 'get_sdk_setup', arguments: { project_id: id, platform: 'http' } })
    assert.equal(setup.isError, false, text(setup))
  }
}

const versions = []
{
  const client = new ModernClient({ name: 'conformance-pat', version: '2.0.0' }, { versionNegotiation: { mode: 'auto' } })
  await client.connect(new ModernTransport(new URL(MCP_URL), { authProvider: { token: async () => PAT } }))
  assert.match(client.getInstructions(), /personal access token/)
  await exercise(client, 'modern')
  versions.push(client.getNegotiatedProtocolVersion())
  await client.close()
}
{
  const transport = new LegacyTransport(new URL(MCP_URL), { requestInit: { headers: { Authorization: `Bearer ${PAT}` } } })
  const client = new LegacyClient({ name: 'conformance-pat', version: '1.0.0' })
  await client.connect(transport)
  await exercise(client, 'legacy')
  versions.push(transport.protocolVersion)
  await client.close()
}

// A token that does not exist gets the 401 every other bad credential gets.
const bad = await rawPost({ jsonrpc: '2.0', id: 1, method: 'tools/list' }, { authorization: `Bearer pat_${'0'.repeat(32)}` })
assert.equal(bad.status, 401)
assert.match(bad.headers.get('www-authenticate'), /error="invalid_token"/)

console.log(JSON.stringify({ versions }))
