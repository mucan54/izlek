// Root: session bootstrap, routing and the application shell.
import { Fragment, type ComponentChildren } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { LOCALE, S, onLocaleChange } from './i18n'
import { ApiError, asList, createApi } from './lib/api'
import { useAsync } from './lib/hooks'
import { can } from './lib/roles'
import { Link, matchPath, navigate, useLocation } from './lib/router'
import { AppCtx, ProjectCtx, useApp, type AppState, type ProjectState } from './lib/session'
import type { Me, Project, Schema } from './lib/types'
import { Button, Empty, ErrorNote, PageHead, Spinner, Toaster } from './components/ui'
import { Account, Login, Setup } from './screens/Auth'
import { Projects } from './screens/Projects'
import { Events } from './screens/Events'
import { InsightEditor, InsightList } from './screens/Insights'
import { DashboardList, DashboardPage } from './screens/Dashboards'
import { PersonPage, Persons } from './screens/Persons'
import { ReplayPage } from './screens/Replay'
import { FlagEditor, FlagList } from './screens/Flags'
import { Settings } from './screens/Settings'
import { Deletions } from './screens/Deletions'
import { Audit, Members } from './screens/Members'

type Boot = { state: 'loading' } | { state: 'setup' } | { state: 'login' } | { state: 'error'; error: Error } | { state: 'ready'; me: Me; projects: Project[] }

export const App = () => {
  const [boot, setBoot] = useState<Boot>({ state: 'loading' })
  const bootRef = useRef(boot)
  bootRef.current = boot
  // a language change remounts every screen (see the keyed fragment below)
  const [locale, setLocaleShown] = useState(LOCALE)
  useEffect(() => onLocaleChange(() => setLocaleShown(LOCALE)), [])

  const api = useMemo(
    () =>
      createApi({
        fallbackMessage: (status) => (status == 0 ? S.errors.network : (S.errors.byStatus[status] ?? S.errors.http(status))),
        // the session ended under us: keep the URL, show the login form
        onUnauthorized: () => {
          if (bootRef.current.state == 'ready') setBoot({ state: 'login' })
        },
      }),
    [],
  )

  const load = useCallback(async () => {
    try {
      const me = await api.get<Me>('/api/me', { credentialCheck: true })
      api.setCsrf(me.csrf)
      const projects = asList<Project>(await api.get('/api/projects'))
      setBoot({ state: 'ready', me, projects })
    } catch (e) {
      if (e instanceof ApiError && e.status == 401) {
        api.setCsrf(null)
        try {
          const setup = await api.get<{ needs_setup: boolean }>('/api/setup')
          setBoot({ state: setup.needs_setup ? 'setup' : 'login' })
        } catch (e2) {
          setBoot({ state: 'error', error: e2 as Error })
        }
      } else setBoot({ state: 'error', error: e as Error })
    }
  }, [api])

  useEffect(() => void load(), [load])

  const logout = useCallback(async () => {
    try {
      await api.post('/api/auth/logout')
    } catch {
      // the cookie may already be gone; either way we end up at the login form
    }
    api.setCsrf(null)
    setBoot({ state: 'login' })
    navigate('/')
  }, [api])

  let body: ComponentChildren
  if (boot.state == 'loading') body = <Spinner />
  else if (boot.state == 'error')
    body = (
      <main class="auth">
        <div class="auth-box">
          <p class="brand">İzlek</p>
          <ErrorNote error={boot.error} onRetry={() => void load()} />
        </div>
      </main>
    )
  else if (boot.state == 'setup') body = <Setup api={api} onDone={() => void load()} />
  else if (boot.state == 'login') body = <Login api={api} onDone={() => void load()} />
  else {
    const state: AppState = { api, me: boot.me, projects: boot.projects, refresh: load, logout }
    body = (
      <AppCtx.Provider value={state}>
        <Shell />
      </AppCtx.Provider>
    )
  }
  return (
    <>
      {/* Keyed by language: screens are rebuilt from scratch in the new language, so nothing
          they captured (memoised labels, messages kept in state) stays in the old one.
          The session and the API client live above the key and survive the switch. */}
      <Fragment key={locale}>{body}</Fragment>
      <Toaster />
    </>
  )
}

/** Page-level "nothing here" with a heading and a way out. */
const Missing = ({ title, href, label }: { title: string; href: string; label: string }) => (
  <>
    <PageHead title={title} />
    <Empty title={title}>
      <Link href={href}>{label}</Link>
    </Empty>
  </>
)

const NavLink = ({ href, path, also, children }: { href: string; path: string; also?: string; children: ComponentChildren }) => {
  const active = path == href || path.startsWith(href + '/') || (!!also && path.startsWith(also + '/'))
  return (
    <Link href={href} class={`nav-link${active ? ' nav-active' : ''}`} aria-current={active ? 'page' : undefined}>
      {children}
    </Link>
  )
}

const Shell = () => {
  const app = useApp()
  const { path } = useLocation()
  const inProject = matchPath('/p/:pid/*', path)
  const inOrg = matchPath('/org/:oid/*', path)
  const project = inProject ? app.projects.find((p) => String(p.id) == inProject.pid) : undefined

  // remember the last project so org-level pages keep the project menu
  const lastPid = useRef<number | undefined>(undefined)
  if (project) lastPid.current = project.id
  const navProject = project ?? app.projects.find((p) => p.id == lastPid.current) ?? app.projects[0]

  const orgId = inOrg?.oid ?? (navProject ? String(navProject.org_id) : app.me.orgs[0] ? String(app.me.orgs[0].id) : undefined)
  const org = app.me.orgs.find((o) => String(o.id) == orgId)
  const role = org?.role
  const projectRole = navProject ? app.me.orgs.find((o) => String(o.id) == String(navProject.org_id))?.role : undefined

  useEffect(() => {
    if (path == '/' || path == '') navigate(app.projects.length ? `/p/${app.projects[0].id}/dashboards` : '/projects', true)
  }, [path, app.projects])

  const p = (sub: string) => `/p/${navProject?.id}/${sub}`
  let page: ComponentChildren
  if (inProject) {
    page = project ? <ProjectRoutes project={project} path={path} key={project.id} /> : <Missing title={S.shell.projectNotFound} href="/projects" label={S.nav.projects} />
  } else if (path == '/projects') page = <Projects />
  else if (path == '/account') page = <Account />
  else if (inOrg && org && matchPath('/org/:oid/members', path)) page = <Members org={org} key={org.id} />
  else if (inOrg && org && matchPath('/org/:oid/audit', path)) page = <Audit org={org} key={org.id} />
  else if (path == '/' || path == '') page = <Spinner />
  else page = <Missing title={S.shell.notFound} href="/" label={S.shell.home} />

  return (
    <div class="shell">
      <a class="skip" href="#main">
        {S.shell.skip}
      </a>
      <aside class="side">
        <div class="side-top">
          <Link href="/" class="brand">
            İzlek
          </Link>
          <label class="sr-only" for="project-switch">
            {S.shell.project}
          </label>
          <select
            id="project-switch"
            class="input select project-switch"
            value={inProject && project ? String(project.id) : navProject && !inOrg && path != '/projects' && path != '/account' ? String(navProject.id) : ''}
            onChange={(e) => {
              const v = e.currentTarget.value
              if (!v) return navigate('/projects')
              // stay on the same section when switching projects
              const section = inProject ? path.split('/')[3] : 'dashboards'
              navigate(`/p/${v}/${section || 'dashboards'}`)
            }}
          >
            <option value="">{app.projects.length ? S.shell.allProjects : S.shell.noProjects}</option>
            {app.me.orgs.map((o) => {
              const list = app.projects.filter((x) => String(x.org_id) == String(o.id))
              return list.length ? (
                <optgroup label={o.name} key={o.id}>
                  {list.map((x) => (
                    <option value={String(x.id)} key={x.id}>
                      {x.name}
                    </option>
                  ))}
                </optgroup>
              ) : null
            })}
          </select>
        </div>
        <nav class="nav" aria-label={S.shell.navLabel}>
          {navProject && (
            <>
              <p class="nav-title">{S.nav.analyze}</p>
              <NavLink href={p('dashboards')} path={path}>
                {S.nav.dashboards}
              </NavLink>
              <NavLink href={p('insights')} path={path}>
                {S.nav.insights}
              </NavLink>
              <NavLink href={p('events')} path={path}>
                {S.nav.events}
              </NavLink>
              <NavLink href={p('persons')} path={path} also={p('replay')}>
                {S.nav.persons}
              </NavLink>
              <p class="nav-title">{S.nav.manage}</p>
              <NavLink href={p('flags')} path={path}>
                {S.nav.flags}
              </NavLink>
              {can(projectRole, 'admin') && (
                <NavLink href={p('deletions')} path={path}>
                  {S.nav.deletions}
                </NavLink>
              )}
              <NavLink href={p('settings')} path={path}>
                {S.nav.settings}
              </NavLink>
            </>
          )}
          <p class="nav-title">{S.nav.org}</p>
          <NavLink href="/projects" path={path}>
            {S.nav.projects}
          </NavLink>
          {org && (
            <NavLink href={`/org/${org.id}/members`} path={path}>
              {S.nav.members}
            </NavLink>
          )}
          {org && can(role, 'admin') && (
            <NavLink href={`/org/${org.id}/audit`} path={path}>
              {S.nav.audit}
            </NavLink>
          )}
        </nav>
        <div class="side-bottom">
          <Link href="/account" class={`nav-link user${path == '/account' ? ' nav-active' : ''}`} title={S.nav.account}>
            <span class="user-email">{app.me.user.email}</span>
            {role && <span class="user-role">{S.roles[role]}</span>}
          </Link>
          <Button small kind="ghost" onClick={() => void app.logout()}>
            {S.nav.logout}
          </Button>
        </div>
      </aside>
      <main class="main" id="main" tabIndex={-1}>
        {page}
      </main>
    </div>
  )
}

const ProjectRoutes = ({ project, path }: { project: Project; path: string }) => {
  const app = useApp()
  const base = `/api/projects/${project.id}`
  const org = app.me.orgs.find((o) => String(o.id) == String(project.org_id))
  const schema = useAsync<Schema>((signal) => app.api.post(`${base}/q/schema`, {}, { signal }), [project.id])

  const state: ProjectState = useMemo(
    () => ({
      project,
      org,
      role: org?.role ?? 'viewer',
      base,
      to: (sub: string) => `/p/${project.id}/${sub}`,
      query: <T,>(endpoint: string, body: unknown, signal?: AbortSignal) => app.api.post<T>(`${base}/q/${endpoint}`, body, { signal }),
      schema: schema.data ?? null,
    }),
    [project, org, base, app.api, schema.data],
  )

  const sub = '/' + path.split('/').slice(3).join('/')
  const m = (pattern: string) => matchPath(pattern, sub)
  let page: ComponentChildren
  let params: Record<string, string> | null
  if (sub == '/' || sub == '') {
    page = <Spinner />
    navigate(state.to('dashboards'), true)
  } else if (m('/events')) page = <Events />
  else if (m('/insights')) page = <InsightList />
  else if ((params = m('/insights/new/:kind'))) page = <InsightEditor kind={params.kind} key={'new-' + params.kind} />
  else if ((params = m('/insights/:id'))) page = <InsightEditor id={params.id} key={params.id} />
  else if (m('/dashboards')) page = <DashboardList />
  else if ((params = m('/dashboards/:id'))) page = <DashboardPage id={params.id} key={params.id} />
  else if (m('/persons')) page = <Persons />
  else if ((params = m('/persons/:key'))) page = <PersonPage personKey={params.key} key={params.key} />
  else if ((params = m('/replay/:session'))) page = <ReplayPage session={params.session} key={params.session} />
  else if (m('/flags')) page = <FlagList />
  else if (m('/flags/~new')) page = <FlagEditor key="~new" />
  else if ((params = m('/flags/:key'))) page = <FlagEditor flagKey={params.key} key={params.key} />
  else if (m('/settings')) page = <Settings tab="general" />
  else if ((params = m('/settings/:tab'))) page = <Settings tab={params.tab} />
  else if (m('/deletions')) page = <Deletions />
  else page = <Missing title={S.shell.notFound} href={state.to('dashboards')} label={S.nav.dashboards} />

  return <ProjectCtx.Provider value={state}>{page}</ProjectCtx.Provider>
}
