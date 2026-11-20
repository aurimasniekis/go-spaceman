import { createFileRoute } from '@tanstack/react-router'
import { HubEntry } from '@/components/HubEntry'

export const Route = createFileRoute('/integrations')({ component: () => <HubEntry hub="integrations" /> })
