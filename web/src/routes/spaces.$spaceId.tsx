import { createFileRoute, Outlet } from '@tanstack/react-router'
import { SpaceHeader } from '@/features/spaces/SpaceHeader'
import { SpaceNav } from '@/features/spaces/SpaceNav'

/** Every space page, its sections and their detail pages share one shell: the space header and the section tabs (UX C6). */
export const Route = createFileRoute('/spaces/$spaceId')({ component: SpaceLayout })

function SpaceLayout() {
  const { spaceId } = Route.useParams()
  return (
    <>
      <SpaceHeader spaceId={spaceId} />
      <SpaceNav spaceId={spaceId} />
      <Outlet />
    </>
  )
}
