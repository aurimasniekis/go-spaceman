import { API_PREFIX } from '@/api/session'

export type SSOPurpose = 'login' | 'stepup' | 'link' | 'test'

/** The browser URL that starts an OIDC flow at a provider (a full-page navigation, not a fetch). */
export function ssoStartUrl(slug: string, purpose: SSOPurpose = 'login', returnTo = '/'): string {
  const q = new URLSearchParams({ purpose })
  if (returnTo && returnTo !== '/') q.set('return_to', returnTo)
  return `${API_PREFIX}/auth/oidc/${encodeURIComponent(slug)}/start?${q.toString()}`
}

/** Plain-language messages for the stable `sso_error` codes the OIDC redirects carry back. */
const MESSAGES: Record<string, string> = {
  forbidden: 'The identity provider signed you in, but SpaceMan refused it: the identity is not provisioned here, the account is disabled, or the provider did not confirm a fresh login.',
  unauthorized: "The identity provider's response could not be validated. Try again.",
  validation_failed: 'The sign-in took too long or was started in another browser. Start again.',
  not_found: 'That sign-in provider is not available.',
  unavailable: 'The identity provider is unavailable right now. Try again later or use another sign-in method.',
  conflict: 'That identity is already linked to another SpaceMan user, or its email belongs to one. Ask an administrator to link it.',
  reauth_required: 'Linking needs a recent sign-in. Confirm it is you, then try again.',
  mfa_required: 'The identity provider cannot prove a fresh login. Confirm with your SpaceMan authenticator code.',
  precondition_failed: 'This needs a signed-in SSO session.',
}

export function ssoErrorMessage(code: string): string {
  return MESSAGES[code] ?? 'Single sign-on failed. Try again.'
}

const SIGNED_OUT_KEY = 'spaceman.signed_out'
const PROVIDER_LOGOUT_KEY = 'spaceman.provider_logout'

function store(key: string, value: string | null) {
  try {
    if (value === null) window.sessionStorage.removeItem(key)
    else window.sessionStorage.setItem(key, value)
  } catch {
    // Not persisted: the sign-in page may continue to the direct provider, and no provider sign-out is offered.
  }
}

function load(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key)
  } catch {
    return null
  }
}

/**
 * After a sign-out (ported from Hallpass): the sign-in page stays put instead of continuing to the direct provider, which
 * would sign straight back in, and offers the provider's RP-initiated logout URL when the server returned one.
 */
export function markSignedOut(providerLogoutUrl?: string | null) {
  store(SIGNED_OUT_KEY, '1')
  store(PROVIDER_LOGOUT_KEY, safeExternalUrl(providerLogoutUrl))
}

/** Clears the signed-out mark (the person chose to sign in again or left for the provider). */
export function clearSignedOut() {
  store(SIGNED_OUT_KEY, null)
  store(PROVIDER_LOGOUT_KEY, null)
}

export function wasSignedOut(): boolean {
  return load(SIGNED_OUT_KEY) === '1'
}

/** The provider logout URL to offer after a sign-out, or null. */
export function pendingProviderLogout(): string | null {
  return safeExternalUrl(load(PROVIDER_LOGOUT_KEY))
}

/** Only absolute https URLs (http on localhost, for development) without credentials are followed to a provider. */
export function safeExternalUrl(raw: string | null | undefined): string | null {
  if (!raw) return null
  try {
    const u = new URL(raw)
    if (u.username || u.password) return null
    if (u.protocol === 'https:') return u.toString()
    if (u.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(u.hostname)) return u.toString()
  } catch {
    return null
  }
  return null
}
