// Locale switchboard. All user-facing text lives in one module per language
// (`tr.ts`, `en.ts`); to add a language, write another module of type
// `Strings`, register it in LOCALES and teach `pickLocale` when to choose it.
import { en } from './en'
import { tr } from './tr'

export type Strings = typeof tr

const LOCALES = {
  tr: { strings: tr, intl: 'tr-TR', name: 'Türkçe' },
  en: { strings: en, intl: 'en-US', name: 'English' },
} satisfies Record<string, { strings: Strings; intl: string; name: string }>

export type LocaleCode = keyof typeof LOCALES

const isLocale = (code: unknown): code is LocaleCode => typeof code == 'string' && Object.hasOwn(LOCALES, code)

/** Languages for the switch, each named in its own language. */
export const LOCALE_OPTIONS: readonly (readonly [LocaleCode, string])[] = (Object.keys(LOCALES) as LocaleCode[]).map((code) => [code, LOCALES[code].name] as const)

/** localStorage key of the user's explicit choice. */
export const LOCALE_KEY = 'la_locale'

/** Current strings. A live binding: reassigned by `setLocale`. */
export let S: Strings = tr
/** BCP 47 tag for `Intl` formatting. */
export let INTL: string = LOCALES.tr.intl
/** Code of the current locale. */
export let LOCALE: LocaleCode = 'tr'

const listeners = new Set<() => void>()

/** Calls `fn` after every locale change; returns the unsubscribe function. */
export const onLocaleChange = (fn: () => void): (() => void) => {
  listeners.add(fn)
  return () => void listeners.delete(fn)
}

/** Switches the strings and the `Intl` tag. Unknown codes are ignored; safe to call repeatedly. */
export const setLocale = (code: string): void => {
  if (!isLocale(code)) return
  const changed = code != LOCALE
  const l = LOCALES[code]
  S = l.strings
  INTL = l.intl
  LOCALE = code
  if (typeof document != 'undefined') document.documentElement.lang = code
  if (changed) listeners.forEach((fn) => fn())
}

/** The stored choice if it names a known locale; otherwise Turkish for Turkish browsers, English for everyone else. */
export const pickLocale = (stored: string | null | undefined, browserLanguage: string | null | undefined): LocaleCode =>
  isLocale(stored) ? stored : (browserLanguage ?? '').toLowerCase().startsWith('tr') ? 'tr' : 'en'

/** Locale for this browser: `localStorage[LOCALE_KEY]`, else `navigator.language`. */
export const detectLocale = (): LocaleCode => {
  let stored: string | null = null
  try {
    stored = localStorage.getItem(LOCALE_KEY)
  } catch {
    // storage is blocked (private mode, sandboxed frame): fall back to the browser language
  }
  return pickLocale(stored, typeof navigator != 'undefined' ? navigator.language : undefined)
}

/** The user's explicit choice: remembered in this browser and applied at once. */
export const chooseLocale = (code: string): void => {
  if (!isLocale(code)) return
  try {
    localStorage.setItem(LOCALE_KEY, code)
  } catch {
    // not remembered; the choice still applies until the page is reloaded
  }
  setLocale(code)
}
