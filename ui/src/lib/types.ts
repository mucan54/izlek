// Shapes of the control-plane API (spec/control.md) and of the query API it
// proxies (spec/query.md). Timestamps are unix ms unless noted.

export type Role = 'owner' | 'admin' | 'member' | 'viewer'

export interface User {
  id: string | number
  email: string
  totp_enabled: boolean
}
export interface OrgRef {
  id: string | number
  name: string
  role: Role
}
export interface Me {
  user: User
  orgs: OrgRef[]
  csrf: string
}

export interface Member {
  user_id: string | number
  email: string
  role: Role
  created_at: number
}
export interface AuditEntry {
  id: string | number
  ts: number
  actor: string | number
  action: string
  target: string
  detail: unknown
}

export interface Project {
  id: number
  org_id: string | number
  name: string
  retention_days: number
  replay_retention_days: number
  drop_keys: string[]
  scan_limit_bytes: number
  max_concurrent_queries: number
  replay_sample_rate: number
  created_at: number
}

export interface Token {
  token: string
  name: string
  status: 'active' | 'disabled'
  allowed_origins: string[]
  rate_limit: number
  replay: boolean
  created_at: number
}
export type KeyScope = 'query' | 'admin'
export interface Key {
  id: string | number
  name: string
  scopes: KeyScope[]
  prefix: string
  created_at: number
  last_used_at: number | null
  /** only in the response of the creating POST */
  secret?: string
}

/** An account-wide personal access token (`pat_…`): acts as the user, limited by its scopes. */
export interface AccessToken {
  id: string
  name: string
  /** `pat_…abcd` */
  prefix: string
  /** `izlek:read`, optionally `izlek:admin` */
  scopes: string[]
  created_at: number
  last_used_at: number | null
  /** null = no expiry */
  expires_at: number | null
  /** the token itself; only in the response of the creating POST */
  token?: string
}

/** An application the user approved on the OAuth consent page (docs/mcp.md, "Revoking access"). */
export interface ConnectedApp {
  id: string | number
  client_id: string
  /** chosen by the application; may be empty */
  client_name: string
  /** `dcr`: registered itself, the name is unverified; `cimd`: described by a metadata document at `client_id` */
  client_kind: string
  client_uri: string
  /** where the access is sent: hosts of the redirect URIs */
  redirect_hosts: string[]
  /** `izlek:read`, `izlek:admin` */
  scopes: string[]
  created_at: number
  last_used_at: number | null
  expires_at: number
}

// ---- expressions (query filters and flag conditions share the grammar) ----

export type Expr = { and: Expr[] } | { or: Expr[] } | { not: Expr } | { prop: string; op: string; value?: unknown }

// ---- flags (spec/flags.md) ----

export interface Variant {
  key: string
  /** weight in basis points; a rule's weights sum to 10000 */
  w: number
}
export interface Rule {
  when?: Expr
  /** 0–10000; absent = 10000 */
  rollout?: number
  variants?: Variant[]
  stable_on?: 'anon_id'
}
export type FlagDefault = null | boolean | string
export interface Flag {
  key: string
  name: string
  description: string
  enabled: boolean
  salt: string
  rules: Rule[]
  default: FlagDefault
  version: number
  updated_at: number
  updated_by: string | number
}
export interface FlagVersion {
  version: number
  ts: number
  actor: string | number
  flag: Flag
}

// ---- insights / dashboards ----

export type InsightKind = 'trends' | 'funnel' | 'retention' | 'sql'
export interface Insight<Q = StoredQuery> {
  id: string | number
  name: string
  kind: InsightKind
  query: Q
  created_by: string | number
  updated_at: number
}
export interface DashboardItem {
  insight_id: string | number
  x: number
  y: number
  w: number
  h: number
}
export interface Dashboard {
  id: string | number
  name: string
  items: DashboardItem[]
  updated_at: number
}

export interface Deletion {
  id: string
  user_ids: string[]
  anon_ids: string[]
  status: 'pending' | 'done'
  created_at: number
  created_by: string | number
  note: string
}

// ---- query API ----

/** Either a stored relative range or concrete bounds (spec/control.md §6). */
export interface TimeBounds {
  range?: string
  from?: string
  to?: string
}

export type Agg = 'count' | 'unique' | 'sum' | 'avg' | 'min' | 'max'
export type Interval = 'hour' | 'day' | 'week' | 'month'
export interface TrendSeries {
  event: string | null
  agg: Agg
  prop?: string
  label?: string
  filter?: Expr
}
export interface TrendsQuery extends TimeBounds {
  interval: Interval
  series: TrendSeries[]
  filter?: Expr
  breakdown?: string
  breakdown_limit?: number
}
export interface FunnelStep {
  event: string | null
  filter?: Expr
}
export type FunnelOrder = 'ordered' | 'strict' | 'any'
export interface FunnelQuery extends TimeBounds {
  steps: FunnelStep[]
  window_seconds: number
  order: FunnelOrder
  filter?: Expr
  breakdown?: string
}
export type Period = 'day' | 'week' | 'month'
export interface RetentionQuery extends TimeBounds {
  period: Period
  periods: number
  start: FunnelStep
  return: FunnelStep
  filter?: Expr
}
export interface SqlQuery extends TimeBounds {
  sql: string
}
export interface EventsQuery extends TimeBounds {
  filter?: Expr
  limit?: number
  order?: 'asc' | 'desc'
  cursor?: string
}
export type StoredQuery = TrendsQuery | FunnelQuery | RetentionQuery | SqlQuery

export interface Meta {
  elapsed_ms: number
  bytes_scanned: number
  files: number
  cached: boolean
  manifests?: Record<string, number>
}
export interface TrendsResult {
  buckets: string[]
  series: { index: number; label: string; breakdown?: string | null; values: (number | null)[] }[]
  meta: Meta
}
export interface FunnelStepResult {
  event: string | null
  count: number
  median_seconds: number | null
}
export interface FunnelResult {
  steps: FunnelStepResult[]
  breakdowns?: { value: string | null; steps: FunnelStepResult[] }[]
  meta: Meta
}
export interface RetentionResult {
  cohorts: { period_start: string; size: number; returned: number[] }[]
  meta: Meta
}
export interface EventRow {
  uuid: string
  ts: number
  recv_ts: number
  event: string
  person_key: string
  anon_id: string
  user_id: string | null
  session_id: string | null
  platform: string | null
  os: string | null
  os_version: string | null
  device_model: string | null
  device_type: string | null
  app_version: string | null
  locale: string | null
  country: string | null
  sdk: string | null
  url_path: string | null
  referrer_host: string | null
  screen: string | null
  flags: number
  props: Record<string, unknown>
}
export interface EventsResult {
  events: EventRow[]
  next_cursor: string | null
  meta: Meta
}
export interface SqlResult {
  columns: { name: string; type: string }[]
  rows: unknown[][]
  meta: Meta
}
export interface Schema {
  events: string[]
  props: { key: string; type: string; promoted: boolean }[]
  person_props: string[]
}
