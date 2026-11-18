// Role table from spec/control.md §1. The server enforces it; the UI uses it
// to hide or disable what the current role cannot do.
import type { Role } from './types'

export type Action = 'read' | 'insight.write' | 'admin' | 'owner'

const RANK: Record<Role, number> = { viewer: 0, member: 1, admin: 2, owner: 3 }
const NEED: Record<Action, Role> = { read: 'viewer', 'insight.write': 'member', admin: 'admin', owner: 'owner' }

export const can = (role: Role | undefined | null, action: Action): boolean =>
  !!role && role in RANK && RANK[role] >= RANK[NEED[action]]

export const ROLES: Role[] = ['owner', 'admin', 'member', 'viewer']
