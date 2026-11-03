import { Button } from '@mantine/core'
import { IconRefresh } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useAttention } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { AttentionView } from '@/features/attention/AttentionView'

export const Route = createFileRoute('/attention')({ component: AttentionPage })

/**
 * One view of what needs a person (SM-UI-003): human actions, blocked and failed tasks, quarantined events, runs in
 * unknown or failed states, budgets, attachment storage, disconnected nodes, outbound dead letters, backups and
 * storage health. It refreshes when the change feed reports a change; storage health has no change event, so Refresh
 * re-reads it.
 */
function AttentionPage() {
  const q = useAttention()
  return (
    <>
      <PageHeader title="Attention" description="Everything that needs a person, across the spaces and areas you can see."
        actions={<Button variant="default" leftSection={<IconRefresh size={14} />} loading={q.isFetching} onClick={() => void q.refetch()}>Refresh</Button>} />
      <ErrorAlert error={q.error} />
      {q.data && <AttentionView data={q.data} />}
    </>
  )
}
