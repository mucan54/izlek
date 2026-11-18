// Locale switchboard. All user-facing text lives in one module per language
// (`tr.ts`); to add a language, write another module of type `Strings` and
// register it here.
import { tr } from './tr'

export type Strings = typeof tr

const LOCALES: Record<string, { strings: Strings; intl: string }> = {
  tr: { strings: tr, intl: 'tr-TR' },
}

/** Current strings. A live binding: reassigned by `setLocale`. */
export let S: Strings = tr
/** BCP 47 tag for `Intl` formatting. */
export let INTL = 'tr-TR'

export const setLocale = (code: string): void => {
  const l = LOCALES[code]
  if (!l) return
  S = l.strings
  INTL = l.intl
  if (typeof document != 'undefined') document.documentElement.lang = code
}
