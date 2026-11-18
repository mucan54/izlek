// KVKK/GDPR deletion requests: create and track.
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { fmtDateTime } from '../lib/format'
import { useAction, useAsync, useInterval } from '../lib/hooks'
import { can } from '../lib/roles'
import { useLocation } from '../lib/router'
import { useApp, useProject } from '../lib/session'
import type { Deletion } from '../lib/types'
import { AsyncView, Badge, Button, Card, Empty, ErrorNote, Field, Modal, PageHead } from '../components/ui'

const ids = (text: string): string[] => [...new Set(text.split(/[\n,]/).map((s) => s.trim()).filter(Boolean))]

export const Deletions = () => {
  const { base, role } = useProject()
  const app = useApp()
  const { query } = useLocation()
  const list = useAsync(async (signal) => asList<Deletion>(await app.api.get(`${base}/deletions`, { signal })), [base], !can(role, 'admin'))
  const [users, setUsers] = useState(query.get('user_id') ?? '')
  const [anons, setAnons] = useState(query.get('anon_id') ?? '')
  const [note, setNote] = useState('')
  const [confirm, setConfirm] = useState(false)
  const userIds = ids(users)
  const anonIds = ids(anons)
  const create = useAction(async () => {
    await app.api.post(`${base}/deletions`, { user_ids: userIds, anon_ids: anonIds, note: note.trim() })
    setUsers('')
    setAnons('')
    setNote('')
    setConfirm(false)
    list.reload()
  })
  // pending requests complete in the background; keep the status current
  useInterval(list.reload, 15_000, (list.data ?? []).some((d) => d.status == 'pending'))

  if (!can(role, 'admin')) return <Empty title={S.deletions.forbidden} />
  return (
    <>
      <PageHead title={S.deletions.title} sub={S.deletions.sub} />
      <Card title={S.deletions.create}>
        <form
          class="stack"
          onSubmit={(e) => {
            e.preventDefault()
            if (userIds.length || anonIds.length) setConfirm(true)
          }}
        >
          <div class="form-grid form-grid-2">
            <Field label={S.deletions.userIds} hint={S.deletions.idsHint}>
              {(id, d) => <textarea id={id} name="user_ids" class="input mono" rows={4} spellcheck={false} value={users} aria-describedby={d} onInput={(e) => setUsers(e.currentTarget.value)} />}
            </Field>
            <Field label={S.deletions.anonIds} hint={S.deletions.anonHint}>
              {(id, d) => <textarea id={id} name="anon_ids" class="input mono" rows={4} spellcheck={false} value={anons} aria-describedby={d} onInput={(e) => setAnons(e.currentTarget.value)} />}
            </Field>
          </div>
          <Field label={S.deletions.note} hint={S.deletions.noteHint}>
            {(id, d) => <input id={id} name="note" class="input" value={note} maxLength={1000} aria-describedby={d} onInput={(e) => setNote(e.currentTarget.value)} />}
          </Field>
          <div>
            <Button kind="danger" type="submit" disabled={!userIds.length && !anonIds.length}>
              {S.deletions.submit}
            </Button>
          </div>
        </form>
      </Card>
      <Card title={S.deletions.list}>
        <AsyncView state={list} empty={<Empty title={S.deletions.empty} />}>
          {(items) => (
            <div class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.deletions.id}</th>
                    <th scope="col">{S.deletions.status}</th>
                    <th scope="col">{S.deletions.who}</th>
                    <th scope="col">{S.deletions.note}</th>
                    <th scope="col">{S.common.created}</th>
                  </tr>
                </thead>
                <tbody>
                  {[...items]
                    .sort((a, b) => b.created_at - a.created_at)
                    .map((d) => (
                      <tr key={d.id}>
                        <td>
                          <code>{d.id}</code>
                        </td>
                        <td>
                          <Badge kind={d.status == 'done' ? 'ok' : 'warn'}>{d.status == 'done' ? '✓ ' + S.deletions.done : '… ' + S.deletions.pending}</Badge>
                        </td>
                        <td class="ids-cell">
                          {d.user_ids.length > 0 && (
                            <div>
                              <span class="muted">{S.deletions.userIds}:</span> <span class="mono">{d.user_ids.join(', ')}</span>
                            </div>
                          )}
                          {d.anon_ids.length > 0 && (
                            <div>
                              <span class="muted">{S.deletions.anonIds}:</span> <span class="mono">{d.anon_ids.join(', ')}</span>
                            </div>
                          )}
                        </td>
                        <td>{d.note || <span class="muted">—</span>}</td>
                        <td class="nowrap">{fmtDateTime(d.created_at)}</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          )}
        </AsyncView>
      </Card>
      {confirm && (
        <Modal
          title={S.deletions.confirmTitle}
          onClose={() => setConfirm(false)}
          footer={
            <>
              <Button onClick={() => setConfirm(false)}>{S.common.cancel}</Button>
              <Button kind="danger" busy={create.busy} onClick={() => void create.run()}>
                {S.deletions.confirm}
              </Button>
            </>
          }
        >
          <div class="stack">
            <p>{S.deletions.confirmMessage(userIds.length, anonIds.length)}</p>
            <p class="muted">{S.deletions.confirmDetail}</p>
            <ErrorNote error={create.error} />
          </div>
        </Modal>
      )}
    </>
  )
}
