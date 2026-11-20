import { Badge, Card, Group, Stack, Text } from '@mantine/core'
import { IconCircleCheck } from '@tabler/icons-react'
import type { AttentionItem } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { ago } from '@/lib/format'
import { itemLink, topItems } from './targets'

/**
 * The top items that need a person (plan §2.2, Q2): act before watch, newest first, each linking to where it is
 * handled, and "See all (N)" to the full list. The total counts what the page lists (items need a person, §2.3).
 * Without data (loading, or the read failed) it says so: it never claims that nothing needs attention, nor a count of 0.
 */
export function AttentionTop({ items, total, spaceName, loading, error }: { items?: AttentionItem[]; total?: number; spaceName: (id: string) => string; loading?: boolean; error?: unknown }) {
  const top = topItems(items ?? [])
  const known = items !== undefined && !error
  return (
    <Card data-testid="attention-top">
      <Group justify="space-between" mb={6} wrap="nowrap">
        <Text fw={600}>Needs attention</Text>
        <AnchorLink to="/attention" size="sm">{known && total !== undefined ? `See all (${total})` : 'See all'}</AnchorLink>
      </Group>
      {error ? <ErrorAlert error={error} title="Attention is unavailable" /> : !known ? (
        <Text size="sm" c="dimmed" role="status">{loading ? 'Loading…' : '—'}</Text>
      ) : top.length === 0 ? (
        <Group gap={6}>
          <IconCircleCheck size={16} aria-hidden />
          <Text size="sm" c="dimmed">Nothing needs you right now.</Text>
        </Group>
      ) : (
        <Stack gap={8} component="ul" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          {top.map((it) => {
            const l = itemLink(it)
            return (
              <li key={`${it.kind}-${it.id}`} style={{ minWidth: 0 }}>
                <Group gap="xs" wrap="nowrap" align="flex-start">
                  <Badge size="sm" variant="light" color={it.severity === 'act' ? 'owlRed' : 'owlYellow'} style={{ flexShrink: 0 }}>{it.severity === 'act' ? 'Act now' : 'Watch'}</Badge>
                  <div style={{ minWidth: 0, flex: 1 }}>
                    <AnchorLink {...l} size="sm" style={{ overflowWrap: 'anywhere' }}>{it.title}</AnchorLink>
                    <Text size="xs" c="dimmed">{spaceName(it.space_id)} · {ago(it.since)}</Text>
                  </div>
                </Group>
              </li>
            )
          })}
        </Stack>
      )}
    </Card>
  )
}
