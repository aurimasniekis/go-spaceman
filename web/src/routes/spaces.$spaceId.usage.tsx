import { createFileRoute } from '@tanstack/react-router'
import { LocalErrorAlert } from '@/components/LocalErrorBoundary'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { SpaceUsage } from '@/features/usage/SpaceUsage'

export const Route = createFileRoute('/spaces/$spaceId/usage')({
  component: UsagePage,
  errorComponent: ({ error, reset }) => <LocalErrorAlert error={error} title="Usage could not be shown" reset={reset} />,
})

function UsagePage() {
  const { spaceId } = Route.useParams()
  return (
    <>
      <SectionHeader title="Usage" description="Tokens the space's runs used, over time. Cost is the harness's own list-price estimate, not a bill" />
      <SpaceUsage spaceId={spaceId} />
    </>
  )
}
