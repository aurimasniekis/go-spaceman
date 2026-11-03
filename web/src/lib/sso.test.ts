import { clearSignedOut, markSignedOut, pendingProviderLogout, safeExternalUrl, ssoStartUrl, wasSignedOut } from './sso'

describe('sso', () => {
  it('builds start URLs with the purpose and a return path', () => {
    expect(ssoStartUrl('porch')).toBe('/api/v1/auth/oidc/porch/start?purpose=login')
    expect(ssoStartUrl('porch', 'login', '/spaces')).toBe('/api/v1/auth/oidc/porch/start?purpose=login&return_to=%2Fspaces')
  })

  it('follows only https (or localhost http) provider logout URLs without credentials', () => {
    expect(safeExternalUrl('https://id.example.com/oauth2/end_session?client_id=x')).toBe('https://id.example.com/oauth2/end_session?client_id=x')
    expect(safeExternalUrl('http://localhost:8081/logout')).toBe('http://localhost:8081/logout')
    expect(safeExternalUrl('http://id.example.com/logout')).toBeNull()
    expect(safeExternalUrl('javascript:alert(1)')).toBeNull()
    expect(safeExternalUrl('https://user:pw@id.example.com/')).toBeNull()
    expect(safeExternalUrl('/login')).toBeNull()
    expect(safeExternalUrl(null)).toBeNull()
  })

  it('remembers a sign-out and the provider logout to offer, until sign-in', () => {
    clearSignedOut()
    expect(wasSignedOut()).toBe(false)
    markSignedOut('https://id.example.com/oauth2/end_session?client_id=x')
    expect(wasSignedOut()).toBe(true)
    expect(pendingProviderLogout()).toBe('https://id.example.com/oauth2/end_session?client_id=x')
    markSignedOut(null) // "Not now" or followed: still signed out, nothing more to offer
    expect(wasSignedOut()).toBe(true)
    expect(pendingProviderLogout()).toBeNull()
    markSignedOut('javascript:alert(1)')
    expect(pendingProviderLogout()).toBeNull()
    clearSignedOut()
    expect(wasSignedOut()).toBe(false)
  })
})
