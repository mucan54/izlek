// Account → "Access tokens": account-wide personal access tokens (`pat_…`)
// for assistants and scripts that send a header. The token itself is shown
// once, right after it was created.
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { ApiError, asList } from '../lib/api'
import { fmtAgo, fmtDate } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { PAT_DEFAULT_EXPIRY_DAYS, PAT_EXPIRY_DAYS, PAT_NAME_MAX, scopesFor, summarizeScopes, type AccessLevel } from '../lib/mcp'
import { useApp } from '../lib/session'
import type { AccessToken } from '../lib/types'
import { Badge, Button, Card, ConfirmButton, CopyButton, Empty, ErrorNote, Field, Input, Modal, Select, Spinner, toast } from '../components/ui'

const expiryLabel = (days: number | null): string => (days == null ? S.pat.expiryNever : S.pat.expiryDays(days))

/**
 * `stamp` changes when the password or two-step verification changed, which
 * revokes every token on the server: the list is read again.
 */
export const AccessTokens = ({ stamp }: { stamp: number }) => {
  const { api } = useApp()
  const list = useAsync<AccessToken[] | null>(
    async (signal) => {
      try {
        return asList<AccessToken>(await api.get('/api/me/tokens', { signal }))
      } catch (e) {
        // a server that predates access tokens: no card at all
        if (e instanceof ApiError && e.status == 404) return null
        throw e
      }
    },
    [api, stamp],
  )
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [level, setLevel] = useState<AccessLevel>('read')
  const [expiry, setExpiry] = useState(String(PAT_DEFAULT_EXPIRY_DAYS))
  // the token lives only in this component's state and is dropped when the dialog closes
  const [created, setCreated] = useState<AccessToken | null>(null)
  const create = useAction(async () => {
    try {
      const token = await api.post<AccessToken>('/api/me/tokens', { name: name.trim(), scopes: scopesFor(level), expires_in_days: expiry == 'never' ? null : Number(expiry) })
      setCreated(token)
      list.reload()
    } catch (e) {
      if (e instanceof ApiError && e.code == 'too_many_tokens') throw new Error(S.pat.tooMany)
      throw e
    }
  })
  const close = () => {
    setOpen(false)
    setCreated(null)
    setName('')
    setLevel('read')
    setExpiry(String(PAT_DEFAULT_EXPIRY_DAYS))
    create.clear()
  }

  const tokens = list.data
  if (tokens === null) return null
  return (
    <Card
      title={S.pat.title}
      actions={
        tokens !== undefined && (
          <Button kind="primary" small onClick={() => setOpen(true)}>
            {S.pat.create}
          </Button>
        )
      }
    >
      <div class={`stack${list.loading && tokens ? ' refreshing' : ''}`} data-testid="access-tokens" data-count={tokens ? tokens.length : undefined}>
        {list.error ? (
          <ErrorNote error={list.error} onRetry={list.reload} />
        ) : tokens === undefined ? (
          <Spinner />
        ) : (
          <>
            <p class="muted">{S.pat.intro}</p>
            {tokens.length == 0 ? (
              <Empty title={S.pat.empty}>{S.pat.emptyHint}</Empty>
            ) : (
              <div class="table-wrap">
                <table class="table">
                  <thead>
                    <tr>
                      <th scope="col">{S.pat.name}</th>
                      <th scope="col">{S.pat.prefix}</th>
                      <th scope="col">{S.pat.access}</th>
                      <th scope="col">{S.common.created}</th>
                      <th scope="col">{S.pat.lastUsed}</th>
                      <th scope="col">{S.pat.expires}</th>
                      <th scope="col">
                        <span class="sr-only">{S.common.actions}</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {tokens.map((t) => {
                      const { level: access } = summarizeScopes(t.scopes)
                      return (
                        <tr key={t.id} data-token-id={t.id}>
                          <td class="strong">{t.name}</td>
                          <td>
                            <code>{t.prefix}</code>
                          </td>
                          <td>{access ? <Badge kind={access == 'admin' ? 'warn' : 'neutral'}>{S.apps.levels[access]}</Badge> : <span class="mono muted">{(t.scopes ?? []).join(', ') || '—'}</span>}</td>
                          <td class="nowrap">{fmtDate(t.created_at)}</td>
                          <td class="nowrap">{t.last_used_at ? fmtAgo(t.last_used_at) : <span class="muted">{S.pat.never}</span>}</td>
                          <td class="nowrap">{t.expires_at ? fmtDate(t.expires_at) : <span class="muted">{S.pat.noExpiry}</span>}</td>
                          <td class="cell-action">
                            <ConfirmButton
                              small
                              label={S.pat.revoke}
                              title={S.pat.revokeTitle}
                              message={S.pat.revokeMessage(t.name)}
                              onConfirm={async () => {
                                await api.del(`/api/me/tokens/${encodeURIComponent(t.id)}`)
                                toast(S.pat.revoked(t.name))
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
            <p class="muted small">{S.pat.credentialsNote}</p>
          </>
        )}
      </div>
      {open &&
        (created ? (
          <Modal title={S.pat.createdTitle} onClose={close} footer={<Button kind="primary" onClick={close}>{S.pat.done}</Button>}>
            <div class="stack">
              <p class="note note-warn" role="alert">
                {S.pat.onceWarning}
              </p>
              <div class="secret">
                <code data-testid="pat-secret">{created.token ?? S.pat.secretMissing}</code>
                {created.token && <CopyButton text={created.token} testId="pat-copy" />}
              </div>
              <p class="muted">{S.pat.usage}</p>
            </div>
          </Modal>
        ) : (
          <Modal
            title={S.pat.create}
            onClose={close}
            footer={
              <>
                <Button onClick={close}>{S.common.cancel}</Button>
                <Button kind="primary" type="submit" form="pat-form" busy={create.busy} disabled={!name.trim()}>
                  {S.common.create}
                </Button>
              </>
            }
          >
            <form
              id="pat-form"
              class="stack"
              onSubmit={(e) => {
                e.preventDefault()
                if (name.trim()) void create.run()
              }}
            >
              <Field label={S.pat.name} hint={S.pat.nameHint}>
                {(id, d) => <Input id={id} name="name" value={name} onChange={setName} required maxLength={PAT_NAME_MAX} aria-describedby={d} autocomplete="off" />}
              </Field>
              <fieldset class="form-section">
                <legend>{S.pat.access}</legend>
                {(['read', 'admin'] as const).map((l) => (
                  <label class="check" key={l}>
                    <input type="radio" name="access" value={l} checked={level == l} onChange={() => setLevel(l)} />
                    <span>{l == 'read' ? S.pat.accessRead : S.pat.accessAdmin}</span>
                  </label>
                ))}
              </fieldset>
              <Field label={S.pat.expiry}>
                {(id) => <Select id={id} value={expiry} onChange={setExpiry} options={PAT_EXPIRY_DAYS.map((d) => [d == null ? 'never' : String(d), expiryLabel(d)] as const)} />}
              </Field>
              <ErrorNote error={create.error} />
            </form>
          </Modal>
        ))}
    </Card>
  )
}
