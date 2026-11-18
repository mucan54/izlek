// Minimal History-API router: current location as a hook, a Link that does
// not reload, and `:param` pattern matching.
import type { ComponentChildren, JSX } from 'preact'
import { useEffect, useState } from 'preact/hooks'

const listeners = new Set<() => void>()
const notify = () => listeners.forEach((l) => l())

if (typeof window != 'undefined') window.addEventListener('popstate', notify)

export const navigate = (to: string, replace = false): void => {
  if (to == location.pathname + location.search) return
  if (replace) history.replaceState(null, '', to)
  else history.pushState(null, '', to)
  notify()
  window.scrollTo(0, 0)
}

export interface Loc {
  path: string
  query: URLSearchParams
}

export const useLocation = (): Loc => {
  const [, bump] = useState(0)
  useEffect(() => {
    const l = () => bump((n) => n + 1)
    listeners.add(l)
    return () => {
      listeners.delete(l)
    }
  }, [])
  return { path: location.pathname, query: new URLSearchParams(location.search) }
}

/** `/p/:pid/flags/:key` against a path → params, or null. A trailing `/*` matches any remainder. */
export const matchPath = (pattern: string, path: string): Record<string, string> | null => {
  const wild = pattern.endsWith('/*')
  const want = (wild ? pattern.slice(0, -2) : pattern).split('/').filter(Boolean)
  const got = path.split('/').filter(Boolean)
  if (got.length < want.length || (!wild && got.length != want.length)) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < want.length; i++) {
    if (want[i].startsWith(':')) {
      try {
        params[want[i].slice(1)] = decodeURIComponent(got[i])
      } catch {
        return null
      }
    } else if (want[i] != got[i]) return null
  }
  return params
}

type LinkProps = Omit<JSX.HTMLAttributes<HTMLAnchorElement>, 'href'> & { href: string; children?: ComponentChildren }

export const Link = ({ href, children, onClick, ...rest }: LinkProps) => (
  <a
    {...rest}
    href={href}
    onClick={(e) => {
      onClick?.(e)
      if (e.defaultPrevented || e.button != 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      e.preventDefault()
      navigate(href)
    }}
  >
    {children}
  </a>
)
