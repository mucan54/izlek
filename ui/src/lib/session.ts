// App-wide and project-wide context.
import { createContext } from 'preact'
import { useContext, useEffect, useState } from 'preact/hooks'
import type { Api } from './api'
import { asList } from './api'
import type { Me, Member, OrgRef, Project, Role, Schema } from './types'

export interface AppState {
  api: Api
  me: Me
  projects: Project[]
  /** re-reads /api/me and /api/projects */
  refresh: () => Promise<void>
  logout: () => Promise<void>
}
export const AppCtx = createContext<AppState>(null as unknown as AppState)
export const useApp = (): AppState => useContext(AppCtx)

export interface ProjectState {
  project: Project
  org: OrgRef | undefined
  role: Role
  /** API prefix: `/api/projects/<id>` */
  base: string
  /** UI path inside the project: `to('flags')` → `/p/<id>/flags` */
  to: (path: string) => string
  /** POST to the query proxy */
  query: <T>(endpoint: string, body: unknown, signal?: AbortSignal) => Promise<T>
  /** names for autocomplete; null until loaded (or if loading failed) */
  schema: Schema | null
}
export const ProjectCtx = createContext<ProjectState>(null as unknown as ProjectState)
export const useProject = (): ProjectState => useContext(ProjectCtx)

/**
 * Display name for an audit/flag-version `actor`: the member's e-mail when the
 * member list is readable, otherwise the raw id (user id or key id).
 */
export const useActorName = (orgId: string | number | undefined): ((actor: string | number) => string) => {
  const { api } = useApp()
  const [names, setNames] = useState<Map<string, string>>(new Map())
  useEffect(() => {
    if (orgId === undefined) return
    let alive = true
    api.get(`/api/orgs/${encodeURIComponent(String(orgId))}/members`).then(
      (list) => alive && setNames(new Map(asList<Member>(list).map((m) => [String(m.user_id), m.email]))),
      () => undefined, // not allowed or unavailable: ids are shown as they are
    )
    return () => {
      alive = false
    }
  }, [api, orgId])
  return (actor) => names.get(String(actor)) ?? String(actor)
}
