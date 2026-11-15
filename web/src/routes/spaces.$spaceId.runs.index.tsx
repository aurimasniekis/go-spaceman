import { createFileRoute, Navigate } from '@tanstack/react-router'
import { isMiniApp } from '@/api/session'
import { TgRuns } from '@/tg/pages/TgRuns'

/** A space's runs list for the Telegram Mini App (read-only); in the web the runs are in the terminals workspace. */
export const Route = createFileRoute('/spaces/$spaceId/runs/')({ component: RunsIndex })

function RunsIndex() {
  const { spaceId } = Route.useParams()
  return isMiniApp() ? <TgRuns spaceId={spaceId} /> : <Navigate to="/spaces/$spaceId/terminals" params={{ spaceId }} replace />
}
