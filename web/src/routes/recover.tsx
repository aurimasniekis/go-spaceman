import { createFileRoute } from '@tanstack/react-router'
import { RecoverPage } from '@/features/auth/RecoverPage'

export const Route = createFileRoute('/recover')({ component: RecoverPage })
