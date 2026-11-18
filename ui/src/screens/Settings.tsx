// Project settings: general, write tokens, secret keys, SDK snippets.
import { useEffect, useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { fmtAgo, fmtDate } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { can } from '../lib/roles'
import { navigate } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import { android, ios, webNpm, webScript } from '../lib/snippets'
import type { Key, KeyScope, Project, Token } from '../lib/types'
import { AsyncView, Badge, Button, Card, Checkbox, ConfirmButton, CopyButton, Empty, ErrorNote, Field, Input, Modal, PageHead, Select, Switch, Tabs, toast } from '../components/ui'

type Tab = 'general' | 'tokens' | 'keys' | 'sdk'

const lines = (text: string): string[] => [...new Set(text.split(/[\n,]/).map((s) => s.trim()).filter(Boolean))]

const General = () => {
  const { project, base, role } = useProject()
  const app = useApp()
  const canWrite = can(role, 'admin')
  const [name, setName] = useState(project.name)
  const [retention, setRetention] = useState(String(project.retention_days))
  const [replayRetention, setReplayRetention] = useState(String(project.replay_retention_days))
  const [sample, setSample] = useState(String(Math.round(project.replay_sample_rate * 10000) / 100))
  const [dropKeys, setDropKeys] = useState(project.drop_keys.join('\n'))
  const intIn = (s: string, min: number, max: number) => /^\d+$/.test(s.trim()) && Number(s) >= min && Number(s) <= max
  const sampleOk = /^\d{1,3}([.,]\d{1,2})?$/.test(sample.trim()) && Number(sample.replace(',', '.')) <= 100
  const valid = name.trim() != '' && intIn(retention, 1, 3650) && intIn(replayRetention, 1, 3650) && sampleOk
  const save = useAction(async () => {
    await app.api.patch<Project>(base, {
      name: name.trim(),
      retention_days: Number(retention),
      replay_retention_days: Number(replayRetention),
      replay_sample_rate: Math.round(Number(sample.replace(',', '.')) * 100) / 10000,
      drop_keys: lines(dropKeys),
    })
    await app.refresh()
    toast(S.settings.saved)
  })
  return (
    <div class="stack">
      <Card title={S.settings.general}>
        <form
          class="stack form-narrow"
          onSubmit={(e) => {
            e.preventDefault()
            if (valid) void save.run()
          }}
        >
          <Field label={S.projects.name}>{(id) => <Input id={id} name="name" value={name} onChange={setName} required maxLength={100} disabled={!canWrite} />}</Field>
          <div class="form-grid form-grid-2">
            <Field label={S.settings.retention} hint={S.settings.retentionHint} error={intIn(retention, 1, 3650) ? null : S.settings.daysError}>
              {(id, d) => (
                <div class="row">
                  <Input id={id} name="retention_days" class="input-num" value={retention} onChange={setRetention} inputMode="numeric" aria-describedby={d} disabled={!canWrite} />
                  <span>{S.units.day}</span>
                </div>
              )}
            </Field>
            <Field label={S.settings.replayRetention} hint={S.settings.replayRetentionHint} error={intIn(replayRetention, 1, 3650) ? null : S.settings.daysError}>
              {(id, d) => (
                <div class="row">
                  <Input id={id} name="replay_retention_days" class="input-num" value={replayRetention} onChange={setReplayRetention} inputMode="numeric" aria-describedby={d} disabled={!canWrite} />
                  <span>{S.units.day}</span>
                </div>
              )}
            </Field>
          </div>
          <Field label={S.settings.sampleRate} hint={S.settings.sampleRateHint} error={sampleOk ? null : S.settings.sampleError}>
            {(id, d) => (
              <div class="row">
                <Input id={id} name="replay_sample_rate" class="input-num" value={sample} onChange={setSample} inputMode="decimal" aria-describedby={d} disabled={!canWrite} />
                <span>%</span>
              </div>
            )}
          </Field>
          <Field label={S.settings.dropKeys} hint={S.settings.dropKeysHint}>
            {(id, d) => <textarea id={id} name="drop_keys" class="input mono" rows={4} spellcheck={false} value={dropKeys} aria-describedby={d} disabled={!canWrite} onInput={(e) => setDropKeys(e.currentTarget.value)} />}
          </Field>
          <dl class="kv">
            <dt>{S.projects.id}</dt>
            <dd>{project.id}</dd>
            <dt>{S.settings.scanLimit}</dt>
            <dd>{project.scan_limit_bytes ? project.scan_limit_bytes : S.settings.unlimited}</dd>
            <dt>{S.settings.maxConcurrent}</dt>
            <dd>{project.max_concurrent_queries}</dd>
            <dt>{S.common.created}</dt>
            <dd>{fmtDate(project.created_at)}</dd>
          </dl>
          <ErrorNote error={save.error} />
          {canWrite ? (
            <div>
              <Button kind="primary" type="submit" busy={save.busy} disabled={!valid}>
                {S.common.save}
              </Button>
            </div>
          ) : (
            <p class="muted">{S.settings.readOnly}</p>
          )}
        </form>
      </Card>
      {canWrite && (
        <Card title={S.settings.danger} class="card-danger">
          <p>{S.settings.closeIntro}</p>
          <ConfirmButton
            label={S.settings.close}
            title={S.settings.closeTitle(project.name)}
            message={S.settings.closeMessage}
            onConfirm={async () => {
              await app.api.del(base)
              await app.refresh()
              toast(S.settings.closed)
              navigate('/projects')
            }}
          />
        </Card>
      )}
    </div>
  )
}

interface TokenForm {
  name: string
  origins: string
  rateLimit: string
  replay: boolean
}

const TokenDialog = ({ token, onClose, onSaved }: { token: Token | null; onClose: () => void; onSaved: () => void }) => {
  const { base } = useProject()
  const app = useApp()
  const [form, setForm] = useState<TokenForm>({
    name: token?.name ?? '',
    origins: (token?.allowed_origins ?? []).join('\n'),
    rateLimit: token?.rate_limit ? String(token.rate_limit) : '',
    replay: token?.replay ?? false,
  })
  const origins = lines(form.origins)
  const badOrigin = origins.find((o) => !/^https?:\/\/[^/\s]+$/.test(o))
  const rateOk = form.rateLimit.trim() == '' || /^\d{1,7}$/.test(form.rateLimit.trim())
  const save = useAction(async () => {
    const body = { name: form.name.trim(), allowed_origins: origins, rate_limit: form.rateLimit.trim() ? Number(form.rateLimit) : 0, replay: form.replay }
    if (token) await app.api.patch(`${base}/tokens/${encodeURIComponent(token.token)}`, body)
    else await app.api.post(`${base}/tokens`, body)
    onSaved()
    onClose()
  })
  return (
    <Modal
      title={token ? S.tokens.edit : S.tokens.create}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>{S.common.cancel}</Button>
          <Button kind="primary" type="submit" form="token-form" busy={save.busy} disabled={!!badOrigin || !rateOk}>
            {token ? S.common.save : S.common.create}
          </Button>
        </>
      }
    >
      <form
        id="token-form"
        class="stack"
        onSubmit={(e) => {
          e.preventDefault()
          void save.run()
        }}
      >
        <Field label={S.tokens.name} hint={S.tokens.nameHint}>
          {(id, d) => <Input id={id} name="name" value={form.name} onChange={(name) => setForm({ ...form, name })} required maxLength={100} aria-describedby={d} />}
        </Field>
        <Field label={S.tokens.origins} hint={S.tokens.originsHint} error={badOrigin ? S.tokens.originError(badOrigin) : null}>
          {(id, d) => (
            <textarea id={id} name="origins" class="input mono" rows={3} spellcheck={false} placeholder="https://app.example.com" value={form.origins} aria-describedby={d} onInput={(e) => setForm({ ...form, origins: e.currentTarget.value })} />
          )}
        </Field>
        <Field label={S.tokens.rateLimit} hint={S.tokens.rateLimitHint} error={rateOk ? null : S.tokens.rateError}>
          {(id, d) => <Input id={id} name="rate_limit" class="input-num" value={form.rateLimit} onChange={(rateLimit) => setForm({ ...form, rateLimit })} inputMode="numeric" placeholder={S.tokens.rateDefault} aria-describedby={d} />}
        </Field>
        <Switch checked={form.replay} onChange={(replay) => setForm({ ...form, replay })} label={S.tokens.replay} />
        <ErrorNote error={save.error} />
      </form>
    </Modal>
  )
}

const Tokens = () => {
  const { base, role } = useProject()
  const app = useApp()
  const canWrite = can(role, 'admin')
  const list = useAsync(async (signal) => asList<Token>(await app.api.get(`${base}/tokens`, { signal })), [base])
  const [dialog, setDialog] = useState<Token | 'new' | null>(null)
  const setStatus = async (t: Token, status: Token['status']) => {
    try {
      await app.api.patch(`${base}/tokens/${encodeURIComponent(t.token)}`, { status })
      list.reload()
    } catch (e) {
      toast((e as Error).message, 'error')
    }
  }
  return (
    <Card
      title={S.tokens.title}
      actions={
        canWrite && (
          <Button kind="primary" small onClick={() => setDialog('new')}>
            {S.tokens.create}
          </Button>
        )
      }
    >
      <p class="muted">{S.tokens.intro}</p>
      <AsyncView state={list} empty={<Empty title={S.tokens.empty} />}>
        {(tokens) => (
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">
                    {S.tokens.name} / {S.tokens.token}
                  </th>
                  <th scope="col">{S.tokens.status}</th>
                  <th scope="col">{S.tokens.origins}</th>
                  <th scope="col" class="num">
                    {S.tokens.rateLimit}
                  </th>
                  <th scope="col">{S.tokens.replayShort}</th>
                  <th scope="col">
                    <span class="sr-only">{S.common.actions}</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((t) => (
                  <tr key={t.token} data-token={t.token}>
                    <td>
                      <div class="strong">{t.name}</div>
                      <div class="token-line">
                        <code class="token">{t.token}</code> <CopyButton text={t.token} />
                      </div>
                    </td>
                    <td>
                      <Badge kind={t.status == 'active' ? 'ok' : 'warn'}>{t.status == 'active' ? S.tokens.active : S.tokens.disabled}</Badge>
                    </td>
                    <td>{t.allowed_origins.length ? t.allowed_origins.join(', ') : <span class="muted">{S.tokens.anyOrigin}</span>}</td>
                    <td class="num">{t.rate_limit ? t.rate_limit : <span class="muted">{S.tokens.rateDefault}</span>}</td>
                    <td>{t.replay ? S.common.on : S.common.off}</td>
                    <td class="cell-action">
                      {canWrite && (
                        <div class="row">
                          <Button small onClick={() => setDialog(t)}>
                            {S.common.edit}
                          </Button>
                          <Button small onClick={() => void setStatus(t, t.status == 'active' ? 'disabled' : 'active')}>
                            {t.status == 'active' ? S.tokens.disable : S.tokens.enable}
                          </Button>
                          <ConfirmButton
                            small
                            label={S.common.delete}
                            title={S.tokens.deleteTitle}
                            message={S.tokens.deleteMessage(t.name)}
                            onConfirm={async () => {
                              await app.api.del(`${base}/tokens/${encodeURIComponent(t.token)}`)
                              list.reload()
                            }}
                          />
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncView>
      {dialog && <TokenDialog token={dialog == 'new' ? null : dialog} onClose={() => setDialog(null)} onSaved={list.reload} />}
    </Card>
  )
}

const Keys = () => {
  const { base } = useProject()
  const app = useApp()
  const list = useAsync(async (signal) => asList<Key>(await app.api.get(`${base}/keys`, { signal })), [base])
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [scopes, setScopes] = useState<Record<KeyScope, boolean>>({ query: true, admin: false })
  // the secret lives only in this component's state and is dropped when the dialog closes
  const [created, setCreated] = useState<Key | null>(null)
  const chosen = (Object.keys(scopes) as KeyScope[]).filter((s) => scopes[s])
  const create = useAction(async () => {
    const key = await app.api.post<Key>(`${base}/keys`, { name: name.trim(), scopes: chosen })
    setCreated(key)
    setName('')
    list.reload()
  })
  const close = () => {
    setOpen(false)
    setCreated(null)
    create.clear()
  }
  return (
    <Card
      title={S.keys.title}
      actions={
        <Button kind="primary" small onClick={() => setOpen(true)}>
          {S.keys.create}
        </Button>
      }
    >
      <p class="muted">{S.keys.intro}</p>
      <AsyncView state={list} empty={<Empty title={S.keys.empty} />}>
        {(keys) => (
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">{S.keys.name}</th>
                  <th scope="col">{S.keys.prefix}</th>
                  <th scope="col">{S.keys.scopes}</th>
                  <th scope="col">{S.common.created}</th>
                  <th scope="col">{S.keys.lastUsed}</th>
                  <th scope="col">
                    <span class="sr-only">{S.common.actions}</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id}>
                    <td>{k.name}</td>
                    <td>
                      <code>{k.prefix}</code>
                    </td>
                    <td>{k.scopes.map((s) => S.keys.scopeNames[s] ?? s).join(', ')}</td>
                    <td class="nowrap">{fmtDate(k.created_at)}</td>
                    <td class="nowrap">{k.last_used_at ? fmtAgo(k.last_used_at) : <span class="muted">{S.keys.never}</span>}</td>
                    <td class="cell-action">
                      <ConfirmButton
                        small
                        label={S.common.delete}
                        title={S.keys.deleteTitle}
                        message={S.keys.deleteMessage(k.name)}
                        onConfirm={async () => {
                          await app.api.del(`${base}/keys/${encodeURIComponent(String(k.id))}`)
                          list.reload()
                        }}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncView>
      {open &&
        (created ? (
          <Modal title={S.keys.createdTitle} onClose={close} footer={<Button kind="primary" onClick={close}>{S.keys.done}</Button>}>
            <div class="stack">
              <p class="note note-warn" role="alert">
                {S.keys.onceWarning}
              </p>
              <div class="secret">
                <code data-testid="secret">{created.secret ?? S.keys.secretMissing}</code>
                {created.secret && <CopyButton text={created.secret} />}
              </div>
              <p class="muted">{S.keys.usage}</p>
            </div>
          </Modal>
        ) : (
          <Modal
            title={S.keys.create}
            onClose={close}
            footer={
              <>
                <Button onClick={close}>{S.common.cancel}</Button>
                <Button kind="primary" type="submit" form="key-form" busy={create.busy} disabled={!chosen.length}>
                  {S.common.create}
                </Button>
              </>
            }
          >
            <form
              id="key-form"
              class="stack"
              onSubmit={(e) => {
                e.preventDefault()
                void create.run()
              }}
            >
              <Field label={S.keys.name}>{(id) => <Input id={id} name="name" value={name} onChange={setName} required maxLength={100} />}</Field>
              <fieldset class="form-section">
                <legend>{S.keys.scopes}</legend>
                <Checkbox checked={scopes.query} onChange={(query) => setScopes({ ...scopes, query })} label={S.keys.scopeQuery} />
                <Checkbox checked={scopes.admin} onChange={(admin) => setScopes({ ...scopes, admin })} label={S.keys.scopeAdmin} />
              </fieldset>
              <ErrorNote error={create.error} />
            </form>
          </Modal>
        ))}
    </Card>
  )
}

const HOST_KEY = 'izlek.ingestHost'
const readHost = (): string => {
  try {
    return localStorage.getItem(HOST_KEY) ?? ''
  } catch {
    return ''
  }
}

const Sdk = () => {
  const { base, project } = useProject()
  const app = useApp()
  const list = useAsync(async (signal) => asList<Token>(await app.api.get(`${base}/tokens`, { signal })), [base])
  const [token, setToken] = useState('')
  const [host, setHost] = useState(readHost)
  const [platform, setPlatform] = useState<'web' | 'script' | 'android' | 'ios'>('web')
  const active = (list.data ?? []).filter((t) => t.status == 'active')
  useEffect(() => {
    if (!token && active.length) setToken(active[0].token)
  }, [active.length])
  useEffect(() => {
    try {
      if (host.trim()) localStorage.setItem(HOST_KEY, host.trim())
    } catch {
      // private mode: the host simply is not remembered
    }
  }, [host])
  const selected = active.find((t) => t.token == token)
  const input = { token: token || 'pk_…', host: host.trim() || 'https://in.example.com', sampleRate: project.replay_sample_rate, replay: !!selected?.replay }
  const code = platform == 'web' ? webNpm(input) : platform == 'script' ? webScript(input) : platform == 'android' ? android(input) : ios(input)
  return (
    <Card title={S.sdk.title}>
      <AsyncView state={list}>
        {() => (
          <div class="stack">
            <div class="form-grid form-grid-2">
              <Field label={S.sdk.token} hint={active.length ? (selected?.replay ? S.sdk.replayOn : S.sdk.replayOff) : S.sdk.noToken}>
                {(id, d) => <Select id={id} aria-describedby={d} value={token} onChange={setToken} options={active.map((t) => [t.token, `${t.name} — ${t.token.slice(0, 9)}…`] as const)} />}
              </Field>
              <Field label={S.sdk.host} hint={S.sdk.hostHint}>
                {(id, d) => <Input id={id} name="host" value={host} onChange={setHost} placeholder="https://in.example.com" aria-describedby={d} inputMode="url" />}
              </Field>
            </div>
            <Tabs
              label={S.sdk.platform}
              value={platform}
              onChange={setPlatform}
              tabs={[
                ['web', S.sdk.web],
                ['script', S.sdk.script],
                ['android', 'Android'],
                ['ios', 'iOS'],
              ]}
            />
            <div class="snippet">
              <div class="snippet-tools">
                <CopyButton text={code} />
              </div>
              <pre>
                <code data-testid="snippet">{code}</code>
              </pre>
            </div>
            <p class="muted">{S.sdk.note}</p>
          </div>
        )}
      </AsyncView>
    </Card>
  )
}

export const Settings = ({ tab }: { tab: string }) => {
  const project = useProject()
  const admin = can(project.role, 'admin')
  const tabs: (readonly [Tab, string])[] = [
    ['general', S.settings.general],
    ['tokens', S.tokens.title],
    ...(admin ? [['keys', S.keys.title] as const] : []),
    ['sdk', S.sdk.title],
  ]
  const current = (tabs.some(([t]) => t == tab) ? tab : 'general') as Tab
  return (
    <>
      <PageHead title={S.settings.title} sub={project.project.name} />
      <Tabs label={S.settings.title} value={current} onChange={(t) => navigate(project.to('settings/' + t))} tabs={tabs} />
      {current == 'general' && <General key={project.project.id} />}
      {current == 'tokens' && <Tokens />}
      {current == 'keys' && <Keys />}
      {current == 'sdk' && <Sdk />}
    </>
  )
}
