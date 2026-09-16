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

/** Only ever navigate to a same-origin path -- an absolute URL would be an open redirect. */
export function safeRedirectTarget(candidate: string | undefined): string {
  return candidate && candidate.startsWith('/') && !candidate.startsWith('//') ? candidate : '/'
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
  queryCache: new QueryCache({
    onError: (error) => {
      if (error instanceof HttpError && error.status === 401) goToLogin()
    },
  }),
})
