import { Badge, Box, Group, UnstyledButton } from '@mantine/core'
import { Link, useRouterState } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { useHumanActions, useInbox, useMessages } from '@/api/queries'

type Section = 'overview' | 'inbox' | 'queue' | 'human' | 'messages' | 'knowledge' | 'memory' | 'events'
type To = '/spaces/$spaceId' | '/spaces/$spaceId/inbox' | '/spaces/$spaceId/queue' | '/spaces/$spaceId/human' | '/spaces/$spaceId/messages' | '/spaces/$spaceId/knowledge' | '/spaces/$spaceId/memory' | '/spaces/$spaceId/events'

/**
 * Section links of a space; inbox, human actions and messages show how many items wait. They are navigation links
 * (a labelled nav with aria-current), not tabs: each section is its own page.
 */
export function SpaceNav({ spaceId }: { spaceId: string }) {
  const path = useRouterState({ select: (s) => s.location.pathname })
  const fresh = useInbox(spaceId, '').data?.pages[0]?.counts.new ?? 0
  const human = useHumanActions(spaceId)
  const waiting = (human.data?.items ?? []).filter((a) => a.status === 'open' || a.status === 'in_progress').length
  const forOperators = useMessages(spaceId).data?.operators_pending ?? 0
  const active: Section = path.includes('/inbox') ? 'inbox' : path.includes('/events') ? 'events' : path.includes('/queue') ? 'queue'
    : path.includes('/human') ? 'human' : path.includes('/messages') ? 'messages' : path.includes('/memory') ? 'memory' : path.includes('/knowledge') ? 'knowledge' : 'overview'
  const count = (n: number, color?: string) => (n > 0 ? <Badge size="xs" color={color} circle={n < 10}>{n}</Badge> : null)
  const items: { key: Section; to: To; label: string; extra?: ReactNode }[] = [
    { key: 'overview', to: '/spaces/$spaceId', label: 'Overview' },
    { key: 'inbox', to: '/spaces/$spaceId/inbox', label: 'Inbox', extra: count(fresh) },
    { key: 'queue', to: '/spaces/$spaceId/queue', label: 'Queue' },
    { key: 'human', to: '/spaces/$spaceId/human', label: 'Human actions', extra: count(waiting, 'owlYellow') },
    { key: 'messages', to: '/spaces/$spaceId/messages', label: 'Messages', extra: count(forOperators, 'owlYellow') },
    { key: 'knowledge', to: '/spaces/$spaceId/knowledge', label: 'Knowledge' },
    { key: 'memory', to: '/spaces/$spaceId/memory', label: 'Memory' },
    { key: 'events', to: '/spaces/$spaceId/events', label: 'Events' },
  ]
  return (
    <Box component="nav" aria-label="Space sections" mb="md" style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
      <Group gap={0} wrap="nowrap" style={{ overflowX: 'auto' }}>
        {items.map((it) => {
          const on = it.key === active
          return (
            <UnstyledButton key={it.key} renderRoot={(props) => <Link {...props} to={it.to} params={{ spaceId }} />} aria-current={on ? 'page' : undefined}
              px="sm" py={6} fz="sm" fw={on ? 600 : 400}
              style={{ borderBottom: `2px solid ${on ? 'var(--mantine-primary-color-filled)' : 'transparent'}`, marginBottom: -1, whiteSpace: 'nowrap' }}>
              <Group gap={6} wrap="nowrap">{it.label}{it.extra}</Group>
            </UnstyledButton>
          )
        })}
      </Group>
    </Box>
  )
}
