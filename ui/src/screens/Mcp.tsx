// The control plane's MCP server as seen from the UI (docs/mcp.md): the
// "AI assistants" tab of the project settings (how to connect Claude and
// ChatGPT, what they can do) and the "Connected apps" card of the account
// page (OAuth grants of the signed-in user, with revoke).
import type { ComponentChildren } from 'preact'
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { ApiError, asList } from '../lib/api'
import { fmtAgo, fmtDate } from '../lib/format'
import { useAsync } from '../lib/hooks'
import { MCP_TOOLS, claudeCodeCommand, clientSite, mcpEndpoint, probeMcp, summarizeScopes, type McpTool } from '../lib/mcp'
import { can } from '../lib/roles'
import { Link } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import type { ConnectedApp } from '../lib/types'
import { Badge, Card, ConfirmButton, CopyButton, Empty, ErrorNote, Field, Spinner, Tabs, toast } from '../components/ui'

/** A command or URL to copy; never contains a real secret. */
const Command = ({ code, testId }: { code: string; testId?: string }) => (
  <div class="snippet snippet-cmd">
    <div class="snippet-tools">
      <CopyButton text={code} />
    </div>
    <pre>
      <code data-testid={testId}>{code}</code>
    </pre>
  </div>
)

const Steps = ({ children }: { children: ComponentChildren }) => <ol class="steps">{children}</ol>

const ToolList = ({ tools }: { tools: McpTool[] }) => (
  <ul class="tool-list">
    {tools.map((t) => (
      <li key={t.name}>
        <code class={`tool${t.write ? ' tool-write' : ''}`}>
          {t.name}
          {t.write && (
            <>
              <span aria-hidden="true"> ✎</span>
              <span class="sr-only"> ({S.mcp.writeMark})</span>
            </>
          )}
        </code>
      </li>
    ))}
  </ul>
)

type Client = 'code' | 'claude' | 'chatgpt'

export const AiAssistants = () => {
  const project = useProject()
  const probe = useAsync((signal) => probeMcp(undefined, signal), [])
  const [client, setClient] = useState<Client>('code')

  const status = probe.data
  const state = status?.state ?? 'checking'
  const endpoint = mcpEndpoint(location.origin, status)
  // claude.ai and ChatGPT can only sign in with OAuth
  const oauthMissing = state == 'keys' || state == 'off'
  const url = endpoint.url

  return (
    <div class="stack">
      <Card title={S.mcp.title}>
        <div class="stack">
          <div class="prose">
            <p>{S.mcp.intro}</p>
            <p class="muted">{S.mcp.intro2}</p>
          </div>
          <Field label={S.mcp.endpoint} hint={S.mcp.endpointHint} class="form-narrow">
            {(id, d) => (
              <div class="row">
                <input id={id} class="input mono grow" readOnly value={url} data-testid="mcp-url" aria-describedby={d} onFocus={(e) => e.currentTarget.select()} />
                <CopyButton text={url} testId="mcp-url-copy" />
              </div>
            )}
          </Field>
          {endpoint.publicOrigin && (
            <p class="note" role="note">
              {S.mcp.publicOriginNote(endpoint.publicOrigin)}
            </p>
          )}
          <p class={`note${oauthMissing ? ' note-warn' : ''}`} role="status" data-testid="mcp-status" data-state={state}>
            {S.mcp.status[state]}
          </p>
        </div>
      </Card>

      <Card title={S.mcp.connectTitle}>
        <div class="stack">
          <Tabs
            label={S.mcp.clientsLabel}
            value={client}
            onChange={setClient}
            tabs={[
              ['code', S.mcp.clients.code],
              ['claude', S.mcp.clients.claude],
              ['chatgpt', S.mcp.clients.chatgpt],
            ]}
          />
          {client == 'code' && (
            <div class="stack" data-testid="mcp-client-code">
              <section class="prose">
                <h3>{S.mcp.code.patTitle}</h3>
                <p class="muted">{S.mcp.code.patIntro}</p>
                <Command code={claudeCodeCommand(url, 'pat')} testId="mcp-cmd-pat" />
                <p>
                  {S.mcp.code.patHelp} <Link href="/account">{S.mcp.code.patLink}</Link>
                </p>
              </section>
              <section class="prose">
                <h3>{S.mcp.code.keyTitle}</h3>
                <p class="muted">{S.mcp.code.keyIntro}</p>
                <Command code={claudeCodeCommand(url, 'key')} testId="mcp-cmd-key" />
                <p>
                  {S.mcp.code.keyHelp} {can(project.role, 'admin') ? <Link href={project.to('settings/keys')}>{S.mcp.code.keyLink}</Link> : <span class="muted">{S.mcp.code.keyAdminOnly}</span>}
                </p>
                <p class="muted">{S.mcp.code.keyWarning}</p>
              </section>
              <section class="prose">
                <h3>{S.mcp.code.oauthTitle}</h3>
                {oauthMissing ? (
                  <p class="muted" data-testid="mcp-code-oauth-off">
                    {S.mcp.code.oauthUnavailable}
                  </p>
                ) : (
                  <>
                    <p class="muted">{S.mcp.code.oauthIntro}</p>
                    <Command code={claudeCodeCommand(url, 'oauth')} testId="mcp-cmd-oauth" />
                    <p>{S.mcp.code.oauthAfter}</p>
                  </>
                )}
              </section>
              <p class="muted">
                {S.mcp.code.check} <code>claude mcp get izlek</code>
              </p>
            </div>
          )}
          {client == 'claude' && (
            <div class="stack" data-testid="mcp-client-claude">
              {oauthMissing && (
                <p class="note note-warn" role="note">
                  {S.mcp.needsOAuth}
                </p>
              )}
              <p class="muted">{S.mcp.needsInternet}</p>
              <Steps>
                <li>{S.mcp.claude.step1}</li>
                <li>{S.mcp.claude.step2}</li>
                <li>{S.mcp.claude.step3(url)}</li>
                <li>{S.mcp.claude.step4}</li>
                <li>{S.mcp.claude.step5}</li>
              </Steps>
              <p>{S.mcp.claude.desktop}</p>
              <p class="muted small">{S.mcp.unverified('Anthropic')}</p>
            </div>
          )}
          {client == 'chatgpt' && (
            <div class="stack" data-testid="mcp-client-chatgpt">
              {oauthMissing && (
                <p class="note note-warn" role="note">
                  {S.mcp.needsOAuth}
                </p>
              )}
              <p class="muted">{S.mcp.needsInternet}</p>
              <Steps>
                <li>
                  {S.mcp.chatgpt.step1} <span class="muted">{S.mcp.chatgpt.step1Note}</span>
                </li>
                <li>{S.mcp.chatgpt.step2(url)}</li>
                <li>{S.mcp.chatgpt.step3}</li>
                <li>{S.mcp.chatgpt.step4}</li>
              </Steps>
              <div class="prose">
                <p>{S.mcp.chatgpt.confirm}</p>
                <p>{S.mcp.chatgpt.noSearch}</p>
              </div>
              <p class="muted small">{S.mcp.unverified('OpenAI')}</p>
            </div>
          )}
        </div>
      </Card>

      <Card title={S.mcp.canTitle}>
        <div class="stack" data-testid="mcp-tools">
          {(['setup', 'flags', 'analytics'] as const).map((group) => (
            <section class="prose" key={group}>
              <h3>{S.mcp.groups[group]}</h3>
              <p>{S.mcp.groupHelp[group]}</p>
              <ToolList tools={MCP_TOOLS[group]} />
            </section>
          ))}
          <p class="muted small">
            <span aria-hidden="true">✎ </span>
            {S.mcp.writeLegend}
          </p>
          <section class="prose">
            <h3>{S.mcp.cannotTitle}</h3>
            <ul class="bullets">
              <li>{S.mcp.cannotItems.close}</li>
              <li>{S.mcp.cannotItems.remove}</li>
              <li>{S.mcp.cannotItems.members}</li>
              <li>{S.mcp.cannotItems.deletions}</li>
            </ul>
            <p class="muted">{S.mcp.cannotNote}</p>
          </section>
        </div>
      </Card>

      <Card title={S.mcp.securityTitle}>
        <ul class="bullets prose">
          <li>{S.mcp.security.acts}</li>
          <li>{S.mcp.security.readOnly}</li>
          <li>{S.mcp.security.injection}</li>
          <li>{S.mcp.security.secrets}</li>
          <li>{S.mcp.security.audit}</li>
          <li>
            {S.mcp.security.revoke} <Link href="/account">{S.mcp.security.revokeLink}</Link>. {S.mcp.security.revokeKeys}
          </li>
        </ul>
      </Card>
    </div>
  )
}

/** What a grant allows, in plain words. */
const Access = ({ scopes }: { scopes: string[] }) => {
  const { level, other } = summarizeScopes(scopes)
  return (
    <>
      {level && <Badge kind={level == 'admin' ? 'warn' : 'neutral'}>{S.apps.levels[level]}</Badge>}
      {level && <div>{S.apps.scopeRead}</div>}
      {level == 'admin' && <div>{S.apps.scopeAdmin}</div>}
      {other.length > 0 && <div class="mono muted">{other.join(', ')}</div>}
    </>
  )
}

/**
 * Applications the signed-in user approved on the OAuth consent page.
 * `stamp` changes when the password or two-step verification changed, which
 * revokes every grant on the server: the list is read again.
 */
export const ConnectedApps = ({ stamp }: { stamp: number }) => {
  const { api, projects } = useApp()
  const list = useAsync<ConnectedApp[] | null>(
    async (signal) => {
      try {
        return asList<ConnectedApp>(await api.get('/api/me/connected-apps', { signal }))
      } catch (e) {
        // MCP is switched off, or the server predates it: say so quietly instead of showing an error
        if (e instanceof ApiError && e.status == 404) return null
        throw e
      }
    },
    [api, stamp],
  )
  const help = projects.length ? `/p/${projects[0].id}/settings/mcp` : null
  const apps = list.data
  return (
    <Card title={S.apps.title}>
      <div class={`stack${list.loading && apps ? ' refreshing' : ''}`} data-testid="connected-apps" data-count={apps ? apps.length : undefined}>
        {list.error ? (
          <ErrorNote error={list.error} onRetry={list.reload} />
        ) : apps === undefined ? (
          <Spinner />
        ) : apps === null ? (
          <p class="muted" data-testid="connected-apps-off">
            {S.apps.notEnabled}
          </p>
        ) : (
          <>
            <p class="muted">{S.apps.intro}</p>
            {apps.length == 0 ? (
              <Empty title={S.apps.empty}>
                {S.apps.emptyHint}
                {help && (
                  <p>
                    <Link href={help}>{S.apps.emptyLink}</Link>
                  </p>
                )}
              </Empty>
            ) : (
              <div class="table-wrap">
                <table class="table">
                  <thead>
                    <tr>
                      <th scope="col">{S.apps.app}</th>
                      <th scope="col">{S.apps.sentTo}</th>
                      <th scope="col">{S.apps.access}</th>
                      <th scope="col">{S.apps.connected}</th>
                      <th scope="col">{S.apps.lastUsed}</th>
                      <th scope="col">{S.apps.expires}</th>
                      <th scope="col">
                        <span class="sr-only">{S.common.actions}</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {apps.map((a) => {
                      const name = a.client_name || a.client_id
                      const site = clientSite(a)
                      return (
                        <tr key={a.id} data-app-id={a.id}>
                          <td>
                            <div class={a.client_name ? 'strong' : 'mono'}>{name}</div>
                            {a.client_kind == 'dcr' ? (
                              <span title={S.apps.unverifiedHint}>
                                <Badge>{S.apps.unverified}</Badge>
                              </span>
                            ) : (
                              site && <div class="muted small">{S.apps.site(site)}</div>
                            )}
                          </td>
                          <td class="mono">{a.redirect_hosts?.length ? a.redirect_hosts.join(', ') : '—'}</td>
                          <td class="app-access">
                            <Access scopes={a.scopes ?? []} />
                          </td>
                          <td class="nowrap">{fmtDate(a.created_at)}</td>
                          <td class="nowrap">{a.last_used_at ? fmtAgo(a.last_used_at) : <span class="muted">{S.apps.never}</span>}</td>
                          <td class="nowrap">{fmtDate(a.expires_at)}</td>
                          <td class="cell-action">
                            <ConfirmButton
                              small
                              label={S.apps.revoke}
                              title={S.apps.revokeTitle}
                              message={S.apps.revokeMessage(name)}
                              onConfirm={async () => {
                                await api.del(`/api/me/connected-apps/${encodeURIComponent(String(a.id))}`)
                                toast(S.apps.revoked(name))
                                list.reload()
                              }}
                            />
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <p class="muted small">
              {S.apps.credentialsNote}
              {help && apps.length > 0 && (
                <>
                  {' '}
                  <Link href={help}>{S.apps.emptyLink}</Link>
                </>
              )}
            </p>
          </>
        )}
      </div>
    </Card>
  )
}
