import { createFileRoute } from '@tanstack/react-router'
import { SetupPage } from '@/features/auth/SetupPage'

export const Route = createFileRoute('/setup')({ component: SetupPage })
