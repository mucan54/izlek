// End-to-end test: the built UI in Chromium against the in-process mock
// control plane, from first-run setup to a viewer's read-only view.
//
//   node test/e2e/run.mjs            (expects dist/; `npm test` builds first)
//
// E2E_SHOTS=<dir> also writes light and dark screenshots of the main screens.
// Playwright is expected to be installed globally (not a package dependency).
import { createRequire } from 'node:module'
import { existsSync, mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import assert from 'node:assert/strict'
import { createMockServer } from '../../mock/server.mjs'
import { evalFlag } from '../../mock/expr.mjs'
import { totp } from '../../mock/totp.mjs'
import { REPLAY_SESSION, REPLAY_SESSION_ZSTD } from '../../mock/data.mjs'

const require = createRequire(import.meta.url)
const { chromium } = require('playwright')

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
if (!existsSync(path.join(root, 'dist/index.html'))) {
  console.error('dist/ is missing: run `npm run build` first')
  process.exit(1)
}
const shotsDir = process.env.E2E_SHOTS
if (shotsDir) mkdirSync(shotsDir, { recursive: true })

const OWNER = { email: 'sahip@ornek.com', password: 'cok-gizli-parola-1' }
const VIEWER = { email: 'izleyici@ornek.com', password: 'izleyici-parola-1' }

const mock = createMockServer({ seed: false })
const port = await mock.listen(0)
const base = `http://127.0.0.1:${port}`

const browser = await chromium.launch()
// The UI picks its language from the browser (Turkish only for tr-*, English otherwise) and Playwright's
// default locale is en-US: the scenario below asserts Turkish text, so the context is Turkish explicitly.
// The language step further down switches to English and back through the UI.
const context = await browser.newContext({ viewport: { width: 1366, height: 860 }, locale: 'tr-TR', colorScheme: 'light' })
await context.grantPermissions(['clipboard-read', 'clipboard-write'])
const page = await context.newPage()
page.setDefaultTimeout(8000)

/** Console noise that is part of the scenario: failed fetches the UI handles (401 on boot, 403 for viewers, …). */
const problems = []
page.on('console', (m) => {
  if (m.type() == 'error' && !/Failed to load resource: the server responded with a status of (400|401|403|404|409)/.test(m.text())) problems.push('console: ' + m.text())
})
page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))

const queries = [] // bodies sent to the query proxy
page.on('request', (r) => {
  const m = /\/q\/(trends|funnel|retention|events|sql)$/.exec(new URL(r.url()).pathname)
  if (m && r.method() == 'POST') queries.push({ endpoint: m[1], body: r.postDataJSON(), csrf: r.headers()['x-la-csrf'] })
})

const results = []
const step = async (name, fn) => {
  const started = Date.now()
  try {
    await fn()
    results.push({ name, ok: true })
    console.log(`  ✓ ${name} (${Date.now() - started} ms)`)
  } catch (e) {
    results.push({ name, ok: false, error: e })
    console.log(`  ✗ ${name}\n      ${String(e?.message ?? e).split('\n').join('\n      ')}`)
    if (shotsDir) await page.screenshot({ path: path.join(shotsDir, `FAILED-${results.length}.png`) }).catch(() => {})
  }
}

/** Light and dark screenshots of the current screen. */
const shot = async (name) => {
  if (!shotsDir) return
  await page.mouse.move(0, 0)
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: scheme })
    await page.waitForTimeout(60)
    await page.screenshot({ path: path.join(shotsDir, `${name}-${scheme}.png`) })
  }
  await page.emulateMedia({ colorScheme: 'light' })
}

const go = async (p) => {
  await page.goto(base + p)
  await page.waitForSelector('.main h1, .auth h1')
}
const heading = (text) => page.getByRole('heading', { level: 1 }).filter({ hasText: text })
const toastGone = () => page.waitForFunction(() => !document.querySelector('.toast'))
const login = async ({ email, password }) => {
  await page.getByLabel('E-posta').fill(email)
  await page.getByLabel('Parola').fill(password)
  await page.getByRole('button', { name: 'Oturum aç' }).click()
}
const logout = async () => {
  await page.getByRole('button', { name: 'Çıkış yap' }).click()
  await page.getByRole('heading', { name: 'Oturum aç' }).waitFor()
}
const noOverflow = async () => {
  const [scroll, width] = await page.evaluate(() => [document.documentElement.scrollWidth, window.innerWidth])
  assert.ok(scroll <= width, `page scrolls horizontally: ${scroll} > ${width}`)
}
/** Polls a Node-side condition (e.g. requests seen so far). */
const until = async (condition, what, timeout = 8000) => {
  const end = Date.now() + timeout
  while (!condition()) {
    if (Date.now() > end) throw new Error('timed out waiting: ' + what)
    await new Promise((r) => setTimeout(r, 25))
  }
}
/** Runs `fn` and returns the body of the query it triggers. */
const queryDuring = async (endpoint, fn) => {
  const from = queries.length
  await fn()
  await page.waitForFunction(() => !document.querySelector('.result-card.refreshing'))
  const sent = queries.slice(from).filter((q) => q.endpoint == endpoint)
  assert.ok(sent.length, `no ${endpoint} query was sent`)
  return sent[sent.length - 1]
}

let pid = 0
const insightIds = {}

console.log(`e2e against ${base}`)

await step('first-run setup creates the owner and signs in', async () => {
  await page.goto(base + '/')
  await page.getByRole('heading', { name: 'İlk kurulum' }).waitFor()
  await shot('01-setup')
  // a short password is rejected by the browser before it reaches the API
  await page.getByLabel('Organizasyon adı').fill('Örnek A.Ş.')
  await page.getByLabel('E-posta').fill(OWNER.email)
  await page.getByLabel('Parola').fill(OWNER.password)
  await page.getByRole('button', { name: 'Kurulumu tamamla' }).click()
  await heading('Projeler').waitFor()
  await page.getByText('Henüz proje yok').waitFor()
  assert.equal(mock.db.users.length, 1)
  assert.equal(new URL(page.url()).pathname, '/projects')
})

await step('logout, failed login shows the API message, login', async () => {
  await logout()
  await login({ email: OWNER.email, password: 'yanlis-parola-00' })
  await page.getByRole('alert').filter({ hasText: 'E-posta ya da parola hatalı' }).waitFor()
  await shot('02-login-error')
  await login(OWNER)
  await heading('Projeler').waitFor()
})

await step('create a project (lands on SDK snippets with its automatic token)', async () => {
  await page.getByRole('button', { name: 'Yeni proje' }).click()
  await page.getByLabel('Proje adı').fill('Mağaza')
  await page.getByRole('button', { name: 'Oluştur' }).click()
  await page.waitForURL(/\/p\/\d+\/settings\/sdk$/)
  pid = Number(/\/p\/(\d+)\//.exec(page.url())[1])
  const token = mock.db.tokens.find((t) => t.project_id == pid).token
  assert.match(token, /^pk_[0-9A-Za-z]{32}$/)
  await page.getByLabel('Ingestion adresi').fill('https://in.ornek.com')
  const snippet = page.getByTestId('snippet')
  await snippet.filter({ hasText: token }).waitFor()
  assert.match(await snippet.innerText(), /host: 'https:\/\/in\.ornek\.com'/)
  await page.getByRole('tab', { name: 'Android' }).click()
  assert.match(await snippet.innerText(), new RegExp(`Config\\(token = "${token}", host = "https://in.ornek.com"\\)`))
  await page.getByRole('tab', { name: 'iOS' }).click()
  assert.match(await snippet.innerText(), new RegExp(`Config\\(token: "${token}"`))
  await shot('03-sdk-snippets')
  // the project switcher now offers it
  assert.equal(await page.locator('#project-switch').inputValue(), String(pid))
})

await step('write token: create with origin and replay, edit, disable, copy', async () => {
  await page.getByRole('tab', { name: 'Yazma token’ları' }).click()
  await page.getByRole('button', { name: 'Yeni token' }).click()
  await page.getByLabel('Ad', { exact: true }).fill('Web – canlı')
  await page.getByLabel('İzinli kökenler').fill('magaza.ornek.com')
  await page.getByText('Geçersiz köken: magaza.ornek.com').waitFor()
  assert.ok(await page.getByRole('button', { name: 'Oluştur' }).isDisabled(), 'an invalid origin blocks the form')
  await page.getByLabel('İzinli kökenler').fill('https://magaza.ornek.com')
  await page.getByLabel('Hız sınırı').fill('500')
  await page.getByText('Oturum kaydı gönderebilir').click()
  await page.getByRole('button', { name: 'Oluştur' }).click()
  const row = page.locator('tr', { hasText: 'Web – canlı' })
  await row.waitFor()
  const created = mock.db.tokens.find((t) => t.name == 'Web – canlı')
  assert.deepEqual([created.allowed_origins, created.rate_limit, created.replay, created.status], [['https://magaza.ornek.com'], 500, true, 'active'])
  await row.getByRole('button', { name: 'Kopyala' }).click()
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), created.token)
  await shot('04-tokens')
  await row.getByRole('button', { name: 'Devre dışı bırak' }).click()
  await row.getByText('Devre dışı', { exact: true }).waitFor()
  assert.equal(created.status, 'disabled')
  await row.getByRole('button', { name: 'Etkinleştir' }).click()
  await row.getByText('Etkin', { exact: true }).waitFor()
  await noOverflow()
})

await step('secret key: the secret is shown exactly once', async () => {
  await page.getByRole('tab', { name: 'Gizli anahtarlar' }).click()
  await page.getByText('Bu projede gizli anahtar yok').waitFor()
  await page.getByRole('button', { name: 'Yeni gizli anahtar' }).click()
  await page.getByLabel('Ad', { exact: true }).fill('Raporlama')
  await page.getByRole('button', { name: 'Oluştur' }).click()
  const secret = (await page.getByTestId('secret').innerText()).trim()
  assert.match(secret, /^sk_[0-9A-Za-z]{32}$/)
  await page.getByText('Bu anahtar yalnızca şimdi gösterilir').waitFor()
  await shot('05-secret-once')
  await page.getByRole('button', { name: 'Kopyaladım, kapat' }).click()
  const row = page.locator('tr', { hasText: 'Raporlama' })
  await row.waitFor()
  assert.match(await row.innerText(), /sk_…[0-9A-Za-z]{4}/)
  assert.equal(await page.content().then((html) => html.includes(secret)), false, 'the secret is gone from the page')
  await page.reload()
  await page.locator('tr', { hasText: 'Raporlama' }).waitFor()
  assert.equal(await page.content().then((html) => html.includes(secret)), false, 'and never comes back')
  assert.equal(await page.evaluate((s) => JSON.stringify({ ...localStorage, ...sessionStorage }).includes(s), secret), false, 'nor is it stored')
})

await step('project settings: general form saves, validation blocks bad input', async () => {
  await page.getByRole('tab', { name: 'Genel' }).click()
  await page.getByLabel('Veri saklama süresi').fill('0')
  await page.getByText('1 ile 3650 arasında bir tam sayı girin.').waitFor()
  assert.ok(await page.getByRole('button', { name: 'Kaydet' }).isDisabled())
  await page.getByLabel('Veri saklama süresi').fill('180')
  await page.getByLabel('Oturum kaydı örnekleme oranı').fill('12,5')
  await page.getByLabel('Atılacak özellik anahtarları').fill('email\nphone\nemail')
  await page.getByRole('button', { name: 'Kaydet' }).click()
  await page.getByText('Ayarlar kaydedildi.').waitFor()
  const p = mock.db.projects.find((x) => x.id == pid)
  assert.deepEqual([p.retention_days, p.replay_sample_rate, p.drop_keys], [180, 0.125, ['email', 'phone']])
  await shot('06-settings')
})

await step('trend insight: series, filter, breakdown → chart + table, saved', async () => {
  await go(`/p/${pid}/insights`)
  await page.getByText('Henüz kayıtlı insight yok').waitFor()
  await page.getByRole('link', { name: '+ Trend' }).click()
  await page.locator('.chart svg path.line').first().waitFor() // the default query runs by itself

  const sent = await queryDuring('trends', async () => {
    await page.getByLabel('1. serinin event’i').fill('purchase')
    await page.getByRole('button', { name: '+ Seri ekle' }).click()
    await page.locator('select[aria-label="Hesaplama"]').nth(1).selectOption('unique')
    await page.locator('input[aria-label="Etiket"]').nth(1).fill('Aktif kişi')
    await page.getByLabel('Kırılım').fill('platform')
    // global filter: country in TR, DE
    await page.locator('.builder .form-section').last().getByRole('button', { name: '+ Koşul ekle' }).click()
    const cond = page.locator('.builder .form-section').last().locator('.filter-cond')
    await cond.getByLabel('Özellik').fill('country')
    await cond.getByLabel('Operatör').selectOption('in')
    await cond.getByLabel('Değer').fill('TR, DE')
    await page.locator('.legend-item').nth(3).waitFor()
  })
  assert.deepEqual(sent.body.series, [{ event: 'purchase', agg: 'count' }, { event: null, agg: 'unique', label: 'Aktif kişi' }])
  assert.deepEqual(sent.body.filter, { prop: 'country', op: 'in', value: ['TR', 'DE'] })
  assert.equal(sent.body.breakdown, 'platform')
  assert.match(sent.body.from, /^\d{4}-\d{2}-\d{2}$/)
  assert.match(sent.body.to, /^\d{4}-\d{2}-\d{2}$/)
  assert.equal('range' in sent.body, false, 'the relative range is resolved before the call')
  assert.equal((Date.parse(sent.body.to) - Date.parse(sent.body.from)) / 86_400_000, 29, '"last 30 days" includes today')
  assert.ok(sent.csrf, 'query POSTs carry the CSRF header')

  // chart: axes, legend, hover tooltip; table: one row per series
  assert.ok((await page.locator('.chart .tick').count()) > 6, 'axes are labelled')
  const legend = await page.locator('.legend-item').allInnerTexts()
  assert.ok(legend.some((l) => l.includes('Aktif kişi · web')), legend.join(' | '))
  await page.locator('.chart svg').hover({ position: { x: 420, y: 120 } })
  await page.locator('.tooltip').waitFor()
  assert.ok((await page.locator('.tooltip tr').count()) >= 2)
  assert.equal(await page.locator('.result-card table.table tbody tr').count(), legend.length)
  await shot('07-trends')

  // bar chart variant
  await page.getByLabel('Grafik').selectOption('bar')
  await page.locator('.chart svg path.bar').first().waitFor()

  await page.getByLabel('Ad', { exact: true }).fill('Satın alma ve aktif kişi')
  await page.getByRole('button', { name: 'Kaydet', exact: true }).click()
  await page.waitForURL(/\/insights\/\d+$/)
  const saved = mock.db.insights.find((i) => i.name == 'Satın alma ve aktif kişi')
  assert.equal(saved.kind, 'trends')
  assert.equal(saved.query.range, '30d', 'the stored query keeps the relative range')
  assert.equal(saved.query.from, undefined)
  assert.deepEqual(saved.query.ui, { chart: 'bar' })
  insightIds.trends = saved.id
  await page.locator('.chart svg path.bar').first().waitFor() // reloaded from the server, still a bar chart
  await toastGone()
})

await step('funnel insight: ordered steps with a step filter → bars with conversion and median time', async () => {
  await go(`/p/${pid}/insights/new/funnel`)
  await page.getByText('1. adım için bir event seçin.').first().waitFor() // incomplete: nothing runs yet
  const sent = await queryDuring('funnel', async () => {
    await page.getByLabel('1. adımın event’i').fill('signup')
    await page.getByLabel('2. adımın event’i').fill('purchase')
    await page.getByRole('button', { name: '+ Adım ekle' }).click()
    await page.getByLabel('3. adımın event’i').fill('product_viewed')
    await page.getByRole('button', { name: '3. adımı yukarı taşı' }).click() // signup → product_viewed → purchase
    // per-step filter on the last step
    const last = page.locator('.rows .row-item').nth(2)
    await last.getByRole('button', { name: /^Filtre/ }).click()
    await last.getByRole('button', { name: '+ Koşul ekle' }).click()
    await last.getByLabel('Özellik').fill('props.amount')
    await last.getByLabel('Operatör').selectOption('gt')
    await last.getByLabel('Değer').fill('100')
    await page.getByLabel('Dönüşüm penceresi').fill('14')
    await page.getByLabel('Kırılım').fill('platform')
    await page.locator('.funnel-step').nth(2).waitFor()
    await page.locator('.result-card table caption').waitFor()
  })
  assert.deepEqual(sent.body.steps, [{ event: 'signup' }, { event: 'product_viewed' }, { event: 'purchase', filter: { prop: 'props.amount', op: 'gt', value: 100 } }])
  assert.equal(sent.body.window_seconds, 14 * 86400)
  assert.equal(sent.body.order, 'ordered')
  const steps = await page.locator('.funnel-step').allInnerTexts()
  assert.equal(steps.length, 3)
  assert.match(steps[0], /signup[\s\S]*%100/)
  assert.match(steps[1], /Önceki adımdan: %\d+[\s\S]*Medyan süre: \d/)
  assert.match(await page.locator('.headline').innerText(), /^%\d+(,\d)? genel dönüşüm/)
  assert.ok((await page.locator('.result-card table tbody tr').count()) >= 2, 'breakdown table')
  await shot('08-funnel')
  await page.getByLabel('Ad', { exact: true }).fill('Kayıt → satın alma')
  await page.getByRole('button', { name: 'Kaydet', exact: true }).click()
  await page.waitForURL(/\/insights\/\d+$/)
  insightIds.funnel = mock.db.insights.find((i) => i.kind == 'funnel').id
  await toastGone()
})

await step('retention insight: cohort heatmap with numbers in every cell', async () => {
  await go(`/p/${pid}/insights/new/retention`)
  const sent = await queryDuring('retention', async () => {
    await page.getByLabel('Başlangıç event’i').fill('signup')
    await page.locator('table.retention').waitFor()
  })
  assert.deepEqual([sent.body.start, sent.body.return, sent.body.period, sent.body.periods], [{ event: 'signup' }, { event: null }, 'week', 8])
  const cells = await page.locator('table.retention td.heat').allInnerTexts()
  assert.ok(cells.length > 10)
  assert.ok(cells.every((c) => /^%\d/.test(c.trim())), 'every coloured cell also carries its number')
  assert.match(await page.locator('table.retention tbody tr').first().innerText(), /Ağırlıklı ortalama/)
  await shot('09-retention')
  await page.getByLabel('Ad', { exact: true }).fill('Haftalık tutundurma')
  await page.getByRole('button', { name: 'Kaydet', exact: true }).click()
  await page.waitForURL(/\/insights\/\d+$/)
  insightIds.retention = mock.db.insights.find((i) => i.kind == 'retention').id
  await toastGone()
})

await step('SQL insight: editor, API error message, result grid; rename and save-as; delete', async () => {
  await go(`/p/${pid}/insights/new/sql`)
  await page.locator('table.grid').waitFor()
  await page.getByLabel('SQL sorgusu').fill('DROP TABLE events')
  await page.getByRole('button', { name: 'Çalıştır' }).click()
  await page.getByRole('alert').filter({ hasText: 'Yalnızca tek bir SELECT sorgusu çalıştırılabilir' }).waitFor()
  await page.getByLabel('SQL sorgusu').fill('SELECT event, count(*) FROM events GROUP BY 1 ORDER BY 2 DESC LIMIT 5')
  await page.getByLabel('SQL sorgusu').press('Control+Enter')
  await page.locator('table.grid tbody tr').nth(4).waitFor()
  assert.equal(await page.locator('table.grid tbody tr').count(), 5)
  await shot('10-sql')
  await page.getByLabel('Ad', { exact: true }).fill('En sık event’ler')
  await page.getByRole('button', { name: 'Kaydet', exact: true }).click()
  await page.waitForURL(/\/insights\/\d+$/)
  await toastGone()
  // rename
  await page.getByLabel('Ad', { exact: true }).fill('Sık event’ler')
  await page.getByText('Kaydedilmemiş değişiklik var').waitFor()
  await page.getByRole('button', { name: 'Kaydet', exact: true }).click()
  await page.getByText('Insight kaydedildi.').waitFor()
  assert.ok(mock.db.insights.some((i) => i.name == 'Sık event’ler'))
  await toastGone()
  // save as copy, then delete the copy
  await page.getByRole('button', { name: 'Kopya olarak kaydet' }).click()
  await page.waitForFunction(() => document.querySelector('.title-input')?.value.endsWith('(kopya)'))
  assert.equal(mock.db.insights.length, 5)
  await page.getByRole('button', { name: 'Sil', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Sil', exact: true }).click()
  await page.waitForURL(/\/insights$/)
  await page.locator('tbody tr').nth(3).waitFor()
  assert.equal(mock.db.insights.length, 4)
  assert.equal(await page.locator('tbody tr').count(), 4)
  await shot('11-insight-list')
})

await step('dashboard: add the three insights, tiles re-run their queries, reorder and resize persist', async () => {
  await go(`/p/${pid}/dashboards`)
  await page.getByText('Henüz dashboard yok').waitFor()
  await page.getByRole('button', { name: 'Yeni dashboard' }).click()
  await page.getByLabel('Dashboard adı').fill('Genel bakış')
  await page.getByRole('button', { name: 'Oluştur' }).click()
  await page.waitForURL(/\/dashboards\/\d+$/)
  await page.getByText('Bu dashboard boş').waitFor()
  await page.getByRole('button', { name: 'Düzenle' }).click()
  for (const id of [insightIds.trends, insightIds.funnel, insightIds.retention]) {
    await page.getByLabel('Insight ekle').selectOption(String(id))
    const before = await page.locator('.tile').count()
    await page.getByRole('button', { name: 'Ekle', exact: true }).click()
    await page.locator('.tile').nth(before).waitFor()
  }
  const before = queries.length
  await page.locator('.tile').nth(0).locator('.chart svg').waitFor()
  await page.locator('.tile').nth(1).locator('.funnel').waitFor()
  await page.locator('.tile').nth(2).locator('table.retention').waitFor()
  assert.equal(await page.locator('.tile-foot').count(), 3, 'every tile shows cached/elapsed')
  assert.match(await page.locator('.tile-foot').first().innerText(), /(önbellekten|yeni hesaplandı) · \d+ ms/)

  // move the funnel first, make it full width
  const funnelTile = page.locator('.tile', { hasText: 'Kayıt → satın alma' })
  await funnelTile.getByRole('button', { name: 'Öne al' }).click()
  await page.waitForFunction(() => document.querySelector('.tile h2')?.textContent == 'Kayıt → satın alma')
  // the UI updates optimistically: wait for the PUT itself before looking at what the server stored
  const saved = page.waitForResponse((r) => r.request().method() == 'PUT' && /\/dashboards\/\d+$/.test(new URL(r.url()).pathname))
  await funnelTile.getByLabel('Genişlik').selectOption('12')
  await page.waitForFunction(() => document.querySelector('.tile')?.classList.contains('tile-w12'))
  assert.equal((await saved).status(), 200)
  await page.getByRole('button', { name: 'Düzenlemeyi bitir' }).click()
  const stored = mock.db.dashboards[0].items
  assert.deepEqual(
    stored.map((i) => [i.insight_id, i.x, i.y, i.w]),
    [[insightIds.funnel, 0, 0, 12], [insightIds.trends, 0, 3, 6], [insightIds.retention, 0, 6, 12]],
  )

  await page.reload()
  await page.locator('.tile').nth(2).locator('table.retention').waitFor()
  assert.deepEqual(await page.locator('.tile h2').allInnerTexts(), ['Kayıt → satın alma', 'Satın alma ve aktif kişi', 'Haftalık tutundurma'])
  // refresh re-runs every tile; identical queries come back from the result cache
  const n = queries.length
  await page.getByRole('button', { name: 'Yenile' }).click()
  await until(() => queries.length - n == 3, 'each of the three tiles re-runs its query')
  await page.waitForFunction(() => [...document.querySelectorAll('.tile-foot')].length == 3 && [...document.querySelectorAll('.tile-foot')].every((f) => f.textContent.includes('önbellekten')))
  assert.equal(queries.length - n, 3, 'and only once')
  assert.ok(queries.length > before)
  await page.locator('.tile .chart svg').waitFor()
  await shot('12-dashboard')
  await noOverflow()
})

await step('flag: two variants must sum to 100%, create, evaluate preview, history', async () => {
  await go(`/p/${pid}/flags`)
  await page.getByText('Henüz flag yok').waitFor()
  await page.getByRole('link', { name: 'Yeni flag' }).click()
  await page.getByLabel('Anahtar', { exact: true }).fill('Yeni Ödeme')
  await page.getByLabel('Ad', { exact: true }).fill('Yeni ödeme akışı')
  // condition: platform = ios
  await page.getByRole('button', { name: '+ Koşul ekle' }).click()
  await page.getByLabel('Özellik').fill('platform')
  await page.getByLabel('Değer').fill('ios')
  await page.getByLabel('Yayılım').fill('100')
  // two variants: added evenly, then unbalanced on purpose
  await page.getByRole('button', { name: '+ Varyant ekle' }).click()
  await page.getByRole('button', { name: '+ Varyant ekle' }).click()
  assert.equal(await page.getByLabel('1. varyantın ağırlığı (%)').inputValue(), '50')
  assert.equal(await page.getByLabel('2. varyantın anahtarı').inputValue(), 'test')
  await page.getByLabel('1. varyantın ağırlığı (%)').fill('60')
  await page.getByText('Ağırlıklar toplamı: %110 — %100 olmalı').waitFor()
  await page.getByText('Flag açık', { exact: true }).or(page.getByText('Flag kapalı', { exact: true })).first().click()
  await page.getByRole('button', { name: 'Flag’i oluştur' }).click()
  // both problems are reported and nothing was sent
  await page.getByText('Kaydetmeden önce 2 hatayı düzeltin.').waitFor()
  await page.getByText('Anahtar küçük harf ya da rakamla başlamalı').waitFor()
  assert.equal(mock.db.flags.length, 0)
  await shot('13-flag-validation')
  await page.getByLabel('Anahtar', { exact: true }).fill('new_checkout')
  await page.getByLabel('2. varyantın ağırlığı (%)').fill('40')
  await page.getByText('Ağırlıklar toplamı: %100 ✓').waitFor()
  await page.getByRole('button', { name: 'Flag’i oluştur' }).click()
  await page.waitForURL(/\/flags\/new_checkout$/)
  const flag = mock.db.flags[0]
  assert.deepEqual(flag.rules, [{ when: { prop: 'platform', op: 'eq', value: 'ios' }, rollout: 10000, variants: [{ key: 'control', w: 6000 }, { key: 'test', w: 4000 }] }])
  assert.equal(flag.enabled, true)
  assert.equal(flag.default, null)
  await shot('14-flag-editor')

  // preview: the server-side evaluation, for a few users
  await page.getByRole('tab', { name: 'Önizleme' }).click()
  // The salt is random per flag, so pick users by the reference algorithm: one per variant, plus a few more.
  const expected = (user) => evalFlag(flag, { platform: 'ios', app_version: '3.2.0' }, user, '')
  const pool = Array.from({ length: 200 }, (_, i) => `user-${i + 1}`)
  const users = [pool.find((u) => expected(u) == 'control'), pool.find((u) => expected(u) == 'test'), 'user-7', 'user-8']
  assert.ok(users[0] && users[1], 'both variants occur among 200 users')
  for (const user of users) {
    await page.getByLabel('Kullanıcı kimliği').fill(user)
    await page.getByRole('button', { name: 'Değerlendir' }).click()
    await page.getByTestId('eval-result').filter({ hasText: `Sonuç: "${expected(user)}" (varyant)` }).waitFor()
  }
  await shot('15-flag-preview')
  // a user the rule does not match gets the default
  await page.getByLabel('Özellikler (JSON)').fill('{ "platform": "android" }')
  await page.getByRole('button', { name: 'Değerlendir' }).click()
  await page.getByTestId('eval-result').filter({ hasText: 'null' }).waitFor()
  await page.getByLabel('Özellikler (JSON)').fill('{ nope')
  await page.getByText('Geçerli bir JSON nesnesi girin.').waitFor()

  // edit → version 2 → history
  await page.getByRole('tab', { name: 'Tanım' }).click()
  await page.getByLabel('Yayılım').fill('25')
  await page.getByRole('button', { name: 'Kaydet', exact: true }).first().click()
  await page.getByText('Flag kaydedildi ve yayınlandı.').waitFor()
  assert.equal(flag.rules[0].rollout, 2500)
  await page.getByRole('tab', { name: 'Sürüm geçmişi' }).click()
  await page.locator('tbody tr').nth(1).waitFor()
  const rows = await page.locator('tbody tr').allInnerTexts()
  assert.match(rows[0], /v2[\s\S]*güncel[\s\S]*%25/)
  assert.match(rows[1], /v1[\s\S]*%100/)
  await shot('16-flag-history')

  // list: summary and the enable switch
  await go(`/p/${pid}/flags`)
  const row = page.locator('tr', { hasText: 'new_checkout' })
  assert.match(await row.innerText(), /platform = ios · %25 → control %60, test %40/)
  await row.getByRole('switch').click({ force: true })
  await page.getByText('new_checkout kapatıldı.').waitFor()
  assert.equal(flag.enabled, false)
  assert.equal(flag.version, 3)
  await shot('17-flag-list')
})

await step('events: live feed with lag note, filter, expandable props, link to the person', async () => {
  await go(`/p/${pid}/events`)
  await page.getByText('~90 sn gecikmeli').waitFor()
  await page.locator('table.events tbody tr').first().waitFor()
  const first = queries.filter((q) => q.endpoint == 'events').at(-1).body
  assert.equal(first.order, 'desc')
  assert.match(first.from, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/, 'hour ranges are sent as RFC 3339 instants')
  await page.getByRole('button', { name: '+ Koşul ekle' }).click()
  await page.getByLabel('Özellik').fill('event')
  await page.getByLabel('Değer').fill('purchase')
  await page.waitForFunction(() => {
    const cells = [...document.querySelectorAll('table.events tbody tr:not(.row-detail) .event-name')]
    return cells.length > 0 && cells.every((c) => c.textContent == 'purchase')
  })
  await page.locator('table.events tbody tr').first().getByRole('button', { name: 'Ayrıntılar' }).click()
  const detail = page.locator('.event-detail')
  await detail.waitFor()
  assert.match(await detail.innerText(), /amount[\s\S]*currency[\s\S]*TRY/)
  await shot('18-events')
  await page.locator('table.events tbody tr').first().locator('a').click()
  await page.waitForURL(/\/persons\/[ua]%3A/)
})

await step('person: lookup by user id → properties, timeline, replay sessions', async () => {
  await go(`/p/${pid}/persons`)
  await page.getByLabel('Kimlik', { exact: true }).fill('yok-boyle-biri')
  await page.getByRole('button', { name: 'Bul' }).click()
  await page.getByText('Son 90 günde bu kimlikle event bulunamadı.').waitFor()
  await page.getByLabel('Kimlik', { exact: true }).fill('1001')
  await page.getByRole('button', { name: 'Bul' }).click()
  await page.waitForURL(/\/persons\/u%3A1001$/)
  await page.locator('table.events tbody tr').first().waitFor()
  const props = page.locator('.card', { hasText: 'Kişi özellikleri' })
  assert.match(await props.innerText(), /name\s+Ayşe[\s\S]*plan/)
  const names = await page.locator('table.events .event-name').allInnerTexts()
  assert.ok(names.includes('purchase'))
  assert.ok(!names.includes('session_replay_chunk'), 'index events are not part of the timeline')
  await page.getByRole('link', { name: '▶ Oynat' }).waitFor()
  await shot('19-person')
  await noOverflow()
})

await step('replay: chunks download in order, the canvas changes while playing, seek and speed work', async () => {
  const requests = []
  const onRequest = (r) => {
    if (r.url().includes('/q/replay?key=')) requests.push(decodeURIComponent(new URL(r.url()).searchParams.get('key')))
  }
  page.on('request', onRequest)
  await page.getByRole('link', { name: '▶ Oynat' }).click()
  await page.waitForURL(new RegExp(`/replay/${REPLAY_SESSION}`))
  const canvas = page.locator('canvas.replay-canvas')
  await canvas.waitFor()
  page.off('request', onRequest)
  assert.equal(requests.length, 5)
  assert.ok(requests.every((k) => k.startsWith(`replay/p=${pid}/`) && k.includes(REPLAY_SESSION)), 'keys carry the project prefix')
  assert.deepEqual([...requests].sort(), requests.map((_, i) => requests.find((k) => k.endsWith(`${String(i).padStart(6, '0')}.bin.gz`))), 'one request per chunk, seq 0…4')

  const snap = () => canvas.evaluate((c) => c.toDataURL())
  const time = () => canvas.evaluate((c) => Number(c.dataset.time))
  const blank = await page.evaluate(() => {
    const c = document.createElement('canvas')
    const real = document.querySelector('canvas.replay-canvas')
    c.width = real.width
    c.height = real.height
    return c.toDataURL()
  })
  const start = await snap()
  assert.notEqual(start, blank, 'the first frame is drawn before playback starts')
  assert.equal(await time(), 0)
  assert.equal(await page.locator('.seek-event').count(), 7, 'session events are ticks on the seek bar')
  assert.equal(await page.locator('.seek-screen').count(), 4, 'screen changes are marked')
  assert.ok((await page.locator('.seek-idle').count()) >= 1, 'idle stretches are marked')
  await shot('20-replay-start')

  // play: frames keep changing
  await page.getByRole('button', { name: 'Oynat' }).click()
  await page.waitForFunction(() => Number(document.querySelector('canvas.replay-canvas').dataset.time) > 1200)
  const playing1 = await snap()
  assert.notEqual(playing1, start, 'the canvas changed after playback started')
  await page.waitForFunction(() => Number(document.querySelector('canvas.replay-canvas').dataset.time) > 3200)
  const playing2 = await snap()
  assert.notEqual(playing2, playing1, 'and keeps changing between later frames')
  await page.getByRole('button', { name: 'Duraklat' }).click()
  await page.getByRole('button', { name: 'Oynat' }).waitFor()
  await page.waitForTimeout(80) // let an already scheduled frame land
  const paused = await time()
  await page.waitForTimeout(300)
  assert.equal(await time(), paused, 'pause stops the clock')

  // seek into the product list and compare with the login screen
  await page.getByLabel('Kayıt konumu').fill('9000')
  assert.equal(await time(), 9000)
  await page.locator('.replay-side').getByText('Ana sayfa').first().waitFor()
  const list = await snap()
  assert.notEqual(list, playing2)
  await shot('21-replay-list')
  // seeking back rebuilds the earlier scene exactly
  await page.getByLabel('Kayıt konumu').fill('0')
  assert.equal(await snap(), start, 'seeking back to 0 reproduces the first frame')

  // event list seeks; 4× with skip-inactivity crosses the 10 s pause quickly
  await page.locator('.replay-event', { hasText: 'product_viewed' }).click()
  assert.equal(await time(), 13700)
  await page.getByRole('button', { name: '4×' }).click()
  const t0 = Date.now()
  await page.getByRole('button', { name: 'Oynat' }).click()
  await page.waitForFunction(() => Number(document.querySelector('canvas.replay-canvas').dataset.time) > 24500)
  const took = Date.now() - t0
  assert.ok(took < 2500, `skip-inactivity should jump the idle stretch (took ${took} ms)`)
  // without skipping, the same stretch takes its full (4×) time
  await page.getByRole('button', { name: 'Duraklat' }).click()
  await page.getByText('Hareketsizliği atla').click()
  await page.locator('.replay-event', { hasText: 'product_viewed' }).click()
  await page.getByRole('button', { name: 'Oynat' }).click()
  await page.waitForTimeout(900)
  const slow = await time()
  // skipping would have jumped to ≥ 23.8 s at once; at 4× the idle stretch takes 2.6 s of real time to cross
  assert.ok(slow > 14000 && slow < 23500, `at 4× without skipping, 0.9 s ≈ 3.6 s of recording (at ${slow})`)
  await page.getByRole('button', { name: 'Duraklat' }).click()
  // runs to the end and stops
  await page.getByLabel('Kayıt konumu').fill('33000')
  await page.getByRole('button', { name: 'Oynat' }).click()
  await page.getByRole('button', { name: 'Oynat' }).waitFor()
  await page.locator('.replay-side').getByText('Sepet').first().waitFor()
  await shot('22-replay-end')
  await noOverflow()
})

await step('replay: zstd chunks play where the browser can decode them; otherwise a clear message', async () => {
  // 1) as served by the query service (Content-Encoding: zstd)
  await go(`/p/${pid}/replay/${REPLAY_SESSION_ZSTD}`)
  const canvas = page.locator('canvas.replay-canvas')
  const note = page.getByText('zstd ile sıkıştırılmış ve bu tarayıcı zstd açamıyor')
  await canvas.or(note).first().waitFor()
  console.log(`      (this Chromium ${(await canvas.count()) ? 'decodes' : 'cannot decode'} Content-Encoding: zstd)`)

  // 2) a browser without zstd: bodies arrive still compressed and DecompressionStream refuses the format
  mock.hooks.rawReplay = true
  const old = await context.newPage()
  await old.addInitScript(() => {
    const Native = window.DecompressionStream
    window.DecompressionStream = class extends Native {
      constructor(format) {
        if (format == 'zstd') throw new TypeError('Unsupported compression format')
        super(format)
      }
    }
  })
  try {
    await old.goto(`${base}/p/${pid}/replay/${REPLAY_SESSION_ZSTD}`)
    const alert = old.getByRole('alert').filter({ hasText: 'zstd' })
    await alert.waitFor()
    assert.match(await alert.innerText(), /5 parçadan 5 tanesi zstd ile sıkıştırılmış ve bu tarayıcı zstd açamıyor/)
    assert.equal(await old.locator('canvas').count(), 0, 'nothing to play')
    if (shotsDir) await old.screenshot({ path: path.join(shotsDir, '23-replay-zstd-light.png') })
    // the gzip session still plays there, through the UI's own gunzip (no Content-Encoding)
    await old.goto(`${base}/p/${pid}/replay/${REPLAY_SESSION}`)
    await old.locator('canvas.replay-canvas').waitFor()
  } finally {
    mock.hooks.rawReplay = false
    await old.close()
  }

  // a session without any chunks
  await go(`/p/${pid}/replay/${'0'.repeat(32)}`)
  await page.getByText('Bu oturum için kayıt bulunamadı').waitFor()
})

await step('deletion request: confirm dialog, then listed as pending', async () => {
  await go(`/p/${pid}/persons/u%3A1001`)
  await page.getByRole('link', { name: 'Silme isteği oluştur' }).click()
  await heading('Silme istekleri').waitFor()
  assert.equal(await page.getByLabel('Kullanıcı kimlikleri').inputValue(), '1001', 'prefilled from the person page')
  await page.getByLabel('Kullanıcı kimlikleri').fill('1001\n1002\n1001')
  await page.getByLabel('Anonim kimlikler').fill('a-b-c')
  await page.getByLabel('Not').fill('Destek talebi #77')
  await page.getByRole('button', { name: 'Silme isteği oluştur…' }).click()
  await page.getByText('2 kullanıcı ve 1 anonim kimliğe ait tüm veriler silinecek.').waitFor()
  await shot('24-deletion-confirm')
  assert.equal(mock.db.deletions.length, 0, 'nothing is sent before confirming')
  await page.getByRole('button', { name: 'Evet, kalıcı olarak sil' }).click()
  const row = page.locator('tbody tr', { hasText: 'del_' })
  await row.waitFor()
  assert.match(await row.innerText(), /Bekliyor[\s\S]*1001, 1002[\s\S]*a-b-c[\s\S]*Destek talebi #77/)
  assert.deepEqual([mock.db.deletions[0].user_ids, mock.db.deletions[0].anon_ids, mock.db.deletions[0].status], [['1001', '1002'], ['a-b-c'], 'pending'])
  await shot('25-deletions')
})

await step('members: add a viewer; audit log lists what happened', async () => {
  await page.getByRole('link', { name: 'Üyeler' }).click()
  await heading('Üyeler').waitFor()
  await page.getByRole('button', { name: 'Üye ekle' }).click()
  await page.getByLabel('E-posta').fill(VIEWER.email)
  await page.getByLabel('Rol', { exact: true }).selectOption('viewer')
  await page.getByLabel('İlk parola').fill(VIEWER.password)
  await page.getByRole('button', { name: 'Ekle', exact: true }).click()
  const row = page.locator('tbody tr', { hasText: VIEWER.email })
  await row.waitFor()
  assert.equal(await row.getByRole('combobox').inputValue(), 'viewer')
  // the last owner cannot be demoted: the API's message is shown and the role stays
  await page.locator('tbody tr', { hasText: OWNER.email }).getByRole('combobox').selectOption('admin')
  await page.getByText('Son sahibin rolü düşürülemez').waitFor()
  await page.waitForFunction((email) => [...document.querySelectorAll('tbody tr')].find((r) => r.textContent.includes(email))?.querySelector('select')?.value == 'owner', OWNER.email)
  await shot('26-members')
  await toastGone()
  await page.getByRole('link', { name: 'Denetim kaydı' }).click()
  await heading('Denetim kaydı').waitFor()
  await page.locator('tbody tr').first().waitFor()
  const actions = await page.locator('tbody tr td:nth-child(3)').allInnerTexts()
  for (const a of ['member.add', 'deletion.create', 'flag.update', 'flag.create', 'dashboard.update', 'insight.create', 'key.create', 'token.create', 'project.create'])
    assert.ok(actions.includes(a), `audit log is missing ${a}`)
  assert.ok(actions.indexOf('member.add') < actions.indexOf('project.create'), 'newest first')
  await shot('27-audit')
  // fewer than one page so far: no "older" button; the paging contract itself is checked against the API
  assert.ok(actions.length < 50)
  assert.equal(await page.getByRole('button', { name: 'Daha eski kayıtlar' }).count(), 0)
  const orgId = mock.db.orgs[0].id
  const firstPage = await (await page.request.get(`${base}/api/orgs/${orgId}/audit?limit=5`)).json()
  const nextPage = await (await page.request.get(`${base}/api/orgs/${orgId}/audit?limit=5&before=${firstPage[4].id}`)).json()
  assert.equal(firstPage.length, 5)
  assert.ok(nextPage[0].id < firstPage[4].id && nextPage[0].ts <= firstPage[4].ts, 'before= continues where the page ended')
})

await step('account: change password; enable TOTP; login then asks for the code', async () => {
  await page.locator('a.user').click()
  await heading('Hesap').waitFor()
  await page.getByLabel('Mevcut parola').fill('yanlis-parola-00')
  await page.getByLabel('Yeni parola', { exact: true }).fill('yepyeni-parola-22')
  await page.getByLabel('Yeni parola (tekrar)').fill('yepyeni-parola-23')
  await page.getByText('Parolalar aynı değil.').waitFor()
  await page.getByLabel('Yeni parola (tekrar)').fill('yepyeni-parola-22')
  await page.getByRole('button', { name: 'Parolayı değiştir' }).click()
  await page.getByRole('alert').filter({ hasText: 'Mevcut parola hatalı' }).waitFor()
  assert.ok(await page.locator('.shell').count(), 'a wrong current password does not log the user out')
  await page.getByLabel('Mevcut parola').fill(OWNER.password)
  await page.getByRole('button', { name: 'Parolayı değiştir' }).click()
  await page.getByText('Parola değiştirildi.').waitFor()
  OWNER.password = 'yepyeni-parola-22'

  await page.getByRole('button', { name: 'Kurulumu başlat' }).click()
  const secret = await page.getByLabel('Gizli anahtar').inputValue()
  assert.match(secret, /^[A-Z2-7]{32}$/)
  assert.match(await page.getByLabel('otpauth bağlantısı').inputValue(), /^otpauth:\/\/totp\//)
  await page.getByLabel('Doğrulama kodu').fill('000000')
  await page.getByRole('button', { name: 'Etkinleştir' }).click()
  await page.getByRole('alert').filter({ hasText: 'Doğrulama kodu hatalı' }).waitFor()
  await page.getByLabel('Doğrulama kodu').fill(totp(secret))
  await page.getByRole('button', { name: 'Etkinleştir' }).click()
  await page.getByText('İki adımlı doğrulama açıldı.').waitFor()
  await shot('28-account')
  await toastGone()

  await logout()
  await login(OWNER)
  await page.getByLabel('Doğrulama kodu').waitFor()
  await shot('29-login-totp')
  await page.getByLabel('Doğrulama kodu').fill(totp(secret))
  await page.getByRole('button', { name: 'Doğrula' }).click()
  await page.locator('.shell').waitFor()
  // turn it off again so the rest of the run logs in with the password only
  await go('/account')
  await page.getByLabel('Parola', { exact: true }).fill(OWNER.password)
  await page.getByRole('button', { name: 'İki adımlı doğrulamayı kapat' }).click()
  await page.getByText('İki adımlı doğrulama kapatıldı.').waitFor()
  await toastGone()
})

/** The language step itself; the step wrapper below restores Turkish whatever happens in here. */
const languageScenario = async (lang) => {
  await go('/account')
  await heading('Hesap').waitFor()
  assert.deepEqual(await lang(), ['tr', null], 'Turkish came from the browser language (tr-TR); nothing is stored until the user chooses')
  const picker = page.getByLabel('Dil', { exact: true })
  assert.equal(await picker.inputValue(), 'tr')
  assert.deepEqual(await picker.locator('option').allInnerTexts(), ['Türkçe', 'English'], 'languages are listed by their own names')

  // the switch re-renders in place: no navigation, no reload
  await page.evaluate(() => (window.__sameDocument = true))
  await picker.selectOption('en')
  await heading('Account').waitFor()
  assert.equal(await page.evaluate(() => window.__sameDocument), true, 'the page was not reloaded')
  assert.deepEqual(await lang(), ['en', 'en'])
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'en')
  for (const name of ['Preferences', 'Change password', 'Two-step verification (TOTP)']) await page.getByRole('heading', { name, exact: true }).waitFor()
  await page.getByRole('button', { name: 'Change password' }).waitFor()
  const nav = await page.locator('.nav').innerText()
  for (const item of ['Dashboards', 'Insights', 'Event stream', 'People', 'Feature flags', 'Deletion requests', 'Project settings', 'Projects', 'Members', 'Audit log'])
    assert.ok(nav.split('\n').includes(item), `menu item "${item}" in: ${nav.replace(/\n/g, ' | ')}`)
  assert.equal(await page.getByRole('navigation', { name: 'Main navigation' }).count(), 1)
  assert.match(await page.locator('a.user').innerText(), /Owner/)
  await page.getByRole('button', { name: 'Sign out' }).waitFor()
  const turkish = await page.getByText(/[çğışöüÇĞİŞÖÜ]/).locator('visible=true').allInnerTexts()
  assert.deepEqual(turkish, ['İzlek', 'Örnek A.Ş.'], 'only the brand and the organization’s own name: no Turkish UI text is left on the account page')
  await shot('29b-account-en')

  // a full page load starts in the stored language; numbers and percentages follow it too
  await go(`/p/${pid}/insights/${insightIds.funnel}`)
  await page.locator('.funnel-step').nth(2).waitFor()
  assert.deepEqual(await lang(), ['en', 'en'])
  assert.match(await page.locator('.headline').innerText(), /^\d+(\.\d)?% overall conversion · [\d,]+ of [\d,]+ people reached the last step$/)
  const steps = await page.locator('.funnel-step').allInnerTexts()
  assert.match(steps[0], /signup[\s\S]*100%[\s\S]*People who entered the funnel/)
  assert.match(steps[1], /From previous step: \d+(\.\d)?%[\s\S]*Dropped off: [\d,]+[\s\S]*Median time: \d/)
  for (const name of ['Run', 'Save', 'Save as copy', 'Delete']) assert.equal(await page.getByRole('button', { name, exact: true }).count(), 1, `button "${name}"`)
  await page.getByRole('link', { name: 'Insights' }).first().waitFor()
  await shot('29c-funnel-en')
  await page.reload()
  await page.locator('.funnel-step').nth(2).waitFor()
  await page.getByRole('button', { name: 'Save as copy' }).waitFor()
  assert.deepEqual(await lang(), ['en', 'en'], 'the choice survives a reload')

  await page.getByRole('link', { name: 'Feature flags' }).click()
  await heading('Feature flags').waitFor()
  assert.match(await page.locator('tr', { hasText: 'new_checkout' }).innerText(), /platform = ios · 25% → control 60%, test 40%/)
  await page.getByRole('link', { name: 'New flag' }).waitFor()
  await page.getByRole('link', { name: 'Project settings' }).click()
  await heading('Project settings').waitFor()
  assert.deepEqual(await page.getByRole('tab').allInnerTexts(), ['General', 'Write tokens', 'Secret keys', 'SDK setup', 'AI assistants'])
  await page.getByRole('tab', { name: 'SDK setup' }).click()
  await page.getByRole('tab', { name: 'Android' }).click()
  assert.match(await page.getByTestId('snippet').innerText(), /analytics-replay:0\.1\.0"\)\s+\/\/ optional/)

  // the login form offers the switch too; there it is operated with the keyboard only
  await page.getByRole('button', { name: 'Sign out' }).click()
  await page.getByRole('heading', { name: 'Sign in' }).waitFor()
  for (const label of ['Email', 'Password']) await page.getByLabel(label, { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Sign in' }).waitFor()
  await shot('29d-login-en')
  const loginPicker = page.getByLabel('Language', { exact: true })
  assert.equal(await loginPicker.inputValue(), 'en')
  await loginPicker.focus()
  await page.keyboard.press('ArrowUp') // Türkçe is listed before English
  await page.getByRole('heading', { name: 'Oturum aç' }).waitFor()
  assert.deepEqual(await lang(), ['tr', 'tr'])
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute('data-testid')), 'lang-switch', 'focus stays on the switch after the screen is rebuilt')
  assert.equal(await page.getByLabel('Dil', { exact: true }).inputValue(), 'tr')
  await page.reload()
  await page.getByRole('heading', { name: 'Oturum aç' }).waitFor()
  assert.deepEqual(await lang(), ['tr', 'tr'])

  // without a stored choice the browser language decides: Turkish only for Turkish browsers
  for (const [locale, title, picked] of [['en-US', 'Sign in', 'en'], ['de-DE', 'Sign in', 'en'], ['tr-TR', 'Oturum aç', 'tr']]) {
    const other = await browser.newContext({ locale })
    try {
      const p = await other.newPage()
      await p.goto(base + '/')
      await p.getByRole('heading', { name: title }).waitFor()
      assert.deepEqual(await p.evaluate(() => [document.documentElement.lang, localStorage.getItem('la_locale')]), [picked, null], `browser language ${locale}`)
      if (locale != 'en-US') continue
      // …and an explicit choice beats it from then on
      await p.getByLabel('Language', { exact: true }).selectOption('tr')
      await p.getByRole('heading', { name: 'Oturum aç' }).waitFor()
      await p.reload()
      await p.getByRole('heading', { name: 'Oturum aç' }).waitFor()
      assert.equal(await p.getByLabel('Dil', { exact: true }).inputValue(), 'tr')
    } finally {
      await other.close()
    }
  }

  // back in Turkish for the rest of the run
  await login(OWNER)
  await page.locator('.shell').waitFor()
  await page.getByRole('button', { name: 'Çıkış yap' }).waitFor()
}

await step('language: switch to English on the account page, it persists across reloads, switch back on the login form', async () => {
  const lang = () => page.evaluate(() => [document.documentElement.lang, localStorage.getItem('la_locale')])
  try {
    await languageScenario(lang)
  } finally {
    // the later steps assert Turkish text: do not let a failure in here leave the browser in English
    await page.evaluate(() => localStorage.setItem('la_locale', 'tr')).catch(() => {})
  }
})

await step('AI assistants: MCP address with copy, connect steps per client; notes when OAuth or the endpoint is off', async () => {
  const endpoint = base + '/mcp'
  const status = (state) => page.locator(`[data-testid="mcp-status"][data-state="${state}"]`)
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText())
  try {
    await go(`/p/${pid}/settings`)
    await page.getByRole('tab', { name: 'Yapay zekâ asistanları' }).click()
    await page.waitForURL(/\/settings\/mcp$/)
    await page.getByRole('heading', { name: 'Yapay zekâ asistanları (MCP)' }).waitFor()

    // the address, and the copy button next to it
    assert.equal(await page.getByTestId('mcp-url').inputValue(), endpoint)
    assert.equal(await page.getByLabel('MCP adresi').inputValue(), endpoint)
    await page.getByTestId('mcp-url-copy').click()
    assert.equal(await clipboard(), endpoint)

    // OAuth is on: both Claude Code forms, exactly as docs/mcp.md gives them
    await status('oauth').waitFor()
    assert.match(await status('oauth').innerText(), /OAuth ile oturum açma kullanılabilir/)
    const code = page.getByTestId('mcp-client-code')
    const keyCommand = `claude mcp add --transport http izlek ${endpoint} \\\n  --header "Authorization: Bearer sk_…"`
    assert.equal(await page.getByTestId('mcp-cmd-key').innerText(), keyCommand)
    assert.equal(await page.getByTestId('mcp-cmd-oauth').innerText(), `claude mcp add --transport http izlek ${endpoint}`)
    // the account-wide access token comes first: all projects, with a link to where tokens are made
    const patCommand = `claude mcp add --transport http izlek ${endpoint} --header "Authorization: Bearer pat_…"`
    assert.equal(await page.getByTestId('mcp-cmd-pat').innerText(), patCommand)
    assert.deepEqual(await code.getByRole('heading', { level: 3 }).allInnerTexts(), ['Kişisel erişim token’ıyla (tüm projeleriniz)', 'Proje gizli anahtarıyla (yalnızca bu proje)', 'OAuth ile'])
    await code.getByRole('button', { name: 'Kopyala' }).first().click()
    assert.equal(await clipboard(), patCommand)
    assert.equal(await code.getByRole('link', { name: 'Hesap → Erişim token’ları' }).getAttribute('href'), '/account')
    assert.ok(!/pat_[0-9A-Za-z]{4,}/.test(await page.content()), 'no real access token on the page')
    // the project has a secret key by now; only the placeholder is ever rendered
    assert.ok(mock.db.keys.some((k) => k.project_id == pid))
    assert.ok(!/sk_[0-9A-Za-z]{4,}/.test(await page.content()), 'no real secret key on the page')
    assert.equal(await code.getByRole('link', { name: 'Gizli anahtar oluştur' }).getAttribute('href'), `/p/${pid}/settings/keys`)
    await shot('29e-mcp')

    // claude.ai / Claude Desktop and ChatGPT: numbered steps with the address, and the "not verified" caveat
    await page.getByRole('tab', { name: 'claude.ai / Claude Desktop' }).click()
    const claude = page.getByTestId('mcp-client-claude')
    assert.equal(await claude.locator('ol.steps > li').count(), 5)
    assert.match(await claude.innerText(), /Add custom connector/)
    assert.ok((await claude.innerText()).includes(endpoint))
    assert.match(await claude.innerText(), /Doğrulanmadı: bu adımlar Anthropic belgelerinin 2026-11-19/)
    assert.equal(await claude.getByRole('note').count(), 0, 'no warning while OAuth is on')
    await page.getByRole('tab', { name: 'ChatGPT' }).click()
    const chatgpt = page.getByTestId('mcp-client-chatgpt')
    assert.equal(await chatgpt.locator('ol.steps > li').count(), 4)
    assert.match(await chatgpt.innerText(), /Add custom MCP server/)
    assert.ok((await chatgpt.innerText()).includes(endpoint))
    assert.match(await chatgpt.innerText(), /Doğrulanmadı: bu adımlar OpenAI belgelerinin 2026-11-19/)
    await shot('29f-mcp-chatgpt')

    // what an assistant can and cannot do; where to revoke
    const tools = page.getByTestId('mcp-tools')
    assert.equal(await tools.locator('.tool').count(), 22)
    assert.equal(await tools.locator('.tool-write').count(), 7)
    assert.deepEqual(await tools.getByRole('heading', { level: 3 }).allInnerTexts(), ['Kurulum', 'Flag’ler', 'Analiz', 'Neleri yapamaz?'])
    for (const text of ['Proje kapatmak', 'Üyeleri yönetmek', 'Silme isteği oluşturmak (KVKK/GDPR)']) await tools.getByText(text, { exact: true }).waitFor()
    await page.getByRole('heading', { name: 'Güvenlik' }).waitFor()
    assert.equal(await page.getByRole('link', { name: 'Hesap → Bağlı uygulamalar' }).getAttribute('href'), '/account')
    await noOverflow()

    // without LA_PUBLIC_URL the metadata document is a 404: secret keys only, and the page says so
    mock.hooks.oauth = false
    await page.reload()
    await status('keys').waitFor()
    const note = await status('keys').innerText()
    assert.match(note, /yalnızca erişim token’ları ve gizli anahtarlar çalışır/)
    assert.match(note, /LA_PUBLIC_URL ayarı \(https adresi\)/)
    assert.match(note, /claude\.ai ve ChatGPT bağlanamaz; Claude Code erişim token’ı ya da gizli anahtarla bağlanabilir/)
    assert.equal(await page.getByTestId('mcp-url').inputValue(), endpoint)
    assert.equal(await page.getByTestId('mcp-cmd-key').innerText(), keyCommand, 'the secret-key form still applies')
    assert.equal(await page.getByTestId('mcp-cmd-pat').count(), 1, 'and so does the access-token form')
    assert.equal(await page.getByTestId('mcp-cmd-oauth').count(), 0, 'the OAuth form is not offered')
    await page.getByTestId('mcp-code-oauth-off').waitFor()
    await shot('29g-mcp-oauth-off')
    for (const [tab, id] of [['claude.ai / Claude Desktop', 'mcp-client-claude'], ['ChatGPT', 'mcp-client-chatgpt']]) {
      await page.getByRole('tab', { name: tab }).click()
      assert.match(await page.getByTestId(id).getByRole('note').innerText(), /yalnızca OAuth ile bağlanır/)
    }

    // LA_MCP_ENABLED=false: the path falls through to the SPA shell (200, HTML), which must not read as "OAuth is on"
    mock.hooks.mcp = false
    await page.reload()
    await status('off').waitFor()
    assert.match(await status('off').innerText(), /LA_MCP_ENABLED=false/)
    mock.hooks.mcp = true
    mock.hooks.oauth = true

    // the same page in English
    await page.evaluate(() => localStorage.setItem('la_locale', 'en'))
    await page.reload()
    await page.getByRole('heading', { name: 'AI assistants (MCP)' }).waitFor()
    assert.equal(await page.getByRole('tab', { name: 'AI assistants' }).getAttribute('aria-selected'), 'true')
    await status('oauth').waitFor()
    assert.equal(await page.getByLabel('MCP endpoint').inputValue(), endpoint)
    assert.deepEqual(await page.getByTestId('mcp-tools').getByRole('heading', { level: 3 }).allInnerTexts(), ['Setup', 'Feature flags', 'Analytics', 'What it cannot do'])
    await page.getByRole('link', { name: 'Create a secret key' }).waitFor()
    await page.getByRole('link', { name: 'Account → Connected apps' }).waitFor()
    await shot('29h-mcp-en')
  } finally {
    mock.hooks.mcp = true
    mock.hooks.oauth = true
    await page.evaluate(() => localStorage.setItem('la_locale', 'tr')).catch(() => {})
  }
})

await step('connected apps: listed with their access in plain words, revoked through the confirm dialog; quiet when the server has none', async () => {
  const owner = mock.db.users.find((u) => u.email == OWNER.email)
  const other = mock.db.users.find((u) => u.email == VIEWER.email)
  const [claude, chatgpt] = mock.connectDemoApps(owner.id)
  const foreign = mock.connectApp(other.id, { client_id: 'oc_' + 'x'.repeat(24), client_name: 'Başkasının uygulaması', client_kind: 'dcr', redirect_hosts: ['localhost'] })
  const mine = () => mock.db.grants.filter((g) => g.user_id == owner.id).map((g) => g.id)
  try {
    await go('/account')
    await page.getByRole('heading', { name: 'Bağlı uygulamalar' }).waitFor()
    const list = page.getByTestId('connected-apps')
    const rows = list.locator('tbody tr')
    await rows.nth(1).waitFor()
    assert.equal(await rows.count(), 2, 'only this user’s applications')
    assert.equal(await list.getAttribute('data-count'), '2')
    const claudeRow = list.locator(`tr[data-app-id="${claude.id}"]`)
    const chatgptRow = list.locator(`tr[data-app-id="${chatgpt.id}"]`)
    assert.match(
      await claudeRow.innerText(),
      /Claude[\s\S]*Ad doğrulanmadı[\s\S]*claude\.ai[\s\S]*Okuma ve değişiklik[\s\S]*Projeleri görebilir ve sorgu çalıştırabilir\.[\s\S]*Ayrıca proje, token, anahtar ve flag oluşturabilir ve değiştirebilir\.[\s\S]*2 sa önce/,
    )
    const readOnly = await chatgptRow.innerText()
    assert.match(readOnly, /ChatGPT[\s\S]*Bilgileri chatgpt\.com adresinden okundu[\s\S]*chatgpt\.com[\s\S]*Salt okunur[\s\S]*Projeleri görebilir ve sorgu çalıştırabilir\.[\s\S]*Hiç kullanılmadı/)
    assert.ok(!readOnly.includes('Ayrıca proje'), 'a read-only grant does not claim change access')
    await list.getByText('Parolanızı değiştirmek ya da iki adımlı doğrulamayı açmak, bağlı uygulamaların tümünün erişimini iptal eder.').waitFor()
    await shot('29i-connected-apps')
    await noOverflow()

    // revoke asks first; cancelling changes nothing
    await chatgptRow.getByRole('button', { name: 'Erişimi iptal et' }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByRole('heading', { name: 'Erişim iptal edilsin mi?' }).waitFor()
    await dialog.getByText('“ChatGPT” erişimini hemen kaybeder.').waitFor()
    await dialog.getByRole('button', { name: 'Vazgeç' }).click()
    assert.equal(await page.getByRole('dialog').count(), 0)
    assert.deepEqual(mine(), [claude.id, chatgpt.id])

    // confirming sends the DELETE with the CSRF header; the row disappears
    await chatgptRow.getByRole('button', { name: 'Erişimi iptal et' }).click()
    const deleted = page.waitForResponse((r) => r.request().method() == 'DELETE' && new URL(r.url()).pathname == `/api/me/connected-apps/${chatgpt.id}`)
    await page.getByRole('dialog').getByRole('button', { name: 'Erişimi iptal et' }).click()
    const response = await deleted
    assert.equal(response.status(), 204)
    assert.ok(response.request().headers()['x-la-csrf'], 'revoking carries the CSRF header')
    await page.getByText('ChatGPT erişimi iptal edildi.').waitFor()
    await chatgptRow.waitFor({ state: 'detached' })
    assert.equal(await rows.count(), 1)
    await claudeRow.waitFor()
    assert.deepEqual(mine(), [claude.id])
    assert.ok(mock.db.grants.some((g) => g.id == foreign.id), 'other users’ grants are untouched')
    assert.ok(mock.db.audit.some((a) => a.action == 'oauth.revoke' && a.target == 'ChatGPT'))
    await toastGone()

    // nothing left: the empty state explains the list and points to the AI assistants page
    await claudeRow.getByRole('button', { name: 'Erişimi iptal et' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Erişimi iptal et' }).click()
    await list.getByText('Bağlı uygulama yok').waitFor()
    assert.deepEqual(mine(), [])
    await list.getByText('OAuth ile bağladığınız uygulamalar burada listelenir').waitFor()
    await toastGone()
    await list.getByRole('link', { name: 'Yapay zekâ asistanı nasıl bağlanır?' }).click()
    await page.waitForURL(new RegExp(`/p/${pid}/settings/mcp$`))
    await page.getByTestId('mcp-url').waitFor()

    // a server without the endpoint answers 404: one quiet line, no error banner
    mock.hooks.mcp = false
    await go('/account')
    await page.getByTestId('connected-apps-off').waitFor()
    assert.equal(await page.getByTestId('connected-apps-off').innerText(), 'Bu sunucuda uygulama bağlama etkin değil.')
    assert.equal(await page.getByRole('alert').count(), 0)
    assert.equal(await page.getByTestId('connected-apps').locator('table').count(), 0)
  } finally {
    mock.hooks.mcp = true
    mock.db.grants = mock.db.grants.filter((g) => g.id != foreign.id && g.user_id != owner.id)
  }
})

await step('access tokens: create (read-only, 30 days), shown once with copy, listed, limit of 25, revoked through the confirm dialog; hidden on an older server', async () => {
  const owner = mock.db.users.find((u) => u.email == OWNER.email)
  const mine = () => mock.db.pats.filter((t) => t.user_id == owner.id)
  try {
    await go('/account')
    await page.getByRole('heading', { name: 'Erişim token’ları' }).waitFor()
    const list = page.getByTestId('access-tokens')
    await list.getByText('Erişim token’ı yok').waitFor()
    await list.getByText('Parolanızı değiştirmek ya da iki adımlı doğrulamayı açmak tüm erişim token’larını iptal eder.').waitFor()

    // the form: read-only and 90 days are the defaults
    await page.getByRole('button', { name: 'Yeni erişim token’ı' }).click()
    const dialog = page.getByRole('dialog')
    assert.ok(await dialog.getByRole('radio', { name: /^Salt okunur/ }).isChecked())
    assert.equal(await dialog.getByLabel('Geçerlilik süresi').inputValue(), '90')
    assert.deepEqual(await dialog.getByLabel('Geçerlilik süresi').locator('option').allInnerTexts(), ['30 gün', '90 gün', '365 gün', 'Süresiz'])
    assert.ok(await dialog.getByRole('button', { name: 'Oluştur' }).isDisabled(), 'a name is required')
    await dialog.getByLabel('Ad', { exact: true }).fill('Claude Code – dizüstü')
    await dialog.getByLabel('Geçerlilik süresi').selectOption('30')
    const posted = page.waitForResponse((r) => r.request().method() == 'POST' && new URL(r.url()).pathname == '/api/me/tokens')
    await dialog.getByRole('button', { name: 'Oluştur' }).click()
    const response = await posted
    assert.equal(response.status(), 201)
    assert.deepEqual(response.request().postDataJSON(), { name: 'Claude Code – dizüstü', scopes: ['izlek:read'], expires_in_days: 30 })
    assert.ok(response.request().headers()['x-la-csrf'])

    // shown once, with copy
    await dialog.getByRole('heading', { name: 'Erişim token’ı oluşturuldu' }).waitFor()
    const token = (await page.getByTestId('pat-secret').innerText()).trim()
    assert.match(token, /^pat_[0-9A-Za-z]{40}$/)
    await dialog.getByText('Bu token yalnızca şimdi gösterilir; bir daha göremezsiniz.').waitFor()
    await page.getByTestId('pat-copy').click()
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), token)
    await shot('29j-access-token-once')
    await dialog.getByRole('button', { name: 'Kopyaladım, kapat' }).click()
    assert.equal(await page.getByRole('dialog').count(), 0)
    assert.equal(await page.content().then((html) => html.includes(token)), false, 'the token is gone from the page')

    // listed with its access in plain words
    const created = mine()[0]
    const row = list.locator(`tr[data-token-id="${created.id}"]`)
    await row.waitFor()
    assert.match(await row.innerText(), /Claude Code – dizüstü[\s\S]*pat_…[0-9A-Za-z]{4}[\s\S]*Salt okunur[\s\S]*Hiç kullanılmadı/)
    assert.ok((await row.innerText()).includes('pat_…' + token.slice(-4)))
    assert.deepEqual([created.scopes, Math.round((created.expires_at - created.created_at) / 86_400_000)], [['izlek:read'], 30])
    assert.equal(await list.getAttribute('data-count'), '1')
    await page.reload()
    await list.locator('tbody tr').first().waitFor()
    assert.equal(await page.content().then((html) => html.includes(token)), false, 'and never comes back')

    // a second one with change access and no expiry; newest first
    await page.getByRole('button', { name: 'Yeni erişim token’ı' }).click()
    await page.getByRole('dialog').getByLabel('Ad', { exact: true }).fill('Betik')
    await page.getByRole('dialog').getByRole('radio', { name: /^Okuma ve değişiklik/ }).check()
    await page.getByRole('dialog').getByLabel('Geçerlilik süresi').selectOption('never')
    await page.getByRole('dialog').getByRole('button', { name: 'Oluştur' }).click()
    await page.getByTestId('pat-secret').waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Kopyaladım, kapat' }).click()
    await list.locator('tbody tr').nth(1).waitFor()
    const rows = await list.locator('tbody tr').allInnerTexts()
    assert.match(rows[0], /Betik[\s\S]*Okuma ve değişiklik[\s\S]*Süresiz/)
    assert.match(rows[1], /Claude Code – dizüstü/)
    assert.deepEqual(mine().find((t) => t.name == 'Betik').scopes, ['izlek:read', 'izlek:admin'])
    await shot('29k-access-tokens')
    await noOverflow()

    // the API: bad input is refused, other users' tokens do not exist, 25 is the limit
    const api = await page.evaluate(async (other) => {
      const me = await (await fetch('/api/me')).json()
      const call = async (method, url, body, csrf = me.csrf) => {
        const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json', ...(csrf ? { 'X-LA-CSRF': csrf } : {}) }, body: body && JSON.stringify(body) })
        return [r.status, r.status == 204 ? null : (await r.json()).error?.code ?? null]
      }
      const ok = { name: 'x', scopes: ['izlek:read'], expires_in_days: 90 }
      const out = {
        noCsrf: await call('POST', '/api/me/tokens', ok, null),
        noName: await call('POST', '/api/me/tokens', { ...ok, name: '' }),
        longName: await call('POST', '/api/me/tokens', { ...ok, name: 'n'.repeat(81) }),
        adminOnly: await call('POST', '/api/me/tokens', { ...ok, scopes: ['izlek:admin'] }),
        badExpiry: await call('POST', '/api/me/tokens', { ...ok, expires_in_days: 7 }),
        foreign: await call('DELETE', `/api/me/tokens/${other}`),
        filled: [],
      }
      for (let i = 0; i < 23; i++) out.filled.push((await call('POST', '/api/me/tokens', { ...ok, name: 'dolgu ' + i }))[0])
      out.tooMany = await call('POST', '/api/me/tokens', ok)
      return out
    }, (mock.db.pats.push({ user_id: -1, id: 'foreign', seq: 0, name: 'başkası', prefix: 'pat_…zzzz', scopes: ['izlek:read'], created_at: 0, last_used_at: null, expires_at: null, hash: '' }), 'foreign'))
    assert.deepEqual(
      { ...api, filled: [...new Set(api.filled)] },
      { noCsrf: [403, 'csrf'], noName: [400, 'bad_request'], longName: [400, 'bad_request'], adminOnly: [400, 'bad_request'], badExpiry: [400, 'bad_request'], foreign: [404, 'not_found'], filled: [201], tooMany: [400, 'too_many_tokens'] },
    )
    assert.equal(mine().length, 25)
    assert.ok(mock.db.pats.some((t) => t.id == 'foreign'))
    // the UI says it in the user's language instead of echoing the server
    await page.reload()
    await list.locator('tbody tr').nth(24).waitFor()
    assert.equal(await list.locator('tbody tr').count(), 25, 'only this user’s tokens')
    await page.getByRole('button', { name: 'Yeni erişim token’ı' }).click()
    await page.getByRole('dialog').getByLabel('Ad', { exact: true }).fill('Fazla')
    await page.getByRole('dialog').getByRole('button', { name: 'Oluştur' }).click()
    await page.getByRole('dialog').getByRole('alert').filter({ hasText: 'En çok 25 erişim token’ınız olabilir.' }).waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Vazgeç' }).click()
    mock.db.pats = mock.db.pats.filter((t) => !t.name.startsWith('dolgu') && t.name != 'x')
    await page.reload()
    await list.locator('tbody tr').nth(1).waitFor()

    // revoke asks first; cancelling changes nothing; confirming removes the row
    const revoke = row.getByRole('button', { name: 'İptal et' })
    await revoke.click()
    await page.getByRole('dialog').getByRole('heading', { name: 'Erişim token’ı iptal edilsin mi?' }).waitFor()
    await page.getByRole('dialog').getByText('“Claude Code – dizüstü” hemen geçersiz olur.').waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Vazgeç' }).click()
    assert.equal(mine().length, 2)
    await revoke.click()
    const deleted = page.waitForResponse((r) => r.request().method() == 'DELETE' && new URL(r.url()).pathname == `/api/me/tokens/${created.id}`)
    await page.getByRole('dialog').getByRole('button', { name: 'İptal et' }).click()
    const gone = await deleted
    assert.equal(gone.status(), 204)
    assert.ok(gone.request().headers()['x-la-csrf'])
    await page.getByText('Claude Code – dizüstü iptal edildi.').waitFor()
    await row.waitFor({ state: 'detached' })
    assert.deepEqual(mine().map((t) => t.name), ['Betik'])
    await toastGone()

    // English headings and access wording
    await page.evaluate(() => localStorage.setItem('la_locale', 'en'))
    await page.reload()
    await page.getByRole('heading', { name: 'Access tokens' }).waitFor()
    assert.match(await page.getByTestId('access-tokens').locator('tbody tr').first().innerText(), /Betik[\s\S]*Read and change[\s\S]*Never used[\s\S]*Never/)
    await page.getByRole('button', { name: 'New access token' }).click()
    assert.deepEqual(await page.getByRole('dialog').getByLabel('Expires after').locator('option').allInnerTexts(), ['30 days', '90 days', '365 days', 'Never'])
    await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click()
    await page.evaluate(() => localStorage.setItem('la_locale', 'tr'))

    // a server that predates access tokens answers 404: no card, no error
    mock.hooks.pats = false
    await go('/account')
    await page.getByTestId('connected-apps').waitFor()
    await page.getByRole('heading', { name: 'Bağlı uygulamalar' }).waitFor()
    assert.equal(await page.getByTestId('access-tokens').count(), 0)
    assert.equal(await page.getByRole('heading', { name: 'Erişim token’ları' }).count(), 0)
    assert.equal(await page.getByRole('alert').count(), 0)
  } finally {
    mock.hooks.pats = true
    mock.db.pats = []
    await page.evaluate(() => localStorage.setItem('la_locale', 'tr')).catch(() => {})
  }
})

await step('an expired session sends the user to the login form and back to the same page', async () => {
  await go(`/p/${pid}/flags`)
  await page.locator('tr', { hasText: 'new_checkout' }).waitFor()
  mock.db.sessions.clear()
  await page.getByRole('link', { name: 'Insight’lar' }).click()
  await page.getByRole('heading', { name: 'Oturum aç' }).waitFor()
  await login(OWNER)
  await heading('Insight’lar').waitFor()
  assert.equal(new URL(page.url()).pathname, `/p/${pid}/insights`)
})

await step('viewer: reads everything, sees no admin or write actions, and the API agrees', async () => {
  await logout()
  await login(VIEWER)
  await page.locator('.shell').waitFor()
  await go(`/p/${pid}/dashboards`)
  const nav = await page.locator('.nav').innerText()
  assert.ok(!nav.includes('Silme istekleri'), 'no deletion requests in the menu')
  assert.ok(!nav.includes('Denetim kaydı'), 'no audit log in the menu')
  assert.match(await page.locator('a.user').innerText(), /İzleyici/)
  assert.equal(await page.getByRole('button', { name: 'Yeni dashboard' }).count(), 0)
  await page.getByRole('link', { name: 'Genel bakış' }).click()
  await page.locator('.tile .funnel').waitFor()
  assert.equal(await page.getByRole('button', { name: 'Düzenle' }).count(), 0, 'dashboards are read-only')

  await go(`/p/${pid}/insights`)
  await page.locator('tbody tr').first().waitFor()
  assert.equal(await page.getByRole('link', { name: '+ Trend' }).count(), 0)
  await page.getByRole('link', { name: 'Satın alma ve aktif kişi' }).click()
  await page.locator('.chart svg').waitFor() // queries still run for viewers
  assert.equal(await page.getByRole('button', { name: 'Kaydet', exact: true }).count(), 0)
  assert.equal(await page.getByRole('button', { name: 'Sil', exact: true }).count(), 0)
  await page.getByText('Salt okunur: kaydetmek için üye rolü gerekir').waitFor()

  await go(`/p/${pid}/flags`)
  const row = page.locator('tr', { hasText: 'new_checkout' })
  await row.waitFor()
  assert.equal(await page.getByRole('link', { name: 'Yeni flag' }).count(), 0)
  assert.equal(await row.getByRole('switch').count(), 0, 'no enable switch')
  await row.getByRole('link', { name: 'new_checkout' }).click()
  await page.getByText('Salt okunur: değiştirmek için yönetici rolü gerekir').waitFor()
  assert.equal(await page.getByRole('button', { name: 'Kaydet', exact: true }).count(), 0)
  assert.ok(await page.getByLabel('Yayılım').isDisabled())
  await shot('30-viewer-flag')

  await go(`/p/${pid}/settings`)
  assert.equal(await page.getByRole('tab', { name: 'Gizli anahtarlar' }).count(), 0)
  assert.ok(await page.getByLabel('Proje adı').isDisabled())
  assert.equal(await page.getByRole('button', { name: 'Projeyi kapat' }).count(), 0)
  await page.getByRole('tab', { name: 'Yazma token’ları' }).click()
  await page.locator('tr', { hasText: 'Web – canlı' }).waitFor()
  assert.equal(await page.getByRole('button', { name: 'Yeni token' }).count(), 0)
  assert.equal(await page.getByRole('button', { name: 'Düzenle' }).count(), 0)
  await shot('31-viewer-tokens')
  // the AI assistants page is readable; creating a secret key stays an admin's job
  await page.getByRole('tab', { name: 'Yapay zekâ asistanları' }).click()
  assert.equal(await page.getByTestId('mcp-url').inputValue(), base + '/mcp')
  await page.getByTestId('mcp-cmd-key').waitFor()
  assert.equal(await page.getByRole('link', { name: 'Gizli anahtar oluştur' }).count(), 0)
  await page.getByText('Gizli anahtarı yalnızca projenin yöneticisi oluşturabilir (Proje ayarları → Gizli anahtarlar).').waitFor()

  // typing an admin URL does not help
  await go(`/p/${pid}/deletions`)
  await page.getByText('Silme isteklerini görmek için yönetici rolü gerekir').waitFor()
  await go(`/p/${pid}/settings/keys`)
  assert.equal(await page.getByRole('button', { name: 'Yeni gizli anahtar' }).count(), 0)
  await go('/projects')
  assert.equal(await page.getByRole('button', { name: 'Yeni proje' }).count(), 0)
  await page.getByRole('link', { name: 'Üyeler' }).click()
  await page.locator('tbody tr', { hasText: VIEWER.email }).waitFor()
  assert.equal(await page.getByRole('button', { name: 'Üye ekle' }).count(), 0)
  assert.equal(await page.locator('tbody select').count(), 0)

  // and the server enforces the same table: hidden is not the only protection
  const statuses = await page.evaluate(async (pid) => {
    const me = await (await fetch('/api/me')).json()
    const call = (method, url, body) =>
      fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-LA-CSRF': me.csrf }, body: body && JSON.stringify(body) }).then((r) => r.status)
    return {
      flag: await call('POST', `/api/projects/${pid}/flags`, { key: 'x' }),
      insight: await call('POST', `/api/projects/${pid}/insights`, { name: 'x', kind: 'sql', query: {} }),
      keys: await call('GET', `/api/projects/${pid}/keys`),
      deletion: await call('POST', `/api/projects/${pid}/deletions`, { user_ids: ['1'] }),
      query: await call('POST', `/api/projects/${pid}/q/schema`, {}),
      otherProject: await call('GET', `/api/projects/999`),
      noCsrf: await fetch(`/api/projects/${pid}/q/schema`, { method: 'POST', body: '{}' }).then((r) => r.status),
    }
  }, pid)
  assert.deepEqual(statuses, { flag: 403, insight: 403, keys: 403, deletion: 403, query: 200, otherProject: 404, noCsrf: 403 })
})

await step('tablet width: navigation collapses, nothing overflows', async () => {
  await logout()
  await login(OWNER)
  await page.locator('.shell').waitFor()
  await page.setViewportSize({ width: 820, height: 1100 })
  for (const [name, p, ready] of [
    ['dashboard', `/p/${pid}/dashboards/${mock.db.dashboards[0].id}`, '.tile table.retention'],
    ['insight', `/p/${pid}/insights/${insightIds.funnel}`, '.funnel-step'],
    ['flag', `/p/${pid}/flags/new_checkout`, '.rule'],
    ['events', `/p/${pid}/events`, 'table.events tbody tr'],
    ['replay', `/p/${pid}/replay/${REPLAY_SESSION}`, 'canvas.replay-canvas'],
    ['tokens', `/p/${pid}/settings/tokens`, 'tbody tr'],
  ]) {
    await go(p)
    await page.locator(ready).first().waitFor()
    await noOverflow()
    await shot(`40-tablet-${name}`)
  }
  await page.setViewportSize({ width: 1366, height: 860 })
})

await step('keyboard: forms are reachable and operable without a mouse', async () => {
  await go(`/p/${pid}/insights/${insightIds.trends}`)
  await page.locator('.chart svg').waitFor()
  // skip link is the first stop and moves focus to the content
  await page.keyboard.press('Tab')
  assert.equal(await page.evaluate(() => document.activeElement?.className), 'skip')
  await page.keyboard.press('Enter')
  // the chart is focusable and arrow keys move the tooltip
  await page.locator('.chart svg').focus()
  await page.keyboard.press('ArrowRight')
  await page.keyboard.press('ArrowRight')
  await page.locator('.tooltip').waitFor()
  const first = await page.locator('.tooltip-title').innerText()
  await page.keyboard.press('ArrowRight')
  assert.notEqual(await page.locator('.tooltip-title').innerText(), first)
  await page.keyboard.press('Escape')
  assert.equal(await page.locator('.tooltip').count(), 0)
  // dialogs trap focus and close on Escape
  await go('/projects')
  await page.getByRole('button', { name: 'Yeni proje' }).focus()
  await page.keyboard.press('Enter')
  await page.getByRole('dialog').waitFor()
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press('Tab')
    // a modal <dialog> makes the page behind it inert: focus is in the dialog, or momentarily on the browser chrome (body)
    assert.ok(await page.evaluate(() => document.activeElement == document.body || !!document.activeElement?.closest('dialog')), 'focus never reaches the page behind the dialog')
  }
  await page.keyboard.press('Escape')
  assert.equal(await page.getByRole('dialog').count(), 0)
  // focus is visible: a focused control gets an outline
  await page.getByRole('button', { name: 'Yeni proje' }).focus()
  await page.keyboard.press('Shift+Tab')
  await page.keyboard.press('Tab')
  const outline = await page.evaluate(() => getComputedStyle(document.activeElement).outlineStyle)
  assert.equal(outline, 'solid')
})

await step('security headers and no unexpected console errors (CSP included)', async () => {
  const res = await page.request.get(base + '/p/1/unknown/deep/link')
  assert.equal(res.status(), 200, 'unknown paths fall back to index.html')
  assert.match(await res.text(), /<div id="app">/)
  assert.match(res.headers()['content-security-policy'], /default-src 'self'/)
  assert.equal(res.headers()['x-content-type-options'], 'nosniff')
  const api = await page.request.get(base + '/api/nope')
  assert.equal(api.status(), 404, '/api/ never falls back to the SPA')
  assert.deepEqual(problems, [])
})

await browser.close()
await mock.close()

const failed = results.filter((r) => !r.ok)
console.log(`\n${results.length - failed.length}/${results.length} e2e steps passed${shotsDir ? ` · screenshots in ${shotsDir}` : ''}`)
process.exit(failed.length ? 1 : 0)
