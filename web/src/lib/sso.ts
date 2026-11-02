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
