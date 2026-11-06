import { Breadcrumbs, CopyButton, Text, Tooltip } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { AnchorLink } from '@/components/AnchorLink'
import { useRun } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { RunWorkbench } from '@/features/runs/RunWorkbench'
import { isEnded, runTitle } from '@/features/runs/runInfo'
import { RunUsageCard } from '@/features/usage/UsageCards'

export const Route = createFileRoute('/spaces/$spaceId/runs/$runId')({
  validateSearch: (s: Record<string, unknown>): { view?: 'live' | 'replay' } => (s.view === 'replay' || s.view === 'live' ? { view: s.view } : {}),
  component: RunPage,
})

/**
 * One run, inside the space layout (UX review §4.1/§4.2): the workbench with the inspector open, a breadcrumb back to
 * the space's terminals, and the full usage table below.
 */
function RunPage() {
  const { spaceId, runId } = Route.useParams()
  const { view } = Route.useSearch()
  const navigate = Route.useNavigate()
  const run = useRun(spaceId, runId)
  if (run.error) return <ErrorAlert error={run.error} />
  if (!run.data) return null
  const r = run.data
  const mode = view ?? (isEnded(r) ? 'replay' : 'live')
  const crumbs = (
    <Breadcrumbs>
      <AnchorLink to="/spaces/$spaceId/terminals" params={{ spaceId }} search={{ run: runId }} size="sm">Terminals</AnchorLink>
      <CopyButton value={runId}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? 'Copied' : `${runId} (click to copy)`}>
            <Text size="sm" onClick={copy} style={{ cursor: 'copy' }}>{runTitle(r)}</Text>
          </Tooltip>
        )}
      </CopyButton>
    </Breadcrumbs>
  )
  return (
    <>
      <RunWorkbench spaceId={spaceId} run={r} mode={mode} onMode={(v) => void navigate({ search: { view: v } })} crumbs={crumbs} />
      <div style={{ marginTop: 'var(--mantine-spacing-md)' }}>
        <RunUsageCard spaceId={spaceId} runId={runId} />
      </div>
    </>
  )
}
