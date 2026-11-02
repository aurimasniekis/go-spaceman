import { Badge, Group, Tabs } from '@mantine/core'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useHumanActions, useInbox, useMessages } from '@/api/queries'

/** Section links of a space; inbox and human actions show how many items wait. */
export function SpaceNav({ spaceId }: { spaceId: string }) {
  const navigate = useNavigate()
  const path = useRouterState({ select: (s) => s.location.pathname })
  const inbox = useInbox(spaceId, '')
  const fresh = inbox.data?.pages[0]?.counts.new ?? 0
  const human = useHumanActions(spaceId)
  const waiting = (human.data?.items ?? []).filter((a) => a.status === 'open' || a.status === 'in_progress').length
  const forOperators = useMessages(spaceId).data?.operators_pending ?? 0
  const value = path.includes('/inbox') ? 'inbox' : path.includes('/events') ? 'events' : path.includes('/queue') ? 'queue' : path.includes('/human') ? 'human' : path.includes('/messages') ? 'messages' : 'overview'
  const to = {
    overview: { to: '/spaces/$spaceId', params: { spaceId } },
    inbox: { to: '/spaces/$spaceId/inbox', params: { spaceId } },
    queue: { to: '/spaces/$spaceId/queue', params: { spaceId } },
    human: { to: '/spaces/$spaceId/human', params: { spaceId } },
    messages: { to: '/spaces/$spaceId/messages', params: { spaceId } },
    events: { to: '/spaces/$spaceId/events', params: { spaceId } },
  } as const
  return (
    <Tabs value={value} mb="md" onChange={(v) => void navigate(to[(v ?? 'overview') as keyof typeof to])}>
      <Tabs.List>
        <Tabs.Tab value="overview">Overview</Tabs.Tab>
        <Tabs.Tab value="inbox">
          <Group gap={6} wrap="nowrap">Inbox{fresh > 0 && <Badge size="xs" circle={fresh < 10}>{fresh}</Badge>}</Group>
        </Tabs.Tab>
        <Tabs.Tab value="queue">Queue</Tabs.Tab>
        <Tabs.Tab value="human">
          <Group gap={6} wrap="nowrap">Human actions{waiting > 0 && <Badge size="xs" color="owlYellow" circle={waiting < 10}>{waiting}</Badge>}</Group>
        </Tabs.Tab>
        <Tabs.Tab value="messages">
          <Group gap={6} wrap="nowrap">Messages{forOperators > 0 && <Badge size="xs" color="owlYellow" circle={forOperators < 10}>{forOperators}</Badge>}</Group>
        </Tabs.Tab>
        <Tabs.Tab value="events">Events</Tabs.Tab>
      </Tabs.List>
    </Tabs>
  )
}
