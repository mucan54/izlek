// Org-level screens: members and the audit log.
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { fmtDate, fmtDateTime } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { ROLES, can } from '../lib/roles'
import { useApp } from '../lib/session'
import type { AuditEntry, Member, OrgRef, Role } from '../lib/types'
import { AsyncView, Button, Card, ConfirmButton, Empty, ErrorNote, Field, Input, Modal, PageHead, Select, toast } from '../components/ui'

const roleOptions = () => ROLES.map((r) => [r, S.roles[r]] as const)

export const Members = ({ org }: { org: OrgRef }) => {
  const app = useApp()
  const base = `/api/orgs/${encodeURIComponent(String(org.id))}/members`
  const list = useAsync(async (signal) => asList<Member>(await app.api.get(base, { signal })), [base])
  const owner = can(org.role, 'owner')
  const [open, setOpen] = useState(false)
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('member')
  const [password, setPassword] = useState('')
  const add = useAction(async () => {
    await app.api.post(base, { email: email.trim(), role, password })
    setOpen(false)
    setEmail('')
    setPassword('')
    list.reload()
    toast(S.members.added)
  })
  const setMemberRole = async (m: Member, next: Role) => {
    try {
      await app.api.patch(`${base}/${encodeURIComponent(String(m.user_id))}`, { role: next })
      if (String(m.user_id) == String(app.me.user.id)) await app.refresh()
    } catch (e) {
      toast((e as Error).message, 'error')
    }
    list.reload()
  }
  return (
    <>
      <PageHead title={S.members.title} sub={org.name}>
        {owner && (
          <Button kind="primary" onClick={() => setOpen(true)}>
            {S.members.add}
          </Button>
        )}
      </PageHead>
      {!owner && <p class="muted">{S.members.ownerOnly}</p>}
      <AsyncView state={list} empty={<Empty title={S.members.empty} />}>
        {(members) => (
          <Card>
            <div class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.auth.email}</th>
                    <th scope="col">{S.members.role}</th>
                    <th scope="col">{S.members.since}</th>
                    <th scope="col">
                      <span class="sr-only">{S.common.actions}</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {members.map((m) => (
                    <tr key={m.user_id}>
                      <td>
                        {m.email}
                        {String(m.user_id) == String(app.me.user.id) && <span class="muted"> ({S.members.you})</span>}
                      </td>
                      <td>{owner ? <Select aria-label={S.members.roleOf(m.email)} value={m.role} onChange={(r) => void setMemberRole(m, r)} options={roleOptions()} /> : S.roles[m.role]}</td>
                      <td class="nowrap">{fmtDate(m.created_at)}</td>
                      <td class="cell-action">
                        {owner && String(m.user_id) != String(app.me.user.id) && (
                          <ConfirmButton
                            small
                            label={S.members.remove}
                            title={S.members.removeTitle}
                            message={S.members.removeMessage(m.email)}
                            onConfirm={async () => {
                              await app.api.del(`${base}/${encodeURIComponent(String(m.user_id))}`)
                              list.reload()
                            }}
                          />
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        )}
      </AsyncView>
      <Card title={S.members.rolesTitle}>
        <dl class="kv kv-wide">
          {ROLES.map((r) => (
            <>
              <dt>{S.roles[r]}</dt>
              <dd>{S.members.roleHelp[r]}</dd>
            </>
          ))}
        </dl>
      </Card>
      {open && (
        <Modal
          title={S.members.add}
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button onClick={() => setOpen(false)}>{S.common.cancel}</Button>
              <Button kind="primary" type="submit" form="member-form" busy={add.busy}>
                {S.common.add}
              </Button>
            </>
          }
        >
          <form
            id="member-form"
            class="stack"
            onSubmit={(e) => {
              e.preventDefault()
              void add.run()
            }}
          >
            <Field label={S.auth.email}>{(id) => <Input id={id} name="email" type="email" value={email} onChange={setEmail} required autocomplete="off" />}</Field>
            <Field label={S.members.role}>{(id) => <Select id={id} value={role} onChange={setRole} options={roleOptions()} />}</Field>
            <Field label={S.members.initialPassword} hint={S.members.initialPasswordHint}>
              {(id, d) => <Input id={id} name="password" type="text" class="mono" value={password} onChange={setPassword} required minLength={10} autocomplete="off" aria-describedby={d} />}
            </Field>
            <ErrorNote error={add.error} />
          </form>
        </Modal>
      )}
    </>
  )
}

const PAGE = 50

export const Audit = ({ org }: { org: OrgRef }) => {
  const app = useApp()
  const base = `/api/orgs/${encodeURIComponent(String(org.id))}/audit`
  const [extra, setExtra] = useState<AuditEntry[]>([])
  const [end, setEnd] = useState(false)
  const first = useAsync(
    async (signal) => {
      const rows = asList<AuditEntry>(await app.api.get(`${base}?limit=${PAGE}`, { signal }))
      setExtra([])
      setEnd(rows.length < PAGE)
      return rows
    },
    [base],
  )
  const more = useAction(async () => {
    const all = [...(first.data ?? []), ...extra]
    const last = all[all.length - 1]
    if (!last) return
    const rows = asList<AuditEntry>(await app.api.get(`${base}?limit=${PAGE}&before=${encodeURIComponent(String(last.id))}`))
    setExtra([...extra, ...rows])
    setEnd(rows.length < PAGE)
  })
  return (
    <>
      <PageHead title={S.audit.title} sub={org.name}>
        <Button onClick={first.reload}>{S.common.refresh}</Button>
      </PageHead>
      <AsyncView state={first} empty={<Empty title={S.audit.empty} />}>
        {(rows) => (
          <Card>
            <div class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.common.date}</th>
                    <th scope="col">{S.audit.actor}</th>
                    <th scope="col">{S.audit.action}</th>
                    <th scope="col">{S.audit.target}</th>
                    <th scope="col">{S.audit.detail}</th>
                  </tr>
                </thead>
                <tbody>
                  {[...rows, ...extra].map((a) => (
                    <tr key={a.id}>
                      <td class="nowrap">{fmtDateTime(a.ts)}</td>
                      <td>{String(a.actor)}</td>
                      <td>
                        <code>{a.action}</code>
                      </td>
                      <td class="mono ellipsis">{a.target}</td>
                      <td class="mono small detail-cell">{a.detail && typeof a.detail == 'object' && Object.keys(a.detail).length == 0 ? '' : typeof a.detail == 'string' ? a.detail : JSON.stringify(a.detail)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div class="table-foot">
              <ErrorNote error={more.error} />
              {!end && (
                <Button busy={more.busy} onClick={() => void more.run()}>
                  {S.audit.older}
                </Button>
              )}
            </div>
          </Card>
        )}
      </AsyncView>
    </>
  )
}
