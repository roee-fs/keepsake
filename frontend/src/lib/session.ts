import { QueryCache, QueryClient } from '@tanstack/react-query'
import { client } from '../client/client.gen'

// Global default, not a per-call option: a query that forgot to opt in would
// resolve with an unthrown `{ error }` envelope instead, invisible to the
// QueryCache below.
client.setConfig({ baseUrl: '/api', throwOnError: true })

/**
 * The generated client throws the response body on a non-2xx status but drops
 * the status code itself. This interceptor is the one place that puts it back,
 * so callers can tell a 401 apart from any other failure.
 */
export class HttpError extends Error {
  status: number
  body: unknown

  constructor(status: number, body: unknown) {
    super(`request failed with status ${status}`)
    this.status = status
    this.body = body
  }
}

client.interceptors.error.use((error, response) => new HttpError(response?.status ?? 0, error))

const LOGIN_PATH = '/login'

// Prefix checks don't model URL parsing -- e.g. "/\evil.com" isn't caught by
// startsWith('//'), because WHATWG URL parsing normalizes the backslash to a
// slash and resolves it off-origin. Parse and compare origins instead. The
// try/catch matters: a malformed candidate must fail closed to '/', not throw
// and take the login page down with it.
export function safeRedirectTarget(candidate: string | undefined): string {
  if (!candidate) return '/'
  try {
    const url = new URL(candidate, window.location.origin)
    return url.origin === window.location.origin ? `${url.pathname}${url.search}` : '/'
  } catch {
    return '/'
  }
}

function goToLogin(): void {
  if (window.location.pathname === LOGIN_PATH) return
  const here = `${window.location.pathname}${window.location.search}`
  window.location.assign(`${LOGIN_PATH}?redirect=${encodeURIComponent(here)}`)
}

// The session cookie is HttpOnly, so a 401 is the only signal the session ended.
// Handling it here -- once, for every query -- means no query has to check for
// it itself. Mutations (login, logout) deliberately go through their own error
// handling instead, since a failed login must show inline, not bounce to /login.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A 4xx means the request itself is wrong -- 401 the session is gone, 422 bad
      // params, 404 no such concept -- and retrying can't turn it into a success.
      // Without this, the default 3 retries delay a 401's redirect by ~7s of
      // exponential backoff. Keep retrying everything else (5xx, dropped connections).
      retry: (failureCount, error) =>
        error instanceof HttpError && error.status >= 400 && error.status < 500
          ? false
          : failureCount < 3,
    },
  },
  queryCache: new QueryCache({
    onError: (error) => {
      if (error instanceof HttpError && error.status === 401) goToLogin()
    },
  }),
})
