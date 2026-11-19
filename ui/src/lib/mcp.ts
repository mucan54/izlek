// The remote MCP server of the control plane (docs/mcp.md): what the UI needs
// to know about it without talking to it — the endpoint address, whether
// OAuth sign-in is switched on, the commands and the tool list shown on the
// "AI assistants" page, and how a grant's scopes read.

/** RFC 8414 metadata; served only when OAuth is configured (LA_PUBLIC_URL). */
export const OAUTH_METADATA_PATH = '/.well-known/oauth-authorization-server'

export type McpStatus =
  /** OAuth sign-in works; `issuer` is the server's configured public origin */
  | { state: 'oauth'; issuer: string }
  /** the endpoint is served, but only secret keys are accepted */
  | { state: 'keys' }
  /** nothing answers at the metadata address but the UI itself: MCP is switched off or the server predates it */
  | { state: 'off' }
  /** the check itself failed (network, 5xx) */
  | { state: 'unknown' }

const httpOrigin = (value: unknown): string | null => {
  if (typeof value != 'string') return null
  try {
    const u = new URL(value)
    return u.protocol == 'https:' || u.protocol == 'http:' ? u.origin : null
  } catch {
    return null
  }
}

/**
 * Reads the answer to `GET /.well-known/oauth-authorization-server`.
 *
 * The control plane answers JSON or 404 there and never the single-page app,
 * *unless* MCP is switched off (or the server is older), in which case the
 * path falls through to the SPA shell: 200 with HTML. So the status alone is
 * not enough; the body has to be the metadata document.
 */
export const classifyOAuthMetadata = (status: number, body: string): McpStatus => {
  if (status == 404) return { state: 'keys' }
  if (status != 200) return { state: 'unknown' }
  try {
    const doc = JSON.parse(body) as { issuer?: unknown } | null
    const issuer = doc && typeof doc == 'object' ? httpOrigin(doc.issuer) : null
    if (issuer) return { state: 'oauth', issuer }
  } catch {
    // not JSON: the SPA shell
  }
  return { state: 'off' }
}

export const probeMcp = async (fetchFn: typeof fetch = (input, init) => fetch(input, init), signal?: AbortSignal): Promise<McpStatus> => {
  try {
    const res = await fetchFn(OAUTH_METADATA_PATH, { headers: { Accept: 'application/json' }, credentials: 'omit', signal })
    return classifyOAuthMetadata(res.status, await res.text())
  } catch (e) {
    if ((e as { name?: string })?.name == 'AbortError') throw e
    return { state: 'unknown' }
  }
}

export interface McpEndpoint {
  url: string
  /**
   * Set when the server's public address differs from the address this page
   * was opened at. Assistants must use the public one: OAuth tokens are bound
   * to exactly `<LA_PUBLIC_URL>/mcp`.
   */
  publicOrigin?: string
}

/** The address to give an assistant: the server's public origin when it is known, else the page's. */
export const mcpEndpoint = (pageOrigin: string, status: McpStatus | undefined): McpEndpoint => {
  const here = pageOrigin.replace(/\/+$/, '')
  if (status?.state == 'oauth' && status.issuer != here) return { url: status.issuer + '/mcp', publicOrigin: status.issuer }
  return { url: here + '/mcp' }
}

/** Placeholder shown where a secret key goes. The UI never puts a real key into a command. */
export const KEY_PLACEHOLDER = 'sk_…'

/** `claude mcp add` as docs/mcp.md gives it: with a secret key in a header, or bare (OAuth, then `/mcp` inside Claude Code). */
export const claudeCodeCommand = (url: string, auth: 'oauth' | 'key'): string => {
  const base = `claude mcp add --transport http izlek ${url}`
  return auth == 'key' ? `${base} \\\n  --header "Authorization: Bearer ${KEY_PLACEHOLDER}"` : base
}

export interface McpTool {
  name: string
  /** needs the `izlek:admin` scope: it changes something */
  write?: boolean
}
const read = (name: string): McpTool => ({ name })
const write = (name: string): McpTool => ({ name, write: true })

/** The tools of the server, grouped as in docs/mcp.md. */
export const MCP_TOOLS: Record<'setup' | 'flags' | 'analytics', McpTool[]> = {
  setup: [
    read('list_projects'),
    write('create_project'),
    read('get_project'),
    write('update_project_settings'),
    read('list_write_tokens'),
    write('create_write_token'),
    write('update_write_token'),
    read('list_secret_keys'),
    write('create_secret_key'),
    read('get_sdk_setup'),
    read('verify_installation'),
  ],
  flags: [read('list_flags'), read('get_flag'), write('create_flag'), write('update_flag'), read('evaluate_flag')],
  analytics: [read('query_trends'), read('query_funnel'), read('query_retention'), read('query_events'), read('run_sql'), read('get_schema')],
}

export const SCOPE_READ = 'izlek:read'
export const SCOPE_ADMIN = 'izlek:admin'

export interface ScopeSummary {
  /** `admin` implies `read`; null when the grant carries neither */
  level: 'read' | 'admin' | null
  /** scopes this UI does not know, shown as they are */
  other: string[]
}

export const summarizeScopes = (scopes: readonly string[] | null | undefined): ScopeSummary => {
  const list = scopes ?? []
  return {
    level: list.includes(SCOPE_ADMIN) ? 'admin' : list.includes(SCOPE_READ) ? 'read' : null,
    other: list.filter((s) => s != SCOPE_ADMIN && s != SCOPE_READ),
  }
}

/** Host of a client's metadata document (`client_id` of a `cimd` client is its URL); null for anything else. */
export const clientSite = (app: { client_id: string; client_kind: string }): string | null => {
  if (app.client_kind != 'cimd') return null
  try {
    return new URL(app.client_id).host || null
  } catch {
    return null
  }
}
