// First-run setup, login (with the TOTP step) and the account page.
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { ApiError, type Api } from '../lib/api'
import { useAction } from '../lib/hooks'
import { useApp } from '../lib/session'
import { Button, Card, ErrorNote, Field, Input, LanguageSelect, PageHead, focusOnMount, toast } from '../components/ui'

const MIN_PASSWORD = 10

const AuthFrame = ({ title, sub, children }: { title: string; sub?: string; children: preact.ComponentChildren }) => (
  <main class="auth">
    <div class="auth-box">
      <p class="brand">İzlek</p>
      <h1>{title}</h1>
      {sub && <p class="muted">{sub}</p>}
      {children}
      <div class="auth-lang">
        <label for="auth-lang">{S.common.language}</label>
        <LanguageSelect id="auth-lang" />
      </div>
    </div>
  </main>
)

export const Setup = ({ api, onDone }: { api: Api; onDone: () => void }) => {
  const [org, setOrg] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const submit = useAction(async () => {
    await api.post('/api/setup', { email: email.trim(), password, org_name: org.trim() })
    onDone()
  })
  return (
    <AuthFrame title={S.auth.setupTitle} sub={S.auth.setupSub}>
      <form
        class="stack"
        onSubmit={(e) => {
          e.preventDefault()
          void submit.run()
        }}
      >
        <Field label={S.auth.orgName}>{(id) => <Input id={id} name="org_name" value={org} onChange={setOrg} required autocomplete="organization" />}</Field>
        <Field label={S.auth.email}>{(id) => <Input id={id} name="email" type="email" value={email} onChange={setEmail} required autocomplete="username" />}</Field>
        <Field label={S.auth.password} hint={S.auth.passwordHint(MIN_PASSWORD)}>
          {(id, d) => <Input id={id} name="password" type="password" value={password} onChange={setPassword} required minLength={MIN_PASSWORD} autocomplete="new-password" aria-describedby={d} />}
        </Field>
        <ErrorNote error={submit.error} />
        <Button kind="primary" type="submit" busy={submit.busy}>
          {S.auth.setupSubmit}
        </Button>
      </form>
    </AuthFrame>
  )
}

export const Login = ({ api, onDone }: { api: Api; onDone: () => void }) => {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [needTotp, setNeedTotp] = useState(false)
  const submit = useAction(async () => {
    try {
      await api.post('/api/auth/login', { email: email.trim(), password, ...(needTotp ? { totp: totp.trim() } : {}) }, { credentialCheck: true })
    } catch (e) {
      if (e instanceof ApiError && e.code == 'totp_required' && !needTotp) {
        setNeedTotp(true)
        return
      }
      throw e
    }
    onDone()
  })
  return (
    <AuthFrame title={S.auth.loginTitle}>
      <form
        class="stack"
        onSubmit={(e) => {
          e.preventDefault()
          void submit.run()
        }}
      >
        <Field label={S.auth.email}>{(id) => <Input id={id} name="email" type="email" value={email} onChange={setEmail} required autocomplete="username" readOnly={needTotp} />}</Field>
        <Field label={S.auth.password}>
          {(id) => <Input id={id} name="password" type="password" value={password} onChange={setPassword} required autocomplete="current-password" readOnly={needTotp} />}
        </Field>
        {needTotp && (
          <Field label={S.auth.totpCode} hint={S.auth.totpHint}>
            {(id, d) => (
              <Input id={id} name="totp" value={totp} onChange={setTotp} required inputMode="numeric" pattern="[0-9]{6}" maxLength={6} autocomplete="one-time-code" aria-describedby={d} inputRef={focusOnMount} />
            )}
          </Field>
        )}
        <ErrorNote error={submit.error} />
        <Button kind="primary" type="submit" busy={submit.busy}>
          {needTotp ? S.auth.verify : S.auth.loginSubmit}
        </Button>
        {needTotp && (
          <Button
            kind="ghost"
            onClick={() => {
              setNeedTotp(false)
              setTotp('')
              submit.clear()
            }}
          >
            {S.auth.back}
          </Button>
        )}
      </form>
    </AuthFrame>
  )
}

const PasswordCard = () => {
  const { api } = useApp()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const mismatch = again != '' && next != again
  const submit = useAction(async () => {
    await api.post('/api/me/password', { current, new: next }, { credentialCheck: true })
    setCurrent('')
    setNext('')
    setAgain('')
    toast(S.account.passwordChanged)
  })
  return (
    <Card title={S.account.passwordTitle}>
      <form
        class="stack form-narrow"
        onSubmit={(e) => {
          e.preventDefault()
          if (!mismatch) void submit.run()
        }}
      >
        <Field label={S.account.currentPassword}>{(id) => <Input id={id} type="password" value={current} onChange={setCurrent} required autocomplete="current-password" />}</Field>
        <Field label={S.account.newPassword} hint={S.auth.passwordHint(MIN_PASSWORD)}>
          {(id, d) => <Input id={id} type="password" value={next} onChange={setNext} required minLength={MIN_PASSWORD} autocomplete="new-password" aria-describedby={d} />}
        </Field>
        <Field label={S.account.repeatPassword} error={mismatch ? S.account.mismatch : null}>
          {(id, d) => <Input id={id} type="password" value={again} onChange={setAgain} required autocomplete="new-password" aria-describedby={d} aria-invalid={mismatch} />}
        </Field>
        <ErrorNote error={submit.error} />
        <div>
          <Button kind="primary" type="submit" busy={submit.busy}>
            {S.account.changePassword}
          </Button>
        </div>
      </form>
    </Card>
  )
}

const TotpCard = () => {
  const { api, me, refresh } = useApp()
  const [pending, setPending] = useState<{ secret: string; otpauth_url: string } | null>(null)
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const begin = useAction(async () => setPending(await api.post('/api/me/totp/begin')))
  const enable = useAction(async () => {
    await api.post('/api/me/totp/enable', { code: code.trim() }, { credentialCheck: true })
    setPending(null)
    setCode('')
    await refresh()
    toast(S.account.totpEnabled)
  })
  const disable = useAction(async () => {
    await api.post('/api/me/totp/disable', { password }, { credentialCheck: true })
    setPassword('')
    await refresh()
    toast(S.account.totpDisabled)
  })
  return (
    <Card title={S.account.totpTitle}>
      <div class="stack form-narrow">
        <p>
          {S.account.totpState}: <strong>{me.user.totp_enabled ? S.common.on : S.common.off}</strong>
        </p>
        {me.user.totp_enabled ? (
          <form
            class="stack"
            onSubmit={(e) => {
              e.preventDefault()
              void disable.run()
            }}
          >
            <Field label={S.auth.password} hint={S.account.totpDisableHint}>
              {(id, d) => <Input id={id} type="password" value={password} onChange={setPassword} required autocomplete="current-password" aria-describedby={d} />}
            </Field>
            <ErrorNote error={disable.error} />
            <div>
              <Button kind="danger" type="submit" busy={disable.busy}>
                {S.account.totpDisable}
              </Button>
            </div>
          </form>
        ) : pending ? (
          <form
            class="stack"
            onSubmit={(e) => {
              e.preventDefault()
              void enable.run()
            }}
          >
            <p>{S.account.totpInstructions}</p>
            <Field label={S.account.totpSecret}>{(id) => <input id={id} class="input mono" readOnly value={pending.secret} onFocus={(e) => e.currentTarget.select()} />}</Field>
            <Field label={S.account.totpUrl}>{(id) => <input id={id} class="input mono" readOnly value={pending.otpauth_url} onFocus={(e) => e.currentTarget.select()} />}</Field>
            <Field label={S.auth.totpCode} hint={S.auth.totpHint}>
              {(id, d) => <Input id={id} name="code" value={code} onChange={setCode} required inputMode="numeric" pattern="[0-9]{6}" maxLength={6} autocomplete="one-time-code" aria-describedby={d} />}
            </Field>
            <ErrorNote error={enable.error} />
            <div class="row">
              <Button kind="primary" type="submit" busy={enable.busy}>
                {S.account.totpEnable}
              </Button>
              <Button onClick={() => setPending(null)}>{S.common.cancel}</Button>
            </div>
          </form>
        ) : (
          <>
            <p class="muted">{S.account.totpIntro}</p>
            <ErrorNote error={begin.error} />
            <div>
              <Button busy={begin.busy} onClick={() => void begin.run()}>
                {S.account.totpBegin}
              </Button>
            </div>
          </>
        )}
      </div>
    </Card>
  )
}

export const Account = () => {
  const { me } = useApp()
  return (
    <>
      <PageHead title={S.account.title} sub={me.user.email} />
      <div class="stack">
        <Card title={S.account.orgsTitle}>
          <table class="table">
            <thead>
              <tr>
                <th scope="col">{S.account.org}</th>
                <th scope="col">{S.members.role}</th>
              </tr>
            </thead>
            <tbody>
              {me.orgs.map((o) => (
                <tr key={o.id}>
                  <td>{o.name}</td>
                  <td>{S.roles[o.role] ?? o.role}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
        <Card title={S.account.prefsTitle}>
          <div class="form-narrow">
            <Field label={S.common.language} hint={S.account.languageHint}>
              {(id, d) => <LanguageSelect id={id} describedBy={d} />}
            </Field>
          </div>
        </Card>
        <PasswordCard />
        <TotpCard />
      </div>
    </>
  )
}
