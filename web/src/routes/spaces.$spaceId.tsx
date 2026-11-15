import { createFileRoute, Outlet } from '@tanstack/react-router'
import { SpaceHeader } from '@/features/spaces/SpaceHeader'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { isMiniApp } from '@/api/session'
import { TgSpaceHeader, TgSpaceNav } from '@/tg/pages/TgSpaceFrame'

/** Every space page, its sections and their detail pages share one shell: the space header and the section tabs (UX C6). */
export const Route = createFileRoute('/spaces/$spaceId')({ component: SpaceLayout })

function SpaceLayout() {
  const { spaceId } = Route.useParams()
  // The Mini App's frame holds only the sections a Telegram link can reach.
  if (isMiniApp()) return <><TgSpaceHeader spaceId={spaceId} /><TgSpaceNav spaceId={spaceId} /><Outlet /></>
  return (
    <>
      <SpaceHeader spaceId={spaceId} />
      <SpaceNav spaceId={spaceId} />
      <Outlet />
    </>
  )
}
