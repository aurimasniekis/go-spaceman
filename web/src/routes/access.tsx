import { createFileRoute } from '@tanstack/react-router'
import { HubEntry } from '@/components/HubEntry'

export const Route = createFileRoute('/access')({ component: () => <HubEntry hub="access" /> })
