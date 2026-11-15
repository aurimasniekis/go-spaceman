import { Badge, Box, Group, UnstyledButton } from '@mantine/core'
import { Link, useRouterState } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { useHumanActions, useInbox, useMessages, useRuns, useXSpaceRequests } from '@/api/queries'

type Section = 'overview' | 'terminals' | 'inbox' | 'queue' | 'human' | 'messages' | 'repositories' | 'knowledge' | 'memory' | 'events' | 'schedules' | 'limits' | 'cross-space'
type To = '/spaces/$spaceId' | '/spaces/$spaceId/terminals' | '/spaces/$spaceId/inbox' | '/spaces/$spaceId/queue' | '/spaces/$spaceId/human' | '/spaces/$spaceId/messages' | '/spaces/$spaceId/repositories'
  | '/spaces/$spaceId/knowledge' | '/spaces/$spaceId/memory' | '/spaces/$spaceId/events' | '/spaces/$spaceId/schedules' | '/spaces/$spaceId/limits' | '/spaces/$spaceId/cross-space'

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
  const incoming = (useXSpaceRequests(spaceId).data ?? []).filter((r) => r.side === 'incoming' && r.state === 'pending').length
  const running = (useRuns(spaceId).data ?? []).filter((r) => !['exited', 'failed', 'lost'].includes(r.observed)).length
  const active: Section = path.includes('/terminals') || path.includes('/runs/') ? 'terminals' : path.includes('/inbox') ? 'inbox' : path.includes('/events') ? 'events' : path.includes('/queue') ? 'queue'
    : path.includes('/schedules') ? 'schedules' : path.includes('/limits') ? 'limits' : path.includes('/repositories') ? 'repositories'
    : path.includes('/human') ? 'human' : path.includes('/messages') ? 'messages' : path.includes('/memory') ? 'memory' : path.includes('/knowledge') ? 'knowledge' : path.includes('/cross-space') ? 'cross-space' : 'overview'
  const count = (n: number, color?: string) => (n > 0 ? <Badge size="xs" color={color} circle={n < 10}>{n}</Badge> : null)
  const items: { key: Section; to: To; label: string; extra?: ReactNode }[] = [
    { key: 'overview', to: '/spaces/$spaceId', label: 'Overview' },
    { key: 'terminals', to: '/spaces/$spaceId/terminals', label: 'Terminals', extra: count(running, 'owlGreen') },
    { key: 'inbox', to: '/spaces/$spaceId/inbox', label: 'Inbox', extra: count(fresh) },
    { key: 'queue', to: '/spaces/$spaceId/queue', label: 'Queue' },
    { key: 'human', to: '/spaces/$spaceId/human', label: 'Human actions', extra: count(waiting, 'owlYellow') },
    { key: 'messages', to: '/spaces/$spaceId/messages', label: 'Messages', extra: count(forOperators, 'owlYellow') },
    { key: 'repositories', to: '/spaces/$spaceId/repositories', label: 'Repositories' },
    { key: 'knowledge', to: '/spaces/$spaceId/knowledge', label: 'Knowledge' },
    { key: 'memory', to: '/spaces/$spaceId/memory', label: 'Memory' },
    { key: 'events', to: '/spaces/$spaceId/events', label: 'Events' },
    { key: 'schedules', to: '/spaces/$spaceId/schedules', label: 'Schedules' },
    { key: 'limits', to: '/spaces/$spaceId/limits', label: 'Limits' },
    { key: 'cross-space', to: '/spaces/$spaceId/cross-space', label: 'Cross-space', extra: count(incoming, 'owlYellow') },
  ]
  return (
    <Box component="nav" aria-label="Space sections" mb="md">
      {/* The baseline and the active underline are inset shadows, not borders with a negative margin: a link that sticks
          out of the row by even a pixel makes the scrolling row show a vertical scrollbar. The active link's shadow paints
          over the row's. */}
      <Group gap={0} wrap="nowrap" style={{ overflowX: 'auto', scrollbarWidth: 'thin', boxShadow: 'inset 0 -1px 0 var(--mantine-color-default-border)' }}>
        {items.map((it) => {
          const on = it.key === active
          return (
            <UnstyledButton key={it.key} renderRoot={(props) => <Link {...props} to={it.to} params={{ spaceId }} />} aria-current={on ? 'page' : undefined}
              px={{ base: 10, xl: 'sm' }} pt={6} pb={8} fz="sm" fw={on ? 600 : 400}
              style={{ boxShadow: on ? 'inset 0 -2px 0 var(--mantine-primary-color-filled)' : undefined, whiteSpace: 'nowrap' }}>
              <Group gap={6} wrap="nowrap">{it.label}{it.extra}</Group>
            </UnstyledButton>
          )
        })}
      </Group>
    </Box>
  )
}
