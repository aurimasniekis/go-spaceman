import { createFileRoute } from '@tanstack/react-router'
import { LoginPage } from '@/features/auth/LoginPage'

export const Route = createFileRoute('/login')({
  // password=1 shows every sign-in method and never continues to the direct provider (Hallpass /login/password).
  validateSearch: (s: Record<string, unknown>): { return_to?: string; sso_error?: string; password?: string } => ({
    ...(typeof s.return_to === 'string' ? { return_to: s.return_to } : {}),
    ...(typeof s.sso_error === 'string' ? { sso_error: s.sso_error } : {}),
    ...(s.password === '1' || s.password === 1 ? { password: '1' } : {}),
  }),
  component: function Login() {
    const { return_to, sso_error, password } = Route.useSearch()
    return <LoginPage returnTo={return_to} ssoError={sso_error} password={password === '1'} />
  },
})
