import type { Middleware } from 'openapi-fetch'

/**
 * Browser session plumbing (adapted from Pulse). The session cookie is HttpOnly; the page holds only the session's CSRF
 * token (from GET /auth/me or a sign-in response) and sends it as X-CSRF-Token on state-changing requests.
 *
 * - `reauth_required` / `mfa_required` (401) on a step-up operation: the registered prompt (ReauthModal) runs and the
 *   original request is retried exactly once. Concurrent requests share one prompt.
 * - `unauthorized` (401) outside /auth/*: the session is gone; the browser goes to /login and comes back afterwards.
 */

export const CSRF_HEADER = 'X-CSRF-Token'
export const API_PREFIX = '/api/v1'

export type ReauthCode = 'reauth_required' | 'mfa_required'
export type ReauthHandler = (code: ReauthCode) => Promise<boolean>

const AUTH_PREFIX = `${API_PREFIX}/auth/`

let csrf = ''
let limited = false
let reauthHandler: ReauthHandler | null = null
let inflight: Promise<boolean> | null = null

export function setSession(token: string | undefined | null, isLimited = false) {
  csrf = token ?? ''
  limited = isLimited
}

export function csrfToken(): string {
  return csrf
}

/** Registers the re-authentication prompt; returns an unregister function. */
export function setReauthHandler(h: ReauthHandler): () => void {
  reauthHandler = h
  return () => {
    if (reauthHandler === h) reauthHandler = null
  }
}

/** Paths that render without a session. */
export const PUBLIC_PATHS = ['/login', '/setup', '/reset', '/recover']

export function isPublicPath(path: string): boolean {
  return PUBLIC_PATHS.includes(path)
}

export function loginUrl(returnTo: string): string {
  return returnTo && returnTo !== '/' && !isPublicPath(returnTo.split('?')[0]) ? `/login?return_to=${encodeURIComponent(returnTo)}` : '/login'
}

/** Only same-origin relative paths are followed after sign-in (no open redirects). */
export function safeReturnTo(v: unknown): string {
  if (typeof v !== 'string' || !v.startsWith('/') || v.startsWith('//') || v.startsWith('/\\')) return '/'
  return isPublicPath(v.split('?')[0]) ? '/' : v
}

/** Sends the browser to sign-in and back to the current page afterwards (no-op on public pages). */
export function toLogin() {
  const here = `${window.location.pathname}${window.location.search}`
  if (!isPublicPath(window.location.pathname)) window.location.assign(loginUrl(here))
}

const mutating = (method: string) => !['GET', 'HEAD', 'OPTIONS'].includes(method.toUpperCase())

/** The `code` of a problem response, or "". */
export async function problemCode(res: Response): Promise<string> {
  try {
    const body = (await res.clone().json()) as { code?: unknown }
    return typeof body.code === 'string' ? body.code : ''
  } catch {
    return ''
  }
}

/** Asks for a re-authentication now (before a browser redirect that needs a recent sign-in, such as linking). */
export function requestReauth(code: ReauthCode = 'reauth_required'): Promise<boolean> {
  return prompt(code)
}

function prompt(code: ReauthCode): Promise<boolean> {
  if (!reauthHandler) return Promise.resolve(false)
  inflight ??= reauthHandler(code).finally(() => {
    inflight = null
  })
  return inflight
}

function prepare(request: Request): Request {
  if (mutating(request.method) && csrf && !request.headers.has('Authorization')) request.headers.set(CSRF_HEADER, csrf)
  return request
}

async function after(original: Request | undefined, request: Request, response: Response, doFetch: typeof fetch): Promise<Response> {
  if (response.status !== 401 || request.headers.has('Authorization')) return response
  const path = new URL(request.url, window.location.origin).pathname
  if (path.startsWith(AUTH_PREFIX)) return response
  const code = await problemCode(response)
  // A limited session (MFA still pending) gets mfa_required for everything: that is a sign-in, not a step-up.
  if ((code === 'reauth_required' || (code === 'mfa_required' && !limited)) && original) {
    if (!(await prompt(code as ReauthCode))) return response
    const retry = original.clone()
    if (mutating(retry.method)) retry.headers.set(CSRF_HEADER, csrf)
    return doFetch(retry)
  }
  if (code === 'unauthorized' || code === 'mfa_required') toLogin()
  return response
}

const originals = new WeakMap<Request, Request>()

/** openapi-fetch middleware for the API client. */
export const sessionMiddleware: Middleware = {
  onRequest({ request }) {
    prepare(request)
    originals.set(request, request.clone())
    return request
  },
  async onResponse({ request, response }) {
    return after(originals.get(request), request, response, (r) => fetch(r))
  },
}
