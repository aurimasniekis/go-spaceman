import { Badge, Box, Group, Skeleton, Title, UnstyledButton } from '@mantine/core'
import { Link, useRouterState } from '@tanstack/react-router'
import { useHumanActions, useInbox, useMessages, useRuns, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { SpaceState } from '@/components/StatusBadge'
import { isEnded } from '@/features/runs/runInfo'

type To = '/spaces/$spaceId' | '/spaces/$spaceId/runs' | '/spaces/$spaceId/inbox' | '/spaces/$spaceId/queue' | '/spaces/$spaceId/human' | '/spaces/$spaceId/messages'

/** The space's name and state, with a way back to the start page. No space actions: they are web-only. */
export function TgSpaceHeader({ spaceId }: { spaceId: string }) {
  const space = useSpace(spaceId)
  if (!space.data) return <Group mb="sm"><Skeleton h={28} w={200} /></Group>
  return (
    <Group justify="space-between" mb="xs" wrap="nowrap" gap="xs" data-testid="space-header">
      <Box style={{ minWidth: 0 }}>
        <AnchorLink to="/" size="xs">‹ All spaces</AnchorLink>
        <Title order={4} lineClamp={1}>{space.data.name}</Title>
      </Box>
      <SpaceState space={space.data} />
    </Group>
  )
}

/** The sections a Telegram link can reach, as a scrolling row of links (the web's section list holds web-only pages). */
export function TgSpaceNav({ spaceId }: { spaceId: string }) {
  const path = useRouterState({ select: (s) => s.location.pathname })
  const fresh = useInbox(spaceId, '').data?.pages[0]?.counts.new ?? 0
  const waiting = (useHumanActions(spaceId).data?.items ?? []).filter((a) => a.status === 'open' || a.status === 'in_progress').length
  const forOperators = useMessages(spaceId).data?.operators_pending ?? 0
  const running = (useRuns(spaceId).data ?? []).filter((r) => !isEnded(r)).length
  const section = (s: string) => path.includes(`/${s}`)
  const active = section('runs') ? 'runs' : section('inbox') ? 'inbox' : section('queue') ? 'queue' : section('human') ? 'human' : section('messages') ? 'messages' : 'overview'
  const count = (n: number) => (n > 0 ? <Badge size="xs" circle={n < 10}>{n}</Badge> : null)
  const items: { key: string; to: To; label: string; extra?: React.ReactNode }[] = [
    { key: 'overview', to: '/spaces/$spaceId', label: 'Overview' },
    { key: 'queue', to: '/spaces/$spaceId/queue', label: 'Queue' },
    { key: 'human', to: '/spaces/$spaceId/human', label: 'Needs you', extra: count(waiting) },
    { key: 'inbox', to: '/spaces/$spaceId/inbox', label: 'Inbox', extra: count(fresh) },
    { key: 'runs', to: '/spaces/$spaceId/runs', label: 'Runs', extra: count(running) },
    { key: 'messages', to: '/spaces/$spaceId/messages', label: 'Messages', extra: count(forOperators) },
  ]
  return (
    <Box component="nav" aria-label="Space sections" mb="md">
      <Group gap={0} wrap="nowrap" style={{ overflowX: 'auto', scrollbarWidth: 'none', boxShadow: 'inset 0 -1px 0 var(--mantine-color-default-border)' }}>
        {items.map((it) => {
          const on = it.key === active
          return (
            <UnstyledButton key={it.key} renderRoot={(props) => <Link {...props} to={it.to} params={{ spaceId }} />} aria-current={on ? 'page' : undefined}
              px={12} pt={10} pb={10} fz="sm" fw={on ? 600 : 400}
              style={{ boxShadow: on ? 'inset 0 -2px 0 var(--mantine-primary-color-filled)' : undefined, whiteSpace: 'nowrap' }}>
              <Group gap={6} wrap="nowrap">{it.label}{it.extra}</Group>
            </UnstyledButton>
          )
        })}
      </Group>
    </Box>
  )
}
