// All projects of the user's orgs, and project creation.
import { useState } from 'preact/hooks'
import { S } from '../i18n'
import { fmtDate } from '../lib/format'
import { useAction } from '../lib/hooks'
import { can } from '../lib/roles'
import { Link, navigate } from '../lib/router'
import { useApp } from '../lib/session'
import type { Project } from '../lib/types'
import { Button, Card, Empty, ErrorNote, Field, Input, Modal, PageHead, Select } from '../components/ui'

export const Projects = () => {
  const app = useApp()
  const adminOrgs = app.me.orgs.filter((o) => can(o.role, 'admin'))
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [org, setOrg] = useState(String(adminOrgs[0]?.id ?? ''))
  const create = useAction(async () => {
    const orgRef = adminOrgs.find((o) => String(o.id) == org)
    const project = await app.api.post<Project>('/api/projects', { org_id: orgRef?.id, name: name.trim() })
    await app.refresh()
    setOpen(false)
    setName('')
    navigate(`/p/${project.id}/settings/sdk`)
  })
  return (
    <>
      <PageHead title={S.projects.title} sub={S.projects.sub}>
        {adminOrgs.length > 0 && (
          <Button kind="primary" onClick={() => setOpen(true)}>
            {S.projects.create}
          </Button>
        )}
      </PageHead>
      {app.projects.length == 0 ? (
        <Empty title={S.projects.empty}>{adminOrgs.length ? S.projects.emptyHint : S.projects.emptyViewer}</Empty>
      ) : (
        <div class="stack">
          {app.me.orgs.map((o) => {
            const list = app.projects.filter((p) => String(p.org_id) == String(o.id))
            return (
              <Card title={o.name} key={o.id} actions={<span class="muted">{S.roles[o.role]}</span>}>
                {list.length == 0 ? (
                  <p class="muted">{S.projects.orgEmpty}</p>
                ) : (
                  <table class="table">
                    <thead>
                      <tr>
                        <th scope="col">{S.projects.name}</th>
                        <th scope="col" class="num">
                          {S.projects.id}
                        </th>
                        <th scope="col" class="num">
                          {S.settings.retention}
                        </th>
                        <th scope="col">{S.common.created}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {list.map((p) => (
                        <tr key={p.id}>
                          <td>
                            <Link href={`/p/${p.id}/dashboards`}>{p.name}</Link>
                          </td>
                          <td class="num">{p.id}</td>
                          <td class="num">{S.common.days(p.retention_days)}</td>
                          <td>{fmtDate(p.created_at)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </Card>
            )
          })}
        </div>
      )}
      {open && (
        <Modal
          title={S.projects.create}
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button onClick={() => setOpen(false)}>{S.common.cancel}</Button>
              <Button kind="primary" type="submit" busy={create.busy} form="project-create">
                {S.common.create}
              </Button>
            </>
          }
        >
          <form
            id="project-create"
            class="stack"
            onSubmit={(e) => {
              e.preventDefault()
              void create.run()
            }}
          >
            {adminOrgs.length > 1 && (
              <Field label={S.account.org}>{(id) => <Select id={id} value={org} onChange={setOrg} options={adminOrgs.map((o) => [String(o.id), o.name] as const)} />}</Field>
            )}
            <Field label={S.projects.name} hint={S.projects.nameHint}>
              {(id, d) => <Input id={id} name="name" value={name} onChange={setName} required maxLength={100} aria-describedby={d} />}
            </Field>
            <ErrorNote error={create.error} />
          </form>
        </Modal>
      )}
    </>
  )
}
