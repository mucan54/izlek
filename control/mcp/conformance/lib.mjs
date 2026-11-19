// Shared helpers of the conformance scripts: configuration from the
// environment, a driver for the consent page, and an in-memory OAuth client
// provider that records what the official client asks it to do.
import assert from 'node:assert/strict'

export const env = (name) => {
  const v = process.env[name]
  assert.ok(v, `${name} is not set (the Go test sets it)`)
  return v
}

export const MCP_URL = env('IZLEK_MCP_URL')
export const BASE_URL = env('IZLEK_BASE_URL')
export const SECRET_KEY = env('IZLEK_SECRET_KEY')
export const SESSION_COOKIE = env('IZLEK_SESSION_COOKIE')
export const PROJECT_ID = Number(env('IZLEK_PROJECT_ID'))
export const WRITE_TOKEN = env('IZLEK_WRITE_TOKEN')
export const CONTROL_URL = env('IZLEK_CONTROL_URL')
export const CIMD_CLIENT_ID = env('IZLEK_CIMD_CLIENT_ID')

/** Moves the server's clock forward. */
export const advanceClock = async (seconds) => {
  const res = await fetch(`${CONTROL_URL}/advance?seconds=${seconds}`)
  assert.equal(res.status, 200, 'clock control')
}

const unescapeHTML = (s) =>
  s
    .replace(/&#(\d+);/g, (_, n) => String.fromCodePoint(Number(n)))
    .replace(/&#x([0-9a-f]+);/gi, (_, n) => String.fromCodePoint(parseInt(n, 16)))
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&amp;/g, '&')

const hidden = (html, name) => {
  const m = html.match(new RegExp(`<input type="hidden" name="${name}" value="([^"]*)"`))
  assert.ok(m, `the page has no hidden field "${name}"`)
  return unescapeHTML(m[1])
}

/**
 * Plays the user on the consent page: loads the authorization URL with the
 * owner's session cookie, checks what the page shows and presses a button.
 * Returns the parameters of the redirect back to the client.
 */
export const consent = async (authorizationUrl, { action = 'approve', grantAdmin = true, expectClient } = {}) => {
  const cookie = `la_session=${SESSION_COOKIE}`
  const page = await fetch(authorizationUrl, { redirect: 'manual', headers: { cookie, 'accept-language': 'en' } })
  const html = await page.text()
  assert.equal(page.status, 200, `consent page: ${html.slice(0, 300)}`)
  assert.match(page.headers.get('content-security-policy') ?? '', /frame-ancestors 'none'/)
  assert.equal(page.headers.get('x-frame-options'), 'DENY')
  assert.match(html, /Connect an application/)
  assert.match(html, /owner@example\.com/, 'the page says which account approves')
  if (expectClient) assert.ok(html.includes(expectClient), `the page names the client "${expectClient}"`)
  const redirectHost = new URL(new URL(authorizationUrl).searchParams.get('redirect_uri')).host
  assert.ok(html.includes(redirectHost), `the page shows the redirect host ${redirectHost}`)

  const form = new URLSearchParams({ q: hidden(html, 'q'), csrf: hidden(html, 'csrf'), action })
  if (grantAdmin) form.set('grant_admin', '1')
  const post = await fetch(new URL('/oauth/authorize', BASE_URL), {
    method: 'POST',
    redirect: 'manual',
    headers: { cookie, 'content-type': 'application/x-www-form-urlencoded', origin: BASE_URL, 'sec-fetch-site': 'same-origin' },
    body: form,
  })
  assert.equal(post.status, 303, `consent submit: ${await post.text()}`)
  const back = new URL(post.headers.get('location'))
  return { url: back, params: back.searchParams, html }
}

/** An OAuthClientProvider kept in memory. `overrides` adds or replaces members. */
export const memoryProvider = ({ redirectUrl, clientMetadata, overrides = {} }) => {
  const store = { client: undefined, tokens: undefined, verifier: undefined, authorizationUrl: undefined, state: undefined, tokenHistory: [] }
  const provider = {
    get redirectUrl() {
      return redirectUrl
    },
    get clientMetadata() {
      return clientMetadata
    },
    state() {
      store.state = `state-${Math.random().toString(36).slice(2)}`
      return store.state
    },
    clientInformation: () => store.client,
    saveClientInformation: (info) => {
      store.client = info
    },
    tokens: () => store.tokens,
    saveTokens: (tokens) => {
      store.tokens = tokens
      store.tokenHistory.push(tokens)
    },
    redirectToAuthorization: (url) => {
      store.authorizationUrl = new URL(url)
    },
    saveCodeVerifier: (v) => {
      store.verifier = v
    },
    codeVerifier: () => store.verifier,
    // 2.x clients bind the callback to the authorization server they started with.
    saveDiscoveryState: (state) => {
      store.discovery = state
    },
    discoveryState: () => store.discovery,
    ...overrides,
  }
  return { provider, store }
}

/** Runs fn and returns the error it throws. */
export const rejection = async (fn) => {
  try {
    await fn()
  } catch (err) {
    return err
  }
  assert.fail('expected the call to fail')
}

export const text = (result) => result.content.map((c) => c.text).join('\n')

/** A raw MCP POST without any client library. */
export const rawPost = (body, headers = {}) =>
  fetch(MCP_URL, {
    method: 'POST',
    headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream', ...headers },
    body: JSON.stringify(body),
  })
