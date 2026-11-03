import { API_PREFIX, toLogin } from './session'

/**
 * Why the server ended a live stream (SM-AUTH-007). A stream's final `end` event carries `{"reason": …}` when the
 * server closed it for an access reason; a terminal whose run ended sends `{}`.
 *
 * - `unauthenticated`: the session or token that opened the stream is no longer valid (signed out, revoked, expired,
 *   user disabled) — or it was replaced by a newer one after a step-up, which keeps working.
 * - `forbidden`: still signed in, but no longer allowed to watch this stream (a grant was removed).
 */
export type StreamEndReason = 'unauthenticated' | 'forbidden' | ''

/** Reads the reason of an `end` event; anything unexpected counts as a normal end. */
export function endReason(ev: Event): StreamEndReason {
  try {
    const reason = (JSON.parse((ev as MessageEvent<string>).data ?? '{}') as { reason?: unknown }).reason
    return reason === 'unauthenticated' || reason === 'forbidden' ? reason : ''
  } catch {
    return ''
  }
}

/**
 * Re-checks the browser session after a stream ended as `unauthenticated`. A rotated session (after a step-up) is still
 * valid: resolves true and the caller reconnects. A session that is gone sends the browser to sign-in and resolves
 * false. A network failure resolves false without leaving the page.
 */
export async function sessionStillValid(): Promise<boolean> {
  try {
    const res = await fetch(`${API_PREFIX}/auth/me`, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
    if (res.ok) {
      const me = (await res.json()) as { limited?: boolean }
      if (!me.limited) return true
    }
    if (res.ok || res.status === 401) toLogin()
    return false
  } catch {
    return false
  }
}
