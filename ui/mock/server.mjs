// Mock control plane: the API of spec/control.md kept in memory, the query
// proxy of spec/query.md answered from a deterministic demo dataset, and the
// built UI served from dist/. No dependencies. Also the dev server:
//
//   node mock/server.mjs            # empty database → first-run setup
//   MOCK_SEED=1 node mock/server.mjs  # demo org, project, flags, insights
//
//   LA_CONTROL_URL=http://127.0.0.1:8090 node mock/server.mjs
//                                   # no mock data: serve dist/ and forward /api/ to a real control plane
//
// Environment: PORT (default 8090; 8089 in proxy mode), HOST, MOCK_SEED,
// LA_UI_DIR (default ../dist), LA_CONTROL_URL.
import { createServer, request as httpRequest } from 'node:http'
import { request as httpsRequest } from 'node:https'
import { createHash, randomBytes } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { evalFlag, validate as validateCond } from './expr.mjs'
import { buildDataset, createEngine, QueryError } from './data.mjs'
import { newSecret, verifyTotp } from './totp.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))

class HttpError extends Error {
  constructor(status, code, message) {
    super(message)
    this.status = status
    this.code = code
  }
}
const badRequest = (message, code = 'bad_request') => new HttpError(400, code, message)
const notFound = () => new HttpError(404, 'not_found', 'Kaynak bulunamadı')
const forbidden = () => new HttpError(403, 'forbidden', 'Bu işlem için yetkiniz yok')

const RANK = { viewer: 0, member: 1, admin: 2, owner: 3 }
const BASE62 = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'
const base62 = (n) => Array.from(randomBytes(n), (b) => BASE62[b % 62]).join('')
const sha256 = (s) => createHash('sha256').update(s).digest('hex')
// A mock, not a password store: the real control plane uses argon2id.
const hashPassword = (password) => sha256('mock-salt:' + password)

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.map': 'application/json',
  '.svg': 'image/svg+xml',
  '.ico': 'image/x-icon',
  '.json': 'application/json',
}

const SECURITY_HEADERS = {
  'Content-Security-Policy': "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'",
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'same-origin',
}

export const createMockServer = (options = {}) => {
  const now = options.now ?? (() => Date.now())
  const uiDir = options.uiDir ?? process.env.LA_UI_DIR ?? path.join(here, '../dist')
  const dataset = buildDataset(now())
  const engine = createEngine(dataset, now)
  /** Switches tests can flip at runtime. */
  const hooks = { rawReplay: false }

  const db = {
    users: [], // { id, email, password, totp_secret, totp_pending, totp_enabled }
    orgs: [], // { id, name }
    memberships: [], // { org_id, user_id, role, created_at }
    projects: [],
    nextProjectId: 1,
    tokens: [], // Token + project_id
    keys: [], // Key + project_id + hash
    flags: [], // Flag + project_id
    flagVersions: [], // { project_id, key, version, ts, actor, flag }
    insights: [],
    dashboards: [],
    deletions: [],
    audit: [],
    sessions: new Map(), // sha256(cookie) → { user_id, csrf, expires }
    loginFailures: new Map(), // email → [ts…]
    seq: 1,
  }
  const nextId = () => db.seq++

  const audit = (actor, action, target, detail = {}) => {
    db.audit.push({ id: nextId(), ts: now(), actor, action, target: String(target), detail })
  }

  // ---- auth helpers ----

  const openSession = (res, user) => {
    const cookie = randomBytes(32).toString('hex')
    db.sessions.set(sha256(cookie), { user_id: user.id, csrf: randomBytes(16).toString('hex'), expires: now() + 30 * 86_400_000 })
    res.setHeader('Set-Cookie', `la_session=${cookie}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${30 * 86400}`)
  }
  const cookieOf = (req) => /(?:^|;\s*)la_session=([0-9a-f]+)/.exec(req.headers.cookie ?? '')?.[1]
  const sessionOf = (req) => {
    const cookie = cookieOf(req)
    if (!cookie) return null
    const session = db.sessions.get(sha256(cookie))
    if (!session || session.expires < now()) return null
    session.expires = now() + 30 * 86_400_000 // sliding
    return session
  }
  const roleIn = (userId, orgId) => db.memberships.find((m) => m.user_id == userId && m.org_id == orgId)?.role
  const requireRole = (ctx, orgId, min) => {
    const role = roleIn(ctx.user.id, orgId)
    if (!role) throw notFound() // other orgs' resources do not exist as far as the caller can tell
    if (RANK[role] < RANK[min]) throw forbidden()
    return role
  }
  const projectFor = (ctx, pid, min) => {
    const project = db.projects.find((p) => String(p.id) == pid)
    if (!project) throw notFound()
    requireRole(ctx, project.org_id, min)
    return project
  }
  const checkPassword = (password) => {
    if (typeof password != 'string' || password.length < 10) throw badRequest('Parola en az 10 karakter olmalıdır', 'weak_password')
  }
  const checkEmail = (email) => {
    if (typeof email != 'string' || !/^[^@\s]+@[^@\s]+$/.test(email)) throw badRequest('Geçerli bir e-posta adresi girin', 'invalid_email')
    return email.trim().toLowerCase()
  }
  const text = (v, field, max = 200, required = true) => {
    if (v === undefined || v === null || v === '') {
      if (required) throw badRequest(`"${field}" zorunludur`)
      return ''
    }
    if (typeof v != 'string' || v.length > max) throw badRequest(`"${field}" en çok ${max} karakterlik bir metin olmalıdır`)
    return v.trim()
  }
  const strings = (v, field) => {
    if (v === undefined || v === null) return []
    if (!Array.isArray(v) || v.some((x) => typeof x != 'string')) throw badRequest(`"${field}" bir metin dizisi olmalıdır`)
    return v.map((x) => x.trim()).filter(Boolean)
  }
  const int = (v, field, min, max) => {
    if (typeof v != 'number' || !Number.isInteger(v) || v < min || v > max) throw badRequest(`"${field}" ${min} ile ${max} arasında bir tam sayı olmalıdır`)
    return v
  }

  // ---- entity helpers ----

  const projectView = ({ closed: _closed, ...p }) => p
  const tokenView = ({ project_id: _p, ...t }) => t
  const keyView = ({ project_id: _p, hash: _h, ...k }) => k
  const flagView = ({ project_id: _p, ...f }) => f

  const createToken = (project, body, actor) => {
    const token = {
      project_id: project.id,
      token: 'pk_' + base62(32),
      name: text(body.name, 'name', 100),
      status: 'active',
      allowed_origins: strings(body.allowed_origins, 'allowed_origins'),
      rate_limit: body.rate_limit === undefined ? 0 : int(body.rate_limit, 'rate_limit', 0, 1_000_000),
      replay: !!body.replay,
      created_at: now(),
    }
    db.tokens.push(token)
    audit(actor, 'token.create', token.token, { project_id: project.id, name: token.name })
    return token
  }

  const createProject = (orgId, name, actor) => {
    const project = {
      id: db.nextProjectId++,
      org_id: orgId,
      name,
      retention_days: 365,
      replay_retention_days: 30,
      drop_keys: [],
      scan_limit_bytes: 0,
      max_concurrent_queries: 4,
      replay_sample_rate: 0.1,
      created_at: now(),
    }
    db.projects.push(project)
    audit(actor, 'project.create', project.id, { name })
    createToken(project, { name: 'Varsayılan' }, actor)
    return project
  }

  const validateFlag = (body) => {
    const rules = body.rules ?? []
    if (!Array.isArray(rules)) throw badRequest('"rules" bir dizi olmalıdır', 'invalid_flag')
    rules.forEach((r, i) => {
      if (!r || typeof r != 'object') throw badRequest(`rules[${i}] bir nesne olmalıdır`, 'invalid_flag')
      if (r.when !== undefined) {
        const e = validateCond(r.when)
        if (e) throw badRequest(`rules[${i}].when: ${e}`, 'invalid_flag')
      }
      if (r.rollout !== undefined && !(Number.isInteger(r.rollout) && r.rollout >= 0 && r.rollout <= 10000))
        throw badRequest(`rules[${i}].rollout 0 ile 10000 arasında olmalıdır`, 'invalid_flag')
      if (r.variants !== undefined) {
        if (!Array.isArray(r.variants) || r.variants.some((v) => !v || typeof v.key != 'string' || !v.key || !Number.isInteger(v.w) || v.w < 0))
          throw badRequest(`rules[${i}].variants geçersiz`, 'invalid_flag')
        const sum = r.variants.reduce((a, v) => a + v.w, 0)
        if (r.variants.length && sum != 10000) throw badRequest(`rules[${i}]: varyant ağırlıkları toplamı 10000 olmalıdır (şu an ${sum})`, 'invalid_flag')
      }
      if (r.stable_on !== undefined && r.stable_on != 'anon_id') throw badRequest(`rules[${i}].stable_on yalnızca "anon_id" olabilir`, 'invalid_flag')
    })
    const d = body.default ?? null
    if (!(d === null || typeof d == 'boolean' || typeof d == 'string')) throw badRequest('"default" null, boolean ya da metin olmalıdır', 'invalid_flag')
    return {
      name: text(body.name, 'name', 200, false),
      description: text(body.description, 'description', 2000, false),
      enabled: !!body.enabled,
      rules,
      default: d,
    }
  }
  const recordFlagVersion = (flag, actor) => {
    db.flagVersions.push({ project_id: flag.project_id, key: flag.key, version: flag.version, ts: flag.updated_at, actor, flag: structuredClone(flagView(flag)) })
  }

  // ---- routes ----

  /** @type {{ method: string, pattern: RegExp, auth: boolean, handler: Function }[]} */
  const routes = []
  const route = (method, pattern, handler, auth = true) => {
    const source = '^' + pattern.replace(/:[a-z]+/g, '([^/]+)') + '$'
    routes.push({ method, pattern: new RegExp(source), handler, auth })
  }

  // -- setup & auth --
  route('GET', '/api/setup', () => ({ needs_setup: db.users.length == 0 }), false)
  route('POST', '/api/setup', (ctx) => {
    if (db.users.length) throw new HttpError(409, 'already_set_up', 'Kurulum zaten tamamlanmış')
    const email = checkEmail(ctx.body.email)
    checkPassword(ctx.body.password)
    const user = { id: nextId(), email, password: hashPassword(ctx.body.password), totp_secret: null, totp_pending: null, totp_enabled: false }
    const org = { id: nextId(), name: text(ctx.body.org_name, 'org_name', 100) }
    db.users.push(user)
    db.orgs.push(org)
    db.memberships.push({ org_id: org.id, user_id: user.id, role: 'owner', created_at: now() })
    audit(user.id, 'setup', org.id, { org_name: org.name })
    openSession(ctx.res, user)
    return { ok: true }
  }, false)

  route('POST', '/api/auth/login', (ctx) => {
    const email = typeof ctx.body.email == 'string' ? ctx.body.email.trim().toLowerCase() : ''
    const recent = (db.loginFailures.get(email) ?? []).filter((t) => t > now() - 60_000)
    if (recent.length >= 5) throw new HttpError(429, 'rate_limited', 'Çok fazla deneme yapıldı; bir dakika sonra yeniden deneyin')
    const user = db.users.find((u) => u.email == email)
    const fail = () => {
      db.loginFailures.set(email, [...recent, now()])
      return new HttpError(401, 'invalid_credentials', 'E-posta ya da parola hatalı')
    }
    if (!user || user.password != hashPassword(String(ctx.body.password ?? ''))) throw fail()
    if (user.totp_enabled) {
      if (!ctx.body.totp) throw new HttpError(401, 'totp_required', 'Doğrulama kodu gerekli')
      if (!verifyTotp(user.totp_secret, String(ctx.body.totp), now())) {
        db.loginFailures.set(email, [...recent, now()])
        throw new HttpError(401, 'invalid_totp', 'Doğrulama kodu hatalı')
      }
    }
    openSession(ctx.res, user)
    audit(user.id, 'auth.login', user.id)
    return { ok: true }
  }, false)

  route('POST', '/api/auth/logout', (ctx) => {
    db.sessions.delete(sha256(cookieOf(ctx.req)))
    ctx.res.setHeader('Set-Cookie', 'la_session=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0')
    return { ok: true }
  })

  route('GET', '/api/me', (ctx) => ({
    user: { id: ctx.user.id, email: ctx.user.email, totp_enabled: ctx.user.totp_enabled },
    orgs: db.memberships
      .filter((m) => m.user_id == ctx.user.id)
      .map((m) => ({ id: m.org_id, name: db.orgs.find((o) => o.id == m.org_id).name, role: m.role })),
    csrf: ctx.session.csrf,
  }))

  route('POST', '/api/me/password', (ctx) => {
    if (ctx.user.password != hashPassword(String(ctx.body.current ?? ''))) throw new HttpError(403, 'invalid_credentials', 'Mevcut parola hatalı')
    checkPassword(ctx.body.new)
    ctx.user.password = hashPassword(ctx.body.new)
    audit(ctx.user.id, 'user.password', ctx.user.id)
    return { ok: true }
  })
  route('POST', '/api/me/totp/begin', (ctx) => {
    if (ctx.user.totp_enabled) throw new HttpError(409, 'totp_already_enabled', 'İki adımlı doğrulama zaten açık')
    ctx.user.totp_pending = newSecret()
    const label = encodeURIComponent('İzlek:' + ctx.user.email)
    return { secret: ctx.user.totp_pending, otpauth_url: `otpauth://totp/${label}?secret=${ctx.user.totp_pending}&issuer=%C4%B0zlek&algorithm=SHA1&digits=6&period=30` }
  })
  route('POST', '/api/me/totp/enable', (ctx) => {
    if (!ctx.user.totp_pending) throw badRequest('Önce kurulumu başlatın', 'totp_not_started')
    if (!verifyTotp(ctx.user.totp_pending, String(ctx.body.code ?? ''), now())) throw badRequest('Doğrulama kodu hatalı', 'invalid_totp')
    ctx.user.totp_secret = ctx.user.totp_pending
    ctx.user.totp_pending = null
    ctx.user.totp_enabled = true
    audit(ctx.user.id, 'user.totp_enable', ctx.user.id)
    return { ok: true }
  })
  route('POST', '/api/me/totp/disable', (ctx) => {
    if (ctx.user.password != hashPassword(String(ctx.body.password ?? ''))) throw new HttpError(403, 'invalid_credentials', 'Parola hatalı')
    ctx.user.totp_secret = null
    ctx.user.totp_enabled = false
    audit(ctx.user.id, 'user.totp_disable', ctx.user.id)
    return { ok: true }
  })

  // -- org members & audit --
  const orgFor = (ctx, min) => {
    const org = db.orgs.find((o) => String(o.id) == ctx.params[0])
    if (!org) throw notFound()
    requireRole(ctx, org.id, min)
    return org
  }
  const memberView = (m) => ({ user_id: m.user_id, email: db.users.find((u) => u.id == m.user_id).email, role: m.role, created_at: m.created_at })
  const checkRole = (role) => {
    if (!(role in RANK)) throw badRequest('"role": owner | admin | member | viewer')
    return role
  }
  const lastOwner = (org, member) =>
    member.role == 'owner' && db.memberships.filter((m) => m.org_id == org.id && m.role == 'owner').length == 1

  route('GET', '/api/orgs/:org/members', (ctx) => {
    const org = orgFor(ctx, 'viewer')
    return db.memberships.filter((m) => m.org_id == org.id).map(memberView)
  })
  route('POST', '/api/orgs/:org/members', (ctx) => {
    const org = orgFor(ctx, 'owner')
    const email = checkEmail(ctx.body.email)
    const role = checkRole(ctx.body.role)
    let user = db.users.find((u) => u.email == email)
    if (!user) {
      checkPassword(ctx.body.password)
      user = { id: nextId(), email, password: hashPassword(ctx.body.password), totp_secret: null, totp_pending: null, totp_enabled: false }
      db.users.push(user)
    }
    if (roleIn(user.id, org.id)) throw new HttpError(409, 'conflict', 'Bu kullanıcı zaten üye')
    const member = { org_id: org.id, user_id: user.id, role, created_at: now() }
    db.memberships.push(member)
    audit(ctx.user.id, 'member.add', user.id, { email, role })
    return memberView(member)
  })
  const memberFor = (ctx, org) => {
    const member = db.memberships.find((m) => m.org_id == org.id && String(m.user_id) == ctx.params[1])
    if (!member) throw notFound()
    return member
  }
  route('PATCH', '/api/orgs/:org/members/:id', (ctx) => {
    const org = orgFor(ctx, 'owner')
    const member = memberFor(ctx, org)
    const role = checkRole(ctx.body.role)
    if (role != 'owner' && lastOwner(org, member)) throw new HttpError(409, 'last_owner', 'Son sahibin rolü düşürülemez')
    member.role = role
    audit(ctx.user.id, 'member.update', member.user_id, { role })
    return memberView(member)
  })
  route('DELETE', '/api/orgs/:org/members/:id', (ctx) => {
    const org = orgFor(ctx, 'owner')
    const member = memberFor(ctx, org)
    if (lastOwner(org, member)) throw new HttpError(409, 'last_owner', 'Son sahip çıkarılamaz')
    db.memberships.splice(db.memberships.indexOf(member), 1)
    audit(ctx.user.id, 'member.remove', member.user_id)
    return null
  })
  route('GET', '/api/orgs/:org/audit', (ctx) => {
    const org = orgFor(ctx, 'admin')
    const members = new Set(db.memberships.filter((m) => m.org_id == org.id).map((m) => m.user_id))
    const limit = Math.max(1, Math.min(500, Number(ctx.query.get('limit')) || 50))
    const before = ctx.query.get('before') ? Number(ctx.query.get('before')) : Infinity
    return db.audit
      .filter((a) => members.has(a.actor) && a.id < before)
      .sort((a, b) => b.id - a.id)
      .slice(0, limit)
  })

  // -- projects --
  route('GET', '/api/projects', (ctx) => {
    const orgs = new Set(db.memberships.filter((m) => m.user_id == ctx.user.id).map((m) => m.org_id))
    return db.projects.filter((p) => orgs.has(p.org_id) && !p.closed).map(projectView)
  })
  route('POST', '/api/projects', (ctx) => {
    const org = db.orgs.find((o) => o.id == ctx.body.org_id)
    if (!org) throw notFound()
    requireRole(ctx, org.id, 'admin')
    return projectView(createProject(org.id, text(ctx.body.name, 'name', 100), ctx.user.id))
  })
  route('GET', '/api/projects/:pid', (ctx) => projectView(projectFor(ctx, ctx.params[0], 'viewer')))
  route('PATCH', '/api/projects/:pid', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const b = ctx.body
    const patch = {}
    if (b.name !== undefined) patch.name = text(b.name, 'name', 100)
    if (b.retention_days !== undefined) patch.retention_days = int(b.retention_days, 'retention_days', 1, 3650)
    if (b.replay_retention_days !== undefined) patch.replay_retention_days = int(b.replay_retention_days, 'replay_retention_days', 1, 3650)
    if (b.drop_keys !== undefined) patch.drop_keys = strings(b.drop_keys, 'drop_keys')
    if (b.scan_limit_bytes !== undefined) patch.scan_limit_bytes = int(b.scan_limit_bytes, 'scan_limit_bytes', 0, Number.MAX_SAFE_INTEGER)
    if (b.max_concurrent_queries !== undefined) patch.max_concurrent_queries = int(b.max_concurrent_queries, 'max_concurrent_queries', 1, 64)
    if (b.replay_sample_rate !== undefined) {
      if (typeof b.replay_sample_rate != 'number' || b.replay_sample_rate < 0 || b.replay_sample_rate > 1) throw badRequest('"replay_sample_rate" 0 ile 1 arasında olmalıdır')
      patch.replay_sample_rate = b.replay_sample_rate
    }
    Object.assign(p, patch)
    audit(ctx.user.id, 'project.update', p.id, patch)
    return projectView(p)
  })
  route('DELETE', '/api/projects/:pid', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    p.closed = true
    p.retention_days = 0
    for (const t of db.tokens) if (t.project_id == p.id) t.status = 'disabled'
    audit(ctx.user.id, 'project.close', p.id)
    return null
  })

  // -- tokens --
  route('GET', '/api/projects/:pid/tokens', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'viewer')
    return db.tokens.filter((t) => t.project_id == p.id).map(tokenView)
  })
  route('POST', '/api/projects/:pid/tokens', (ctx) => tokenView(createToken(projectFor(ctx, ctx.params[0], 'admin'), ctx.body, ctx.user.id)))
  const tokenFor = (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const token = db.tokens.find((t) => t.project_id == p.id && t.token == ctx.params[1])
    if (!token) throw notFound()
    return token
  }
  route('PATCH', '/api/projects/:pid/tokens/:token', (ctx) => {
    const token = tokenFor(ctx)
    const b = ctx.body
    if (b.name !== undefined) token.name = text(b.name, 'name', 100)
    if (b.status !== undefined) {
      if (b.status != 'active' && b.status != 'disabled') throw badRequest('"status": active | disabled')
      token.status = b.status
    }
    if (b.allowed_origins !== undefined) token.allowed_origins = strings(b.allowed_origins, 'allowed_origins')
    if (b.rate_limit !== undefined) token.rate_limit = int(b.rate_limit, 'rate_limit', 0, 1_000_000)
    if (b.replay !== undefined) token.replay = !!b.replay
    audit(ctx.user.id, 'token.update', token.token, { name: token.name, status: token.status })
    return tokenView(token)
  })
  route('DELETE', '/api/projects/:pid/tokens/:token', (ctx) => {
    const token = tokenFor(ctx)
    db.tokens.splice(db.tokens.indexOf(token), 1)
    audit(ctx.user.id, 'token.delete', token.token)
    return null
  })

  // -- secret keys --
  route('GET', '/api/projects/:pid/keys', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    return db.keys.filter((k) => k.project_id == p.id).map(keyView)
  })
  route('POST', '/api/projects/:pid/keys', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const scopes = ctx.body.scopes
    if (!Array.isArray(scopes) || !scopes.length || scopes.some((s) => s != 'query' && s != 'admin')) throw badRequest('"scopes": query ve/veya admin')
    const secret = 'sk_' + base62(32)
    const key = {
      project_id: p.id,
      id: nextId(),
      name: text(ctx.body.name, 'name', 100),
      scopes: [...new Set(scopes)],
      prefix: 'sk_…' + secret.slice(-4),
      created_at: now(),
      last_used_at: null,
      hash: sha256(secret),
    }
    db.keys.push(key)
    audit(ctx.user.id, 'key.create', key.id, { name: key.name, scopes: key.scopes })
    return { ...keyView(key), secret }
  })
  route('DELETE', '/api/projects/:pid/keys/:id', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const i = db.keys.findIndex((k) => k.project_id == p.id && String(k.id) == ctx.params[1])
    if (i < 0) throw notFound()
    audit(ctx.user.id, 'key.delete', db.keys[i].id)
    db.keys.splice(i, 1)
    return null
  })

  // -- flags --
  route('GET', '/api/projects/:pid/flags', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'viewer')
    return db.flags.filter((f) => f.project_id == p.id).map(flagView)
  })
  route('POST', '/api/projects/:pid/flags', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const key = ctx.body.key
    if (typeof key != 'string' || !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(key))
      throw badRequest('"key" küçük harf, rakam, "_" ve "-" içerebilir; harf ya da rakamla başlamalıdır (en çok 64 karakter)', 'invalid_flag')
    if (db.flags.some((f) => f.project_id == p.id && f.key == key)) throw new HttpError(409, 'conflict', `"${key}" anahtarlı bir flag zaten var`)
    const flag = { project_id: p.id, key, ...validateFlag(ctx.body), salt: base62(4).toLowerCase(), version: 1, updated_at: now(), updated_by: ctx.user.id }
    db.flags.push(flag)
    recordFlagVersion(flag, ctx.user.id)
    audit(ctx.user.id, 'flag.create', key, { project_id: p.id })
    return flagView(flag)
  })
  const flagFor = (ctx, min) => {
    const p = projectFor(ctx, ctx.params[0], min)
    const flag = db.flags.find((f) => f.project_id == p.id && f.key == ctx.params[1])
    if (!flag) throw notFound()
    return flag
  }
  route('GET', '/api/projects/:pid/flags/:key', (ctx) => flagView(flagFor(ctx, 'viewer')))
  route('PUT', '/api/projects/:pid/flags/:key', (ctx) => {
    const flag = flagFor(ctx, 'admin')
    Object.assign(flag, validateFlag(ctx.body), { version: flag.version + 1, updated_at: now(), updated_by: ctx.user.id })
    recordFlagVersion(flag, ctx.user.id)
    audit(ctx.user.id, 'flag.update', flag.key, { project_id: flag.project_id, version: flag.version, enabled: flag.enabled })
    return flagView(flag)
  })
  route('DELETE', '/api/projects/:pid/flags/:key', (ctx) => {
    const flag = flagFor(ctx, 'admin')
    db.flags.splice(db.flags.indexOf(flag), 1)
    audit(ctx.user.id, 'flag.delete', flag.key, { project_id: flag.project_id })
    return null
  })
  route('GET', '/api/projects/:pid/flags/:key/versions', (ctx) => {
    const flag = flagFor(ctx, 'viewer')
    return db.flagVersions
      .filter((v) => v.project_id == flag.project_id && v.key == flag.key)
      .map(({ project_id: _p, key: _k, ...v }) => v)
      .sort((a, b) => b.version - a.version)
  })
  route('POST', '/api/projects/:pid/flags/:key/evaluate', (ctx) => {
    const flag = flagFor(ctx, 'viewer')
    const props = ctx.body.props ?? {}
    if (typeof props != 'object' || Array.isArray(props)) throw badRequest('"props" bir nesne olmalıdır')
    const value = evalFlag(flag, props, String(ctx.body.user_id ?? ''), String(ctx.body.anon_id ?? ''))
    return { value: value === false ? null : value }
  })

  // -- insights & dashboards --
  const KINDS = ['trends', 'funnel', 'retention', 'sql']
  const crud = (name, store, build, action) => {
    const base = `/api/projects/:pid/${name}`
    const find = (ctx, min) => {
      const p = projectFor(ctx, ctx.params[0], min)
      const item = store().find((x) => x.project_id == p.id && String(x.id) == ctx.params[1])
      if (!item) throw notFound()
      return item
    }
    const view = ({ project_id: _p, ...x }) => x
    route('GET', base, (ctx) => {
      const p = projectFor(ctx, ctx.params[0], 'viewer')
      return store().filter((x) => x.project_id == p.id).map(view)
    })
    route('POST', base, (ctx) => {
      const p = projectFor(ctx, ctx.params[0], 'member')
      const item = { project_id: p.id, id: nextId(), ...build(ctx, null, p), updated_at: now() }
      store().push(item)
      audit(ctx.user.id, action + '.create', item.id, { name: item.name })
      return view(item)
    })
    route('GET', base + '/:id', (ctx) => view(find(ctx, 'viewer')))
    route('PUT', base + '/:id', (ctx) => {
      const item = find(ctx, 'member')
      Object.assign(item, build(ctx, item, { id: item.project_id }), { updated_at: now() })
      audit(ctx.user.id, action + '.update', item.id, { name: item.name })
      return view(item)
    })
    route('DELETE', base + '/:id', (ctx) => {
      const item = find(ctx, 'member')
      store().splice(store().indexOf(item), 1)
      audit(ctx.user.id, action + '.delete', item.id)
      return null
    })
  }
  crud('insights', () => db.insights, (ctx, existing) => {
    const kind = ctx.body.kind ?? existing?.kind
    if (!KINDS.includes(kind)) throw badRequest('"kind": trends | funnel | retention | sql')
    if (!ctx.body.query || typeof ctx.body.query != 'object') throw badRequest('"query" zorunludur')
    return { name: text(ctx.body.name, 'name', 200), kind, query: ctx.body.query, created_by: existing?.created_by ?? ctx.user.id }
  }, 'insight')
  crud('dashboards', () => db.dashboards, (ctx, _existing, project) => {
    const items = ctx.body.items ?? []
    if (!Array.isArray(items)) throw badRequest('"items" bir dizi olmalıdır')
    for (const it of items) {
      if (!db.insights.some((i) => i.project_id == project.id && i.id == it.insight_id)) throw badRequest(`Bilinmeyen insight: ${it.insight_id}`)
      for (const k of ['x', 'y', 'w', 'h']) if (!Number.isInteger(it[k]) || it[k] < 0) throw badRequest(`items[].${k} negatif olmayan bir tam sayı olmalıdır`)
    }
    return { name: text(ctx.body.name, 'name', 200), items: items.map(({ insight_id, x, y, w, h }) => ({ insight_id, x, y, w, h })) }
  }, 'dashboard')

  // -- deletions --
  route('GET', '/api/projects/:pid/deletions', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    // the real control plane flips to "done" once every day manifest has caught up; here: after 30 s
    for (const d of db.deletions) if (d.status == 'pending' && now() - d.created_at > 30_000) d.status = 'done'
    return db.deletions
      .filter((d) => d.project_id == p.id)
      .map(({ project_id: _p, ...d }) => d)
      .sort((a, b) => b.created_at - a.created_at)
  })
  route('POST', '/api/projects/:pid/deletions', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'admin')
    const user_ids = strings(ctx.body.user_ids, 'user_ids')
    const anon_ids = strings(ctx.body.anon_ids, 'anon_ids')
    if (!user_ids.length && !anon_ids.length) throw badRequest('En az bir kullanıcı ya da anonim kimlik gerekli')
    const d = {
      project_id: p.id,
      id: 'del_' + base62(12),
      user_ids,
      anon_ids,
      status: 'pending',
      created_at: now(),
      created_by: ctx.user.id,
      note: text(ctx.body.note, 'note', 1000, false),
    }
    db.deletions.push(d)
    audit(ctx.user.id, 'deletion.create', d.id, { users: user_ids.length, anons: anon_ids.length })
    const { project_id: _p, ...view } = d
    return view
  })

  // -- query proxy --
  route('POST', '/api/projects/:pid/q/:endpoint', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'viewer')
    return engine.run(ctx.params[1], ctx.body, p.id)
  })
  route('GET', '/api/projects/:pid/q/replay', (ctx) => {
    const p = projectFor(ctx, ctx.params[0], 'viewer')
    const chunk = engine.replayChunk(ctx.query.get('key'), p.id)
    // The query service sets Content-Encoding (spec/query.md §8), so browsers
    // decode transparently. Odd chunks are served without the header, as some
    // intermediaries would, to keep the UI's own gzip path exercised.
    // `hooks.rawReplay` (tests) drops it everywhere, zstd included.
    const headers = { 'Content-Type': 'application/octet-stream', 'Cache-Control': 'no-store' }
    if (!hooks.rawReplay && (chunk.encoding == 'zstd' || chunk.seq % 2 == 0)) headers['Content-Encoding'] = chunk.encoding
    ctx.res.writeHead(200, { ...SECURITY_HEADERS, ...headers })
    ctx.res.end(chunk.body)
    return undefined
  })

  // ---- HTTP plumbing ----

  const readBody = (req) =>
    new Promise((resolve, reject) => {
      const chunks = []
      let size = 0
      req.on('data', (c) => {
        size += c.length
        if (size > 1_048_576) reject(new HttpError(413, 'too_large', 'İstek gövdesi çok büyük'))
        else chunks.push(c)
      })
      req.on('end', () => {
        const raw = Buffer.concat(chunks).toString('utf8')
        if (!raw) return resolve({})
        try {
          const parsed = JSON.parse(raw)
          resolve(parsed && typeof parsed == 'object' ? parsed : {})
        } catch {
          reject(badRequest('Geçersiz JSON'))
        }
      })
      req.on('error', reject)
    })

  const sendJson = (res, status, body) => {
    if (res.writableEnded) return
    const payload = body === null || body === undefined ? '' : JSON.stringify(body)
    res.writeHead(body === null ? 204 : status, { ...SECURITY_HEADERS, 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
    res.end(payload)
  }
  const sendError = (res, e) => {
    const known = e instanceof HttpError || e instanceof QueryError
    if (!known) console.error(e)
    sendJson(res, known ? e.status : 500, { error: { code: known ? e.code : 'internal', message: known ? e.message : 'Beklenmeyen sunucu hatası' } })
  }

  const handleApi = async (req, res, url) => {
    try {
      const matched = routes.map((r) => ({ r, m: r.pattern.exec(url.pathname) })).filter((x) => x.m)
      if (!matched.length) throw notFound()
      const hit = matched.find((x) => x.r.method == req.method)
      if (!hit) throw new HttpError(405, 'method_not_allowed', 'Bu yöntem desteklenmiyor')
      const ctx = { req, res, query: url.searchParams, params: hit.m.slice(1).map(decodeURIComponent), body: {}, session: null, user: null }
      if (hit.r.auth) {
        ctx.session = sessionOf(req)
        if (!ctx.session) throw new HttpError(401, 'unauthorized', 'Oturum açmanız gerekiyor')
        ctx.user = db.users.find((u) => u.id == ctx.session.user_id)
        if (!['GET', 'HEAD'].includes(req.method) && req.headers['x-la-csrf'] !== ctx.session.csrf)
          throw new HttpError(403, 'csrf', 'CSRF doğrulaması başarısız; sayfayı yenileyip yeniden deneyin')
      }
      if (req.method != 'GET' && req.method != 'HEAD') ctx.body = await readBody(req)
      const result = await hit.r.handler(ctx)
      if (result !== undefined) sendJson(res, 200, result)
    } catch (e) {
      sendError(res, e)
    }
  }

  const handleStatic = async (req, res, url) => {
    if (req.method != 'GET' && req.method != 'HEAD') return sendJson(res, 405, { error: { code: 'method_not_allowed', message: 'Bu yöntem desteklenmiyor' } })
    const rel = path.normalize(decodeURIComponent(url.pathname)).replace(/^(\.\.[/\\])+/, '')
    let file = path.join(uiDir, rel)
    let body
    try {
      if (!file.startsWith(uiDir) || rel == '/' || !path.extname(rel)) throw new Error('fallback')
      body = await readFile(file)
    } catch {
      // unknown paths fall back to the SPA shell
      file = path.join(uiDir, 'index.html')
      try {
        body = await readFile(file)
      } catch {
        res.writeHead(503, { 'Content-Type': 'text/plain; charset=utf-8' })
        return res.end('UI derlenmemiş: önce "npm run build" çalıştırın.\n')
      }
    }
    const ext = path.extname(file)
    res.writeHead(200, {
      ...SECURITY_HEADERS,
      'Content-Type': MIME[ext] ?? 'application/octet-stream',
      'Cache-Control': rel.startsWith('/assets/') && ext != '.html' ? 'public, max-age=31536000, immutable' : 'no-cache',
    })
    res.end(req.method == 'HEAD' ? undefined : body)
  }

  /** Proxy mode: /api/ goes to a real control plane, untouched in both directions. */
  const upstream = options.controlUrl ? new URL(options.controlUrl) : null
  const forward = (req, res) => {
    const send = upstream.protocol == 'https:' ? httpsRequest : httpRequest
    const out = send(
      { protocol: upstream.protocol, hostname: upstream.hostname, port: upstream.port, method: req.method, path: req.url, headers: { ...req.headers, host: upstream.host } },
      (up) => {
        res.writeHead(up.statusCode ?? 502, up.headers)
        up.pipe(res)
      },
    )
    out.on('error', (e) => {
      if (res.headersSent) return res.destroy()
      sendJson(res, 502, { error: { code: 'bad_gateway', message: `Control plane'e ulaşılamıyor (${upstream.origin}): ${e.message}` } })
    })
    req.pipe(out)
  }

  const server = createServer((req, res) => {
    const url = new URL(req.url, 'http://localhost')
    if (url.pathname.startsWith('/api/')) return upstream ? forward(req, res) : void handleApi(req, res, url)
    if (url.pathname == '/healthz' || url.pathname == '/readyz') {
      res.writeHead(200, { 'Content-Type': 'text/plain' })
      return res.end('ok\n')
    }
    void handleStatic(req, res, url)
  })

  /** Demo content so `npm run dev` opens onto a populated dashboard. */
  const seed = () => {
    const mk = (email, password) => {
      const user = { id: nextId(), email, password: hashPassword(password), totp_secret: null, totp_pending: null, totp_enabled: false }
      db.users.push(user)
      return user
    }
    const owner = mk('demo@izlek.local', 'izlek-demo-123')
    const viewer = mk('izleyici@izlek.local', 'izlek-demo-123')
    const org = { id: nextId(), name: 'Demo A.Ş.' }
    db.orgs.push(org)
    db.memberships.push({ org_id: org.id, user_id: owner.id, role: 'owner', created_at: now() - 40 * 86_400_000 })
    db.memberships.push({ org_id: org.id, user_id: viewer.id, role: 'viewer', created_at: now() - 12 * 86_400_000 })
    const project = createProject(org.id, 'Mağaza (mobil + web)', owner.id)
    createProject(org.id, 'Deneme ortamı', owner.id)
    db.tokens[0].replay = true
    db.tokens[0].allowed_origins = ['https://magaza.example.com']
    const flag = (key, name, enabled, rules, def = null) => {
      const f = { project_id: project.id, key, name, description: '', enabled, salt: base62(4).toLowerCase(), rules, default: def, version: 1, updated_at: now(), updated_by: owner.id }
      db.flags.push(f)
      recordFlagVersion(f, owner.id)
    }
    flag('new_checkout', 'Yeni ödeme akışı', true, [
      {
        when: { and: [{ prop: 'platform', op: 'eq', value: 'ios' }, { prop: 'app_version', op: 'semver_gte', value: '3.2.0' }] },
        rollout: 2500,
        variants: [{ key: 'control', w: 5000 }, { key: 'b', w: 5000 }],
      },
    ])
    flag('dark_mode', 'Koyu tema', true, [{ rollout: 10000 }])
    flag('search_v2', 'Arama v2', false, [{ when: { prop: 'country', op: 'in', value: ['TR', 'DE'] }, rollout: 5000, stable_on: 'anon_id' }], 'legacy')
    const insight = (name, kind, query) => {
      const i = { project_id: project.id, id: nextId(), name, kind, query, created_by: owner.id, updated_at: now() }
      db.insights.push(i)
      return i.id
    }
    const a = insight('Günlük aktif kişi ve satın alma', 'trends', {
      range: '30d',
      interval: 'day',
      series: [{ event: null, agg: 'unique', label: 'Aktif kişi' }, { event: 'purchase', agg: 'count', label: 'Satın alma' }],
    })
    const b = insight('Kayıt → satın alma hunisi', 'funnel', {
      range: '30d',
      steps: [{ event: 'signup' }, { event: 'product_viewed' }, { event: 'add_to_cart' }, { event: 'purchase' }],
      window_seconds: 604800,
      order: 'ordered',
    })
    const c = insight('Haftalık tutundurma', 'retention', { range: '60d', period: 'week', periods: 8, start: { event: 'signup' }, return: { event: null } })
    const d = insight('Platforma göre ciro', 'trends', {
      range: '30d',
      interval: 'week',
      series: [{ event: 'purchase', agg: 'sum', prop: 'props.amount', label: 'Ciro (₺)' }],
      breakdown: 'platform',
      breakdown_limit: 10,
      ui: { chart: 'bar' },
    })
    insight('En sık event\'ler', 'sql', { range: '7d', sql: 'SELECT event, count(*) FROM events GROUP BY 1 ORDER BY 2 DESC LIMIT 10' })
    db.dashboards.push({
      project_id: project.id,
      id: nextId(),
      name: 'Genel bakış',
      items: [
        { insight_id: a, x: 0, y: 0, w: 8, h: 3 },
        { insight_id: d, x: 8, y: 0, w: 4, h: 3 },
        { insight_id: b, x: 0, y: 3, w: 6, h: 3 },
        { insight_id: c, x: 6, y: 3, w: 6, h: 3 },
      ],
      updated_at: now(),
    })
    db.deletions.push({ project_id: project.id, id: 'del_' + base62(12), user_ids: ['1042'], anon_ids: [], status: 'done', created_at: now() - 9 * 86_400_000, created_by: owner.id, note: 'Destek talebi #1841' })
  }
  if (options.seed) seed()

  return {
    server,
    db,
    dataset,
    hooks,
    listen: (port = 0, host = '127.0.0.1') =>
      new Promise((resolve) => server.listen(port, host, () => resolve(server.address().port))),
    close: () => new Promise((resolve) => server.close(resolve)),
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) == fileURLToPath(import.meta.url)) {
  const controlUrl = process.env.LA_CONTROL_URL
  const mock = createMockServer({ seed: !controlUrl && !!process.env.MOCK_SEED && process.env.MOCK_SEED != '0', controlUrl })
  const port = await mock.listen(Number(process.env.PORT ?? (controlUrl ? 8089 : 8090)), process.env.HOST ?? '127.0.0.1')
  if (controlUrl) console.log(`İzlek UI: http://localhost:${port}  (/api/ → ${controlUrl})`)
  else {
    console.log(`İzlek mock control plane: http://localhost:${port}`)
    if (mock.db.users.length) console.log('Demo girişi: demo@izlek.local / izlek-demo-123 (izleyici: izleyici@izlek.local)')
    else console.log('Veritabanı boş: ilk açılışta kurulum ekranı gelir.')
  }
}
