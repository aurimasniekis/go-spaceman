import { ActionIcon, Alert, AppShell, Badge, Button, Group, Menu, NavLink, ScrollArea, Stack, Text, Tooltip, UnstyledButton, useComputedColorScheme, useMantineColorScheme, VisuallyHidden } from '@mantine/core'
import { useDisclosure, useLocalStorage } from '@mantine/hooks'
import { IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand, IconLogout, IconMoon, IconSun, IconUserCircle } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useRouterState } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { api, check, unwrap } from '@/api/client'
import { useHumanPending, useMe } from '@/api/queries'
import { useWatch } from '@/api/watch'
import { RestoreBanner } from '@/features/backup/RestoreBanner'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'
import { visibleNav } from '@/lib/nav'
import { notifyError } from '@/lib/notify'
import { Logo } from './Logo'

function LiveIndicator() {
  const state = useWatch(true)
  const color = state === 'live' ? 'owlGreen' : state === 'connecting' ? 'owlYellow' : 'owlRed'
  const label = state === 'live' ? 'Live updates connected' : state === 'connecting' ? 'Connecting to live updates…' : 'Live updates offline'
  return (
    <Tooltip label={label}>
      <Badge color={color} variant="dot" size="sm" aria-label={label}>{state}</Badge>
    </Tooltip>
  )
}

/**
 * Raised after a break-glass recovery login (`spaceman admin recover`) until an administrator acknowledges it, so an
 * unexpected recovery cannot go unnoticed.
 */
function BreakGlassBanner() {
  const qc = useQueryClient()
  const status = useQuery({ queryKey: ['break-glass'], queryFn: async () => unwrap(await api.GET('/admin/break-glass')), refetchInterval: 60_000 })
  if (!status.data?.alert) return null
  const ack = async () => {
    try {
      check(await api.POST('/admin/break-glass/acknowledge'))
      void qc.invalidateQueries({ queryKey: ['break-glass'] })
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <Alert color="owlRed" title="Break-glass recovery was used" mb="md">
      <Group justify="space-between" wrap="nowrap">
        <span>An account was recovered with <code>spaceman admin recover</code> at {datetime(status.data.at)}. Check the audit log (<code>breakglass.login</code>), then acknowledge.</span>
        <Button color="owlRed" variant="light" onClick={() => void ack()}>Acknowledge</Button>
      </Group>
    </Alert>
  )
}

function UserMenu() {
  const me = useMe().data
  const qc = useQueryClient()
  const name = me?.root ? 'root token' : (me?.user?.display_name ?? '—')
  return (
    <Menu position="bottom-end" width={200}>
      <Menu.Target>
        <UnstyledButton aria-label={`${name}, account menu`}>
          <Group gap={6} wrap="nowrap">
            <IconUserCircle size={18} />
            <Text size="sm" visibleFrom="sm">{name}</Text>
          </Group>
        </UnstyledButton>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>{me?.user?.email ?? name}</Menu.Label>
        <Menu.Item component={Link} to="/account" leftSection={<IconUserCircle size={14} />}>Account</Menu.Item>
        <Menu.Item
          leftSection={<IconLogout size={14} />}
          onClick={async () => {
            await api.POST('/auth/logout')
            qc.clear()
            window.location.assign('/login')
          }}
        >
          Sign out
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  )
}

export function AppLayout({ children }: { children: ReactNode }) {
  const [collapsed, setCollapsed] = useLocalStorage({ key: 'spaceman.sidebar.collapsed', defaultValue: false })
  const [mobileOpen, { toggle: toggleMobile, close: closeMobile }] = useDisclosure(false)
  const { setColorScheme } = useMantineColorScheme()
  const scheme = useComputedColorScheme('dark')
  const path = useRouterState({ select: (s) => s.location.pathname })
  const me = useMe().data
  const items = visibleNav(me)
  const pending = useHumanPending(!!me && !me.limited).data?.total ?? 0
  const width = collapsed ? 52 : 200
  const groups = ['Work', 'Fleet', 'Configuration', 'Integrations', 'Access'] as const

  return (
    <AppShell header={{ height: 40 }} navbar={{ width, breakpoint: 'sm', collapsed: { mobile: !mobileOpen } }} padding="md">
      <AppShell.Header px="sm">
        <Group h="100%" justify="space-between" wrap="nowrap">
          <Group gap="xs" wrap="nowrap">
            <ActionIcon hiddenFrom="sm" onClick={toggleMobile} aria-label="Toggle navigation">
              <IconLayoutSidebarLeftExpand size={16} />
            </ActionIcon>
            <Logo />
            <Text fw={600} size="md">SpaceMan</Text>
          </Group>
          <Group gap="sm" wrap="nowrap">
            <LiveIndicator />
            <Tooltip label={scheme === 'dark' ? 'Light Owl' : 'Night Owl'}>
              <ActionIcon onClick={() => setColorScheme(scheme === 'dark' ? 'light' : 'dark')} aria-label="Toggle color scheme">
                {scheme === 'dark' ? <IconSun size={16} /> : <IconMoon size={16} />}
              </ActionIcon>
            </Tooltip>
            <UserMenu />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p={6} aria-label="Main navigation">
        <AppShell.Section grow component={ScrollArea}>
          <Stack gap={8}>
            {groups.map((g) => (
              <Stack key={g} gap={1}>
                {!collapsed && items.some((n) => n.group === g) && <Text size="xs" c="dimmed" fw={600} tt="uppercase" px={8} pt={4}>{g}</Text>}
                {items
                  .filter((n) => n.group === g)
                  .map((n) => {
                    const active = n.to === '/' ? path === '/' : path === n.to || path.startsWith(`${n.to}/`)
                    const link = (
                      <NavLink key={n.to} component={Link} to={n.to} active={active} label={collapsed ? undefined : n.label}
                        leftSection={<n.icon size={16} stroke={1.6} />} onClick={closeMobile}
                        // Collapsed links show no text, so they are named; expanded ones are named by their visible text (axe).
                        aria-label={collapsed ? (n.pendingBadge && pending > 0 ? `${n.label}, ${pending} pending` : n.label) : undefined}
                        rightSection={n.pendingBadge && pending > 0 && !collapsed ? <Badge size="xs" color="owlYellow" circle={pending < 10}>{pending}<VisuallyHidden> pending</VisuallyHidden></Badge> : undefined} />
                    )
                    return collapsed ? <Tooltip key={n.to} label={n.label} position="right">{link}</Tooltip> : link
                  })}
              </Stack>
            ))}
          </Stack>
        </AppShell.Section>
        <AppShell.Section visibleFrom="sm">
          <NavLink onClick={() => setCollapsed(!collapsed)} label={collapsed ? undefined : 'Collapse'} aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
            leftSection={collapsed ? <IconLayoutSidebarLeftExpand size={16} /> : <IconLayoutSidebarLeftCollapse size={16} />} />
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>
        {hasGlobal(me, 'admin') && <BreakGlassBanner />}
        {me && <RestoreBanner canAcknowledge={hasGlobal(me, 'admin')} />}
        {children}
      </AppShell.Main>
    </AppShell>
  )
}
