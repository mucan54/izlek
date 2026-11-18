import { useCallback, useEffect, useRef, useState } from 'preact/hooks'

export interface Async<T> {
  data: T | undefined
  error: Error | undefined
  loading: boolean
  /** re-runs the loader; keeps showing the previous data meanwhile */
  reload: () => void
  /** replaces the data locally (after a mutation whose response is the new state) */
  set: (data: T) => void
}

/**
 * Runs `load` when `deps` change. Stale responses are dropped and the request
 * is aborted on unmount or when superseded. `null` deps entry `skip` = don't run.
 */
export const useAsync = <T>(load: (signal: AbortSignal) => Promise<T>, deps: unknown[], skip = false): Async<T> => {
  const [state, setState] = useState<{ data?: T; error?: Error; loading: boolean }>({ loading: !skip })
  const [tick, setTick] = useState(0)
  const loader = useRef(load)
  loader.current = load

  useEffect(() => {
    if (skip) {
      setState({ loading: false })
      return
    }
    const ctl = new AbortController()
    setState((s) => ({ data: s.data, loading: true }))
    loader.current(ctl.signal).then(
      (data) => {
        if (!ctl.signal.aborted) setState({ data, loading: false })
      },
      (error: Error) => {
        if (!ctl.signal.aborted && error?.name != 'AbortError') setState({ error, loading: false })
      },
    )
    return () => ctl.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick, skip])

  const reload = useCallback(() => setTick((n) => n + 1), [])
  const set = useCallback((data: T) => setState({ data, loading: false }), [])
  return { data: state.data, error: state.error, loading: state.loading, reload, set }
}

/** Runs an action with busy/error state, for form submits and buttons. */
export const useAction = <A extends unknown[]>(fn: (...args: A) => Promise<unknown>) => {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error | undefined>()
  const ref = useRef(fn)
  ref.current = fn
  const alive = useRef(true)
  useEffect(() => () => void (alive.current = false), [])
  const run = useCallback(async (...args: A): Promise<boolean> => {
    setBusy(true)
    setError(undefined)
    try {
      await ref.current(...args)
      if (alive.current) setBusy(false)
      return true
    } catch (e) {
      if (alive.current) {
        setBusy(false)
        setError(e as Error)
      }
      return false
    }
  }, [])
  return { run, busy, error, clear: () => setError(undefined) }
}

/** Calls `fn` every `ms` while `active` and the tab is visible. */
export const useInterval = (fn: () => void, ms: number, active: boolean): void => {
  const ref = useRef(fn)
  ref.current = fn
  useEffect(() => {
    if (!active) return
    const id = setInterval(() => {
      if (document.visibilityState == 'visible') ref.current()
    }, ms)
    return () => clearInterval(id)
  }, [ms, active])
}

/** Width of an element, tracked with ResizeObserver. */
export const useWidth = <E extends Element>(initial = 640): [preact.RefObject<E>, number] => {
  const ref = useRef<E>(null)
  const [width, setWidth] = useState(initial)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const measure = () => setWidth(Math.max(120, Math.floor(el.getBoundingClientRect().width)))
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, width]
}
