import { createFileRoute } from '@tanstack/react-router'
import { LoginPage } from '@/features/auth/LoginPage'

export const Route = createFileRoute('/login')({
  validateSearch: (s: Record<string, unknown>): { return_to?: string; sso_error?: string } => ({
    ...(typeof s.return_to === 'string' ? { return_to: s.return_to } : {}),
    ...(typeof s.sso_error === 'string' ? { sso_error: s.sso_error } : {}),
  }),
  component: function Login() {
    const { return_to, sso_error } = Route.useSearch()
    return <LoginPage returnTo={return_to} ssoError={sso_error} />
  },
})
