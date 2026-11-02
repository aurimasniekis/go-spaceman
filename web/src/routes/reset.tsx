import { createFileRoute } from '@tanstack/react-router'
import { ResetPage } from '@/features/auth/ResetPage'

// The token is pasted, never put in the URL (browser history, logs).
export const Route = createFileRoute('/reset')({ component: () => <ResetPage /> })
