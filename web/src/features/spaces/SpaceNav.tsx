import { Badge, Group, Tabs } from '@mantine/core'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useInbox } from '@/api/queries'

/** Section links of a space (overview, inbox); more sections arrive with their features. */
export function SpaceNav({ spaceId }: { spaceId: string }) {
  const navigate = useNavigate()
  const path = useRouterState({ select: (s) => s.location.pathname })
  const inbox = useInbox(spaceId, '')
  const fresh = inbox.data?.pages[0]?.counts.new ?? 0
  const value = path.includes('/inbox') ? 'inbox' : path.includes('/events') ? 'events' : path.includes('/queue') ? 'queue' : 'overview'
  return (
    <Tabs value={value} mb="md" onChange={(v) =>
      void navigate(
        v === 'inbox' ? { to: '/spaces/$spaceId/inbox', params: { spaceId } } : v === 'queue' ? { to: '/spaces/$spaceId/queue', params: { spaceId } } : v === 'events' ? { to: '/spaces/$spaceId/events', params: { spaceId } } : { to: '/spaces/$spaceId', params: { spaceId } },
      )
    }>
      <Tabs.List>
        <Tabs.Tab value="overview">Overview</Tabs.Tab>
        <Tabs.Tab value="inbox">
          <Group gap={6} wrap="nowrap">Inbox{fresh > 0 && <Badge size="xs" circle={fresh < 10}>{fresh}</Badge>}</Group>
        </Tabs.Tab>
        <Tabs.Tab value="queue">Queue</Tabs.Tab>
        <Tabs.Tab value="events">Events</Tabs.Tab>
      </Tabs.List>
    </Tabs>
  )
}
