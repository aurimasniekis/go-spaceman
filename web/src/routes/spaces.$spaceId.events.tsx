import { createFileRoute, Outlet } from '@tanstack/react-router'

export const Route = createFileRoute('/spaces/$spaceId/events')({ component: Outlet })
