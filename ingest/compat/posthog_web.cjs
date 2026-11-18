// posthog-js in headless Chromium against the ingestion compat adapter.
// Usage: node posthog_web.cjs <page-url>
// The page (served by the Go test on another origin than ingestion, so CORS
// is exercised for real) loads the pinned posthog-js bundle and calls
// posthog.init(...). Prints one JSON line:
//   { problems, responses, queueLength, retryQueueLength, distinctId }
'use strict'
const { chromium } = require('playwright')

const [pageURL] = process.argv.slice(2)
if (!pageURL) {
  console.error('usage: posthog_web.cjs <page-url>')
  process.exit(2)
}

async function main() {
  const browser = await chromium.launch({ args: ['--no-sandbox'] })
  try {
    const page = await browser.newPage()
    const problems = []
    const responses = []
    page.on('console', (m) => {
      if (m.type() === 'error' || m.type() === 'warning') problems.push(`console.${m.type()}: ${m.text()}`)
    })
    page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
    page.on('requestfailed', (r) => problems.push(`requestfailed: ${r.method()} ${r.url()} ${r.failure() && r.failure().errorText}`))
    page.on('response', (r) => {
      const u = new URL(r.url())
      responses.push({ method: r.request().method(), path: u.pathname, query: u.search, status: r.status() })
    })

    await page.goto(pageURL, { waitUntil: 'load' })
    await page.waitForFunction(() => window.__ph_loaded === true, null, { timeout: 15000 })

    await page.evaluate(() => {
      window.posthog.capture('web_capture', { plan: 'pro', n: 1 })
      window.posthog.identify('web-user-1', { name: 'Web User' })
      window.posthog.capture('web_after_identify', { text: 'çğıöşü 🚀' })
      window.posthog.capture('$pageview', { manual: true })
    })

    // posthog-js batches requests (3 s tick); wait until its queues are empty
    // and the last event has been acknowledged.
    const queueState = () => page.evaluate(() => {
      const ph = window.posthog
      const q = ph._requestQueue && (ph._requestQueue._queue || ph._requestQueue.queue)
      const rq = ph._retryQueue && (ph._retryQueue._queue || ph._retryQueue.queue)
      return { queue: q ? q.length : 0, retry: rq ? rq.length : 0 }
    })
    const deadline = Date.now() + 20000
    let state = await queueState()
    for (;;) {
      const captured = responses.filter((r) => r.method === 'POST' && /^\/(e|batch|i\/v0\/e|capture)\/?$/.test(r.path)).length
      state = await queueState()
      if (captured >= 2 && state.queue === 0 && state.retry === 0) break
      if (Date.now() > deadline) break
      await page.waitForTimeout(250)
    }
    await page.waitForTimeout(500) // let in-flight responses settle
    state = await queueState()
    const distinctId = await page.evaluate(() => window.posthog.get_distinct_id())
    console.log(JSON.stringify({ problems, responses, queueLength: state.queue, retryQueueLength: state.retry, distinctId }))
  } finally {
    await browser.close()
  }
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
