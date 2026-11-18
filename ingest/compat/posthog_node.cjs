// PostHog Node SDK against the ingestion compat adapter.
// Usage: node posthog_node.cjs <ingest-base-url> <token>
// Prints one JSON line: { errors, queueLength, flag, flagEnabled, sent }.
'use strict'
const { PostHog } = require('posthog-node')

const [host, token] = process.argv.slice(2)
if (!host || !token) {
  console.error('usage: posthog_node.cjs <host> <token>')
  process.exit(2)
}

async function main() {
  const errors = []
  const client = new PostHog(token, { host, flushAt: 5, flushInterval: 200 })
  client.on('error', (e) => errors.push(String((e && e.message) || e)))

  // capture, identify, alias: the calls the design names for the compat test.
  client.capture({ distinctId: 'node-user-1', event: 'node_capture', properties: { plan: 'pro', n: 1, nested: { a: [1, 2] } } })
  client.identify({ distinctId: 'node-user-1', properties: { name: 'Node User', lang: 'tr' } })
  client.alias({ distinctId: 'node-user-1', alias: 'node-anon-1' })
  await client.flush()

  // /flags through the SDK: the adapter always answers "no flags".
  const flag = await client.getFeatureFlag('some-flag', 'node-user-1')
  const flagEnabled = await client.isFeatureEnabled('some-flag', 'node-user-1')

  // More than flushAt events, so the SDK batches and flushes on its own.
  let sent = 3
  for (let i = 0; i < 12; i++) {
    client.capture({ distinctId: 'node-user-2', event: 'node_bulk', properties: { i, text: 'çğıöşü 🚀' } })
    sent++
  }
  await client.shutdown()

  const queue = typeof client.getPersistedProperty === 'function' ? client.getPersistedProperty('queue') : undefined
  console.log(JSON.stringify({
    errors,
    queueLength: Array.isArray(queue) ? queue.length : 0,
    flag: flag === undefined ? null : flag,
    flagEnabled: flagEnabled === undefined ? null : flagEnabled,
    sent,
  }))
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
