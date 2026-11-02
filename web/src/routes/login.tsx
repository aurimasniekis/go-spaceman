import { createFileRoute } from '@tanstack/react-router'
import { LoginPage } from '@/features/auth/LoginPage'

export const Route = createFileRoute('/login')({
  validateSearch: (s: Record<string, unknown>): { return_to?: string } => (typeof s.return_to === 'string' ? { return_to: s.return_to } : {}),
  component: function Login() {
    const { return_to } = Route.useSearch()
    return <LoginPage returnTo={return_to} />
  },
})
