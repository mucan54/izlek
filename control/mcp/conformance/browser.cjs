// The sign-in and consent pages in a real browser (headless Chromium).
// Usage: node browser.cjs <authorize-url> <callback-origin> <email> <password> <locale>
//
// A browser enforces what no HTTP client does: the Content-Security-Policy
// of the pages (including form-action across the redirect back to the
// client), cookie attributes, and which Origin a form submission carries.
// Prints one JSON line: { signIn, consent, styled, landed, problems }.
'use strict'
const { chromium } = require('playwright')

const [authorizeURL, callbackOrigin, email, password, locale] = process.argv.slice(2)
if (!locale) {
  console.error('usage: browser.cjs <authorize-url> <callback-origin> <email> <password> <locale>')
  process.exit(2)
}

async function main() {
  const browser = await chromium.launch({ args: ['--no-sandbox'] })
  try {
    const context = await browser.newContext({ locale })
    const page = await context.newPage()
    const problems = []
    page.on('console', (m) => {
      if (m.type() === 'error' || m.type() === 'warning') problems.push(`console.${m.type()}: ${m.text()}`)
    })
    page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))

    await page.goto(authorizeURL, { waitUntil: 'load' })
    const signIn = await page.locator('h1').innerText()
    await page.fill('#email', email)
    await page.fill('#password', password)
    await page.click('button[value=login]')
    await page.waitForSelector('button[value=approve]', { timeout: 15000 })
    const consent = await page.locator('h1').innerText()
    // The inline style sheet is allowed by its nonce and nothing else.
    const styled = await page.evaluate(() => getComputedStyle(document.querySelector('.card')).borderTopLeftRadius)
    const framed = await page.evaluate(() => window.top !== window.self)
    await page.click('button[value=approve]')
    await page.waitForURL((u) => u.origin === callbackOrigin, { timeout: 15000 })
    console.log(JSON.stringify({ signIn, consent, styled, framed, landed: page.url(), problems }))
  } finally {
    await browser.close()
  }
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
