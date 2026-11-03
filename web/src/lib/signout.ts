import type { QueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import { markSignedOut } from './sso'

/**
 * Signs out of SpaceMan and goes to the sign-in page. For an SSO session whose provider enables RP-initiated logout,
 * the sign-in page then offers signing out at the provider too (that provider only, not a global logout; ADR-0045).
 */
export async function signOut(qc?: QueryClient) {
  let providerLogout: string | null = null
  try {
    const r = await api.POST('/auth/logout')
    providerLogout = r.data?.provider_logout_url ?? null
  } catch {
    // The session may already be gone; the sign-in page is the right place either way.
  }
  markSignedOut(providerLogout)
  qc?.clear()
  window.location.assign('/login')
}
