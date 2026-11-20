import { createFileRoute } from '@tanstack/react-router'
import { LocalErrorAlert } from '@/components/LocalErrorBoundary'
import { PageHeader } from '@/components/PageHeader'
import { parseUsageSearch } from '@/features/usage/state'
import { UsageExplorer } from '@/features/usage/UsageExplorer'

/**
 * Token usage across every space the caller can read, with the rate-limit windows of their accounts. The range,
 * grouping and filters are search parameters, so a view can be linked to and survives a reload.
 */
export const Route = createFileRoute('/usage')({
  validateSearch: parseUsageSearch,
  component: UsagePage,
  errorComponent: ({ error, reset }) => <LocalErrorAlert error={error} title="Usage could not be shown" reset={reset} />,
})

function UsagePage() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <>
      <PageHeader title="Usage" description="Tokens the runs of every space you can see used, over time. Cost is the harness's own list-price estimate, not a bill" />
      <UsageExplorer search={search} onSearch={(next) => void navigate({ search: next, replace: true })} />
    </>
  )
}
