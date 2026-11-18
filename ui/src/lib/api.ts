// Control-plane API client: cookie session, CSRF header on mutating requests,
// uniform errors. Everything the UI sends goes through `request`.

export class ApiError extends Error {
  status: number
  code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export interface ApiOptions {
  /** injected in tests; defaults to the global fetch */
  fetch?: typeof fetch
  /** called once per 401 that means "the session is gone" */
  onUnauthorized?: () => void
  /** message for errors without a usable body, by status (0 = network) */
  fallbackMessage?: (status: number) => string
}

export interface RequestOptions {
  signal?: AbortSignal
  /**
   * 401 on this call is an answer about the submitted credentials (login,
   * password confirmation), not about the session: don't fire onUnauthorized.
   */
  credentialCheck?: boolean
}

export interface Api {
  setCsrf(token: string | null): void
  request<T>(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<T>
  get<T>(path: string, options?: RequestOptions): Promise<T>
  post<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>
  put<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>
  patch<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>
  del<T>(path: string, options?: RequestOptions): Promise<T>
  /** Binary GET (replay chunks). Same error mapping as `request`. */
  bytes(path: string, options?: RequestOptions): Promise<Uint8Array>
}

export const CSRF_HEADER = 'X-LA-CSRF'
const SAFE = new Set(['GET', 'HEAD', 'OPTIONS'])

export const createApi = (options: ApiOptions = {}): Api => {
  const doFetch = options.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init))
  const fallback = options.fallbackMessage ?? ((status: number) => (status ? `HTTP ${status}` : 'Network error'))
  let csrf: string | null = null

  const send = async (method: string, path: string, body: unknown, opts: RequestOptions): Promise<Response> => {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (!SAFE.has(method) && csrf) headers[CSRF_HEADER] = csrf
    let res: Response
    try {
      res = await doFetch(path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        credentials: 'same-origin',
        signal: opts.signal,
      })
    } catch (e) {
      if (e instanceof DOMException && e.name == 'AbortError') throw e
      if ((e as { name?: string })?.name == 'AbortError') throw e
      throw new ApiError(0, 'network', fallback(0))
    }
    if (res.ok) return res
    let code = 'http_' + res.status
    let message = fallback(res.status)
    try {
      const parsed = JSON.parse(await res.text()) as { error?: { code?: unknown; message?: unknown } }
      if (parsed && parsed.error) {
        if (typeof parsed.error.code == 'string') code = parsed.error.code
        if (typeof parsed.error.message == 'string' && parsed.error.message) message = parsed.error.message
      }
    } catch {
      // not JSON (e.g. a proxy error page): keep the fallback
    }
    if (res.status == 401 && !opts.credentialCheck) options.onUnauthorized?.()
    throw new ApiError(res.status, code, message)
  }

  const request = async <T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> => {
    const res = await send(method, path, body, opts)
    const text = await res.text()
    if (!text) return undefined as T
    try {
      return JSON.parse(text) as T
    } catch {
      throw new ApiError(res.status, 'bad_response', fallback(res.status))
    }
  }

  return {
    setCsrf: (token) => {
      csrf = token
    },
    request,
    get: (path, o) => request('GET', path, undefined, o),
    post: (path, body, o) => request('POST', path, body ?? {}, o),
    put: (path, body, o) => request('PUT', path, body ?? {}, o),
    patch: (path, body, o) => request('PATCH', path, body ?? {}, o),
    del: (path, o) => request('DELETE', path, undefined, o),
    bytes: async (path, o = {}) => new Uint8Array(await (await send('GET', path, undefined, o)).arrayBuffer()),
  }
}

/**
 * List endpoints are specified as bare arrays; tolerate a single-key wrapper
 * (`{"items": […]}`) so a harmless server-side envelope does not break the UI.
 */
export const asList = <T>(value: unknown): T[] => {
  if (Array.isArray(value)) return value as T[]
  if (value && typeof value == 'object') {
    const arrays = Object.values(value as Record<string, unknown>).filter(Array.isArray)
    if (arrays.length == 1) return arrays[0] as T[]
  }
  return []
}
