import { Stack, Text, Title } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { useInboxItem } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { MermaidDiagram } from '@/components/markdown/MermaidDiagram'
import { planDiagrams } from '@/lib/markdown'
import { mermaidSources } from '@/tg/diagrams'

// The "underscore" in the file name keeps this page a sibling of the item page instead of nested under it.
export const Route = createFileRoute('/spaces/$spaceId/inbox_/$itemId/diagram')({ component: DiagramPage })

/** The diagrams of one inbox item on a page of their own: the full width of a phone, the source one tap away. */
function DiagramPage() {
  const { spaceId, itemId } = Route.useParams()
  const item = useInboxItem(spaceId, itemId)
  if (item.error) return <ErrorAlert error={item.error} />
  if (!item.data) return null
  const sources = item.data.content_type === 'application/json' ? [] : mermaidSources(item.data.body)
  const plans = planDiagrams(sources)
  return (
    <Stack gap="md">
      <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId }} size="sm">← Back to the item</AnchorLink>
      <Title order={3}>{item.data.title}</Title>
      <Text size="xs" c="dimmed">Diagrams in an untrusted item are drawn in a restricted mode; the source is below each one.</Text>
      {sources.length === 0 && <Text size="sm" c="dimmed">This item has no diagram.</Text>}
      {sources.map((src, i) => <MermaidDiagram key={i} source={src} plan={plans[i]} />)}
    </Stack>
  )
}
