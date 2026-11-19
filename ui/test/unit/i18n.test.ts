import assert from 'node:assert/strict'
import { test } from 'node:test'
import { INTL, LOCALE, LOCALE_KEY, LOCALE_OPTIONS, S, chooseLocale, detectLocale, onLocaleChange, pickLocale, setLocale } from '../../src/i18n'
import { en } from '../../src/i18n/en'
import { tr } from '../../src/i18n/tr'
import { fmtAgo, fmtCompact, fmtDuration, fmtPct } from '../../src/lib/format'
import { android, ios } from '../../src/lib/snippets'

type Fn = (...args: unknown[]) => unknown
type Leaf = { path: string; value: unknown }

/** Every leaf (string or function) of a locale tree, keyed by its dotted path. */
const leaves = (node: unknown, path = ''): Leaf[] =>
  node !== null && typeof node == 'object'
    ? Object.entries(node as Record<string, unknown>).flatMap(([k, v]) => leaves(v, path ? `${path}.${k}` : k))
    : [{ path, value: node }]

/** `path → kind`, where a function's kind includes its arity. */
const shape = (tree: unknown): Record<string, string> =>
  Object.fromEntries(leaves(tree).map(({ path, value }) => [path, typeof value == 'function' ? `function/${value.length}` : typeof value]))

const TR = leaves(tr)
const EN = new Map(leaves(en).map((l) => [l.path, l.value]))

/** Runs a locale function; null when it throws or does not return a non-empty string. */
const call = (fn: unknown, args: unknown[]): string | null => {
  try {
    const out = (fn as Fn)(...args)
    return typeof out == 'string' && out != '' ? out : null
  } catch {
    return null
  }
}

/** Argument sets tried against every function: counts (0, 1, many), formatted numbers, names, an empty string. */
const SAMPLES: unknown[][] = [
  [0, 0],
  [1, 1],
  [2, 5],
  [37, 91],
  ['1', '1'],
  ['zq37', 'zq91'],
  ['', '00:42'],
]

test('en and tr have the same keys, value types and function arities', () => {
  assert.deepEqual(shape(en), shape(tr))
  // only strings and functions are allowed as leaves
  for (const { path, value } of [...TR, ...leaves(en)]) assert.ok(typeof value == 'string' || typeof value == 'function', `${path} is a ${typeof value}`)
  assert.ok(TR.length > 500, 'the walk reaches the whole tree')
})

test('no string is empty in either locale', () => {
  for (const { path, value } of TR) {
    if (typeof value != 'string') continue
    assert.ok(value.trim() != '', `tr.${path} is empty`)
    assert.ok((EN.get(path) as string).trim() != '', `en.${path} is empty`)
  }
})

test('every function returns a non-empty string in en for the arguments that work in tr', () => {
  let functions = 0
  for (const { path, value } of TR) {
    if (typeof value != 'function') continue
    functions++
    let worked = 0
    for (const args of SAMPLES) {
      const turkish = call(value, args)
      if (turkish == null) continue // not a meaningful input for this function (e.g. a number where a name is expected)
      worked++
      const english = call(EN.get(path), args)
      assert.ok(english != null, `en.${path}(${JSON.stringify(args)}) should return text like tr does (${JSON.stringify(turkish)})`)
      // an interpolated argument must not get lost in translation
      for (const arg of args) {
        if (String(arg).length >= 2 && turkish.includes(String(arg))) assert.ok(english.includes(String(arg)), `en.${path}(${JSON.stringify(args)}) drops ${JSON.stringify(arg)}: ${JSON.stringify(english)}`)
      }
    }
    assert.ok(worked > 0, `no sample arguments work for tr.${path}`)
  }
  assert.ok(functions > 40, 'the walk reaches the functions')
})

test('English strings contain no Turkish text', () => {
  const turkishLetters = /[çğıöşüÇĞİÖŞÜ]/
  for (const [path, value] of EN) {
    const texts = typeof value == 'function' ? SAMPLES.map((args) => call(value, args) ?? '') : [value as string]
    for (const text of texts) assert.ok(!turkishLetters.test(text), `en.${path}: ${JSON.stringify(text)}`)
  }
})

test('English counts are singular for one and plural otherwise', () => {
  assert.equal(en.common.days(1), '1 day')
  assert.equal(en.common.days(30), '30 days')
  assert.equal(en.events.shown(1), 'Showing 1 event')
  assert.equal(en.events.shown(0), 'Showing 0 events')
  assert.equal(en.events.shown(250), 'Showing 250 events')
  assert.equal(en.insights.count(1), '1 insight')
  assert.equal(en.insights.count(4), '4 insights')
  assert.equal(en.query.files(1), '1 file')
  assert.equal(en.query.files(12), '12 files')
  assert.equal(en.sql.rowCount('1'), '1 row')
  assert.equal(en.sql.rowCount('1,234'), '1,234 rows')
  assert.equal(en.retention.cellTitle('1'), '1 person returned')
  assert.equal(en.retention.cellTitle('12'), '12 people returned')
  assert.equal(en.flags.fixErrors(1), 'Fix 1 error before saving.')
  assert.equal(en.flags.fixErrors(2), 'Fix 2 errors before saving.')
  assert.equal(en.deletions.confirmMessage(1, 2), 'All data belonging to 1 user and 2 anonymous IDs will be deleted.')
  assert.equal(en.deletions.confirmMessage(2, 1), 'All data belonging to 2 users and 1 anonymous ID will be deleted.')
  assert.equal(en.range.lastDays(7), 'Last 7 days')
  assert.equal(en.range.lastHours(1), 'Last hour')
  assert.equal(en.range.lastHours(24), 'Last 24 hours')
  assert.match(en.replay.zstdTitle(1, 5), /^1 of 5 chunks is /)
  assert.match(en.replay.zstdTitle(5, 5), /^5 of 5 chunks are /)
  assert.match(en.query.trendsAria(3, 1), /3 series, 1 time bucket\./)
  assert.match(en.query.trendsAria(1, 30), /1 series, 30 time buckets\./)
  assert.equal(en.fmt.ago(0, 's'), 'just now')
  assert.equal(en.fmt.ago(3, 'min'), '3 min ago')
  // insight kinds are lower-cased in running text, acronyms are not
  assert.equal(en.insights.untitled(en.insights.kinds.trends), 'Untitled trend')
  assert.equal(en.insights.untitled(en.insights.kinds.sql), 'Untitled SQL')
  assert.equal(tr.insights.untitled(tr.insights.kinds.funnel), 'Adsız huni')
})

test('pickLocale: a valid stored choice wins, otherwise Turkish browsers get tr and everyone else en', () => {
  assert.equal(pickLocale('en', 'tr-TR'), 'en')
  assert.equal(pickLocale('tr', 'en-US'), 'tr')
  assert.equal(pickLocale(null, 'tr'), 'tr')
  assert.equal(pickLocale(null, 'tr-TR'), 'tr')
  assert.equal(pickLocale(null, 'TR-tr'), 'tr')
  assert.equal(pickLocale(null, 'en-US'), 'en')
  assert.equal(pickLocale(null, 'de-DE'), 'en')
  assert.equal(pickLocale(null, 'tk-TM'), 'en', 'Turkmen is not Turkish')
  assert.equal(pickLocale(null, undefined), 'en')
  for (const junk of ['', 'de', 'EN', 'tr-TR', 'constructor', '__proto__', 'toString']) {
    assert.equal(pickLocale(junk, 'tr-TR'), 'tr', `stored ${JSON.stringify(junk)} is ignored`)
    assert.equal(pickLocale(junk, 'fr'), 'en', `stored ${JSON.stringify(junk)} is ignored`)
  }
})

test('setLocale switches the live bindings; formatting follows; unknown codes are ignored', () => {
  setLocale('tr')
  assert.deepEqual([LOCALE, INTL, S === tr], ['tr', 'tr-TR', true])
  assert.equal(S.auth.loginTitle, 'Oturum aç')
  assert.equal(fmtPct(0.25), '%25')
  assert.equal(fmtPct(0.125), '%12,5')
  assert.equal(fmtCompact(12_500), '12,5 B')
  assert.equal(fmtDuration(200), '3 dk 20 sn')
  assert.equal(fmtAgo(1_000_000 - 180_000, 1_000_000), '3 dk önce')
  assert.match(android({ token: 'pk_x', host: 'https://in.example.com', sampleRate: 1, replay: false }), /\/\/ isteğe bağlı/)
  assert.match(ios({ token: 'pk_x', host: 'https://in.example.com', sampleRate: 1, replay: false }), /KMP çatısı \(IzlekAnalytics\.xcframework\)/)

  let notified = 0
  const off = onLocaleChange(() => notified++)
  setLocale('en')
  assert.deepEqual([LOCALE, INTL, S === en], ['en', 'en-US', true])
  assert.equal(S.auth.loginTitle, 'Sign in')
  assert.equal(fmtPct(0.25), '25%')
  assert.equal(fmtPct(0.125), '12.5%')
  assert.equal(fmtCompact(12_500), '12.5K')
  assert.equal(fmtDuration(200), '3 min 20 s')
  assert.equal(fmtAgo(1_000_000 - 180_000, 1_000_000), '3 min ago')
  assert.match(android({ token: 'pk_x', host: 'https://in.example.com', sampleRate: 1, replay: false }), /\/\/ optional/)
  assert.match(ios({ token: 'pk_x', host: 'https://in.example.com', sampleRate: 1, replay: false }), /KMP framework \(IzlekAnalytics\.xcframework\)/)
  assert.equal(notified, 1)

  setLocale('en') // again: nothing changes, nobody is notified
  assert.equal(notified, 1)
  for (const junk of ['de', '', 'EN', 'constructor']) setLocale(junk)
  assert.deepEqual([LOCALE, INTL, S === en, notified], ['en', 'en-US', true, 1])

  setLocale('tr')
  assert.deepEqual([LOCALE, INTL, S === tr, notified], ['tr', 'tr-TR', true, 2])
  off()
  setLocale('en')
  setLocale('tr')
  assert.equal(notified, 2, 'unsubscribed listeners are not called')
})

test('the language switch lists every locale by its own name', () => {
  assert.deepEqual(LOCALE_OPTIONS, [['tr', 'Türkçe'], ['en', 'English']])
})

/** Replaces a global for the duration of `fn` (Node has no localStorage and its own navigator). */
const withGlobals = (globals: Record<string, unknown>, fn: () => void): void => {
  const saved = Object.keys(globals).map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)] as const)
  for (const [k, value] of Object.entries(globals)) Object.defineProperty(globalThis, k, { value, configurable: true, writable: true })
  try {
    fn()
  } finally {
    for (const [k, d] of saved) {
      if (d) Object.defineProperty(globalThis, k, d)
      else delete (globalThis as Record<string, unknown>)[k]
    }
  }
}

const fakeStorage = (initial: Record<string, string> = {}) => {
  const data = new Map(Object.entries(initial))
  return { data, getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => void data.set(k, v) }
}
const brokenStorage = {
  getItem: () => {
    throw new Error('SecurityError')
  },
  setItem: () => {
    throw new Error('QuotaExceededError')
  },
}

test('detectLocale reads la_locale, then navigator.language; storage errors are survived', () => {
  assert.equal(LOCALE_KEY, 'la_locale')
  withGlobals({ localStorage: fakeStorage({ la_locale: 'en' }), navigator: { language: 'tr-TR' } }, () => assert.equal(detectLocale(), 'en'))
  withGlobals({ localStorage: fakeStorage({ la_locale: 'tr' }), navigator: { language: 'en-GB' } }, () => assert.equal(detectLocale(), 'tr'))
  withGlobals({ localStorage: fakeStorage({ la_locale: 'xx' }), navigator: { language: 'tr-TR' } }, () => assert.equal(detectLocale(), 'tr'))
  withGlobals({ localStorage: fakeStorage(), navigator: { language: 'tr' } }, () => assert.equal(detectLocale(), 'tr'))
  withGlobals({ localStorage: fakeStorage(), navigator: { language: 'en-US' } }, () => assert.equal(detectLocale(), 'en'))
  withGlobals({ localStorage: fakeStorage(), navigator: { language: 'ja-JP' } }, () => assert.equal(detectLocale(), 'en'))
  withGlobals({ localStorage: brokenStorage, navigator: { language: 'tr-TR' } }, () => assert.equal(detectLocale(), 'tr'))
  withGlobals({ localStorage: undefined, navigator: { language: 'en-US' } }, () => assert.equal(detectLocale(), 'en'))
})

test('chooseLocale stores the choice and applies it; a full or blocked storage still switches', () => {
  setLocale('tr')
  const storage = fakeStorage()
  withGlobals({ localStorage: storage, navigator: { language: 'tr-TR' } }, () => {
    chooseLocale('en')
    assert.equal(storage.data.get('la_locale'), 'en')
    assert.equal(S.nav.logout, 'Sign out')
    assert.equal(detectLocale(), 'en', 'the next page load starts in the chosen language')
    chooseLocale('xx') // unknown: neither stored nor applied
    assert.equal(storage.data.get('la_locale'), 'en')
    assert.equal(LOCALE, 'en')
    chooseLocale('tr')
    assert.equal(storage.data.get('la_locale'), 'tr')
    assert.equal(S.nav.logout, 'Çıkış yap')
  })
  withGlobals({ localStorage: brokenStorage }, () => {
    chooseLocale('en')
    assert.equal(LOCALE, 'en')
  })
  setLocale('tr')
})
