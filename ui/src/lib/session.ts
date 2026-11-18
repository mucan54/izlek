// App-wide and project-wide context.
import { createContext } from 'preact'
import { useContext } from 'preact/hooks'
import type { Api } from './api'
import type { Me, OrgRef, Project, Role, Schema } from './types'

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
