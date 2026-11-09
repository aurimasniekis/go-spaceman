import { ActionIcon, Box, Button, Checkbox, Drawer, Group, NavLink, ScrollArea, Stack, Text, TextInput, Tooltip, UnstyledButton } from '@mantine/core'
import { IconChevronDown, IconExternalLink, IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand, IconSearch } from '@tabler/icons-react'
import { useQueries } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import type { Run, Space } from '@/api/client'
import { runsQuery, useNodes, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { useNarrow } from '@/lib/media'
import { isEnded, RunDot, runLabels, runStateText, runTone, sortRuns, type Tone } from './runInfo'
import { useNow } from './RunWorkbench'
import { SpaceTerminals } from './SpaceTerminals'

const WORST: Tone[] = ['failed', 'attention', 'offline', 'working', 'idle', 'starting', 'ended']

function readTree(): boolean {
  try {
    return localStorage.getItem('sm.workspace.tree') !== 'closed'
  } catch {
    return true
  }
}

/**
 * The Terminals workspace (UX review §4.4), in the spirit of herdr: a sidebar with every space and its agents, and the
 * selected space's agents as tabs with the selected agent's terminal. Space and agent are in the URL, so a view can be
 * shared and restored.
 */
export function Workspace({ spaceId, runId, mode, onSelect, onMode }: {
  spaceId?: string
  runId?: string
  mode?: 'live' | 'replay'
  onSelect: (spaceId: string, runId?: string) => void
  onMode: (m: 'live' | 'replay') => void
}) {
  const spaces = useSpaces()
  const nodes = useNodes()
  const now = useNow(5000)
  const [filter, setFilter] = useState('')
  const [runningOnly, setRunningOnly] = useState(false)
  const [tree, setTree] = useState(readTree)
  // On a phone the space list is a drawer, closed until asked for, and closes again once a space or agent is chosen.
  const narrow = useNarrow()
  const [drawer, setDrawer] = useState(false)
  const choose = (s: string, r?: string) => {
    setDrawer(false)
    onSelect(s, r)
  }
  const visible = useMemo(() => (spaces.data ?? []).filter((s) => s.state !== 'archived').sort((a, b) => a.name.localeCompare(b.name)), [spaces.data])
  const runs = useQueries({ queries: visible.map((s) => runsQuery(s.id)) })
  const bySpace = new Map<string, Run[]>(visible.map((s, i) => [s.id, sortRuns(runs[i]?.data ?? [])]))
  const allLive = visible.flatMap((s) => (bySpace.get(s.id) ?? []).filter((r) => !isEnded(r)))
  const attention = allLive.filter((r) => runTone(r) === 'attention').length
  // The selected space: the URL's, else the first with a running agent, else the first space.
  const current = spaceId ?? visible.find((s) => (bySpace.get(s.id) ?? []).some((r) => !isEnded(r)))?.id ?? visible[0]?.id
  const currentName = visible.find((s) => s.id === current)?.name
  const q = filter.trim().toLowerCase()
  const toggleTree = () => setTree((v) => {
    try {
      localStorage.setItem('sm.workspace.tree', v ? 'closed' : 'open')
    } catch {
      /* not remembered */
    }
    return !v
  })

  const sidebar = (
    <>
      {!narrow && (
        <Group justify="space-between">
          <Text fw={600}>Terminals</Text>
          <Tooltip label="Hide the space list">
            <ActionIcon aria-label="Hide the space list" onClick={toggleTree}><IconLayoutSidebarLeftCollapse size={16} /></ActionIcon>
          </Tooltip>
        </Group>
      )}
      <TextInput placeholder="Filter spaces and agents" leftSection={<IconSearch size={14} />} value={filter} onChange={(e) => setFilter(e.currentTarget.value)}
        aria-label="Filter spaces and agents" />
      <Checkbox size="xs" label="Running agents only" checked={runningOnly} onChange={(e) => setRunningOnly(e.currentTarget.checked)} />
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap={2}>
          {visible.map((s) => (
            <SpaceBranch key={s.id} space={s} runs={bySpace.get(s.id) ?? []} q={q} runningOnly={runningOnly} now={now}
              active={s.id === current} selectedRun={s.id === current ? runId : undefined} onSelect={choose} />
          ))}
        </Stack>
      </ScrollArea>
      <Text size="xs" c="dimmed">
        {(nodes.data ?? []).map((n) => `${n.name} ${n.connected ? '●' : '○'}`).join('  ') || 'No nodes'}
      </Text>
    </>
  )

  if (spaces.isSuccess && visible.length === 0) {
    return (
      <Stack align="center" py="xl" gap="xs">
        <Text fw={600}>No spaces yet</Text>
        <Text size="sm" c="dimmed">A space is a team of agents on one node. <AnchorLink to="/spaces" size="sm">Create a space</AnchorLink></Text>
      </Stack>
    )
  }
  return (
    <Stack gap={0} style={{ height: 'calc(100vh - 72px)' }}>
      <Box style={{ display: 'grid', gridTemplateColumns: tree && !narrow ? '260px minmax(0, 1fr)' : 'minmax(0, 1fr)', gap: 'var(--mantine-spacing-md)', flex: 1, minHeight: 0 }}>
        {tree && !narrow && (
          <Stack gap="xs" style={{ minHeight: 0, borderRight: '1px solid var(--mantine-color-default-border)', paddingRight: 'var(--mantine-spacing-sm)' }}>
            {sidebar}
          </Stack>
        )}
        {/* Scrolls when the terminal (70vh) and its controls are taller than the column, by whatever the screen height leaves. */}
        <Stack gap="xs" data-scroll-region style={{ minWidth: 0, minHeight: 0, overflow: 'auto' }}>
          {current ? (
            <>
              <Group justify="space-between" wrap="nowrap">
                {narrow ? (
                  // One control for the space name and the space list, instead of a second header with its own toggle.
                  <Button variant="subtle" color="gray" px={4} fw={600} size="sm" aria-label={`Space: ${currentName ?? ''}. Choose a space or agent`}
                    leftSection={<IconLayoutSidebarLeftExpand size={16} />} rightSection={<IconChevronDown size={14} />} onClick={() => setDrawer(true)}
                    styles={{ root: { minWidth: 0, maxWidth: '100%' }, inner: { minWidth: 0 }, label: { minWidth: 0 } }}>
                    {/* Mantine's label is a flex box, where text-overflow has no effect: truncate an inner block. */}
                    <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{currentName}</span>
                  </Button>
                ) : (
                  <Group gap="xs" wrap="nowrap" style={{ minWidth: 0 }}>
                    {!tree && (
                      <Tooltip label="Show the space list">
                        <ActionIcon aria-label="Show the space list" onClick={toggleTree}><IconLayoutSidebarLeftExpand size={16} /></ActionIcon>
                      </Tooltip>
                    )}
                    <Text fw={600} truncate>{currentName}</Text>
                  </Group>
                )}
                <AnchorLink to="/spaces/$spaceId" params={{ spaceId: current }} size="sm" style={{ flexShrink: 0 }}>
                  <Group gap={4} wrap="nowrap">Open space <IconExternalLink size={12} /></Group>
                </AnchorLink>
              </Group>
              <SpaceTerminals key={current} spaceId={current} runId={runId} mode={mode} onSelect={(r) => onSelect(current, r)} onMode={onMode} />
            </>
          ) : <Text size="sm" c="dimmed">Choose a space.</Text>}
        </Stack>
      </Box>
      <Group gap="md" px="xs" py={4} mt="xs" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
        <Text size="xs" c="dimmed">{allLive.length} {allLive.length === 1 ? 'agent' : 'agents'} running in {visible.length} {visible.length === 1 ? 'space' : 'spaces'}</Text>
        {attention > 0 && <AnchorLink to="/human" size="xs" c="owlYellow">{attention} waiting for input →</AnchorLink>}
        {!narrow && <Text size="xs" c="dimmed" ml="auto">[ and ] switch agents</Text>}
      </Group>
      {narrow && (
        <Drawer opened={drawer} onClose={() => setDrawer(false)} title="Terminals" position="left" size="85%">
          <Stack gap="xs">{sidebar}</Stack>
        </Drawer>
      )}
    </Stack>
  )
}

function SpaceBranch({ space: s, runs, q, runningOnly, now, active, selectedRun, onSelect }: {
  space: Space
  runs: Run[]
  q: string
  runningOnly: boolean
  now: number
  active: boolean
  selectedRun?: string
  onSelect: (spaceId: string, runId?: string) => void
}) {
  const live = runs.filter((r) => !isEnded(r))
  const spaceMatch = !q || s.name.toLowerCase().includes(q) || s.slug.includes(q)
  const shown = live.filter((r) => spaceMatch || r.role.toLowerCase().includes(q))
  const labels = runLabels(live)
  if (!spaceMatch && shown.length === 0) return null
  if (runningOnly && live.length === 0) return null
  const worst = WORST.find((t) => live.some((r) => runTone(r) === t))
  const dot = worst ? <RunDot run={live.find((r) => runTone(r) === worst)!} /> : <Box component="span" w={8} h={8} style={{ borderRadius: '50%', border: '1px solid var(--mantine-color-dimmed)' }} />
  return (
    <NavLink label={<Text size="sm" fw={active ? 600 : 500} truncate>{s.name}</Text>} leftSection={dot} active={active && !selectedRun}
      description={s.state === 'paused' ? 'paused' : live.length === 0 ? 'nothing running' : `${live.length} running`}
      defaultOpened={active || live.length > 0} childrenOffset={14} onClick={() => onSelect(s.id)} styles={{ root: { borderRadius: 'var(--mantine-radius-sm)' } }}>
      {shown.map((r) => (
        <UnstyledButton key={r.id} onClick={() => onSelect(s.id, r.id)} px="xs" py={4} aria-current={r.id === selectedRun ? 'true' : undefined}
          style={{ display: 'block', width: '100%', borderRadius: 'var(--mantine-radius-sm)', background: r.id === selectedRun ? 'var(--mantine-primary-color-light)' : undefined }}>
          <Group gap={6} wrap="nowrap">
            <RunDot run={r} />
            <Text size="sm" truncate style={{ flex: 1 }}>{labels.get(r.id)}</Text>
            <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>{runStateText(r, now).split(' · ')[0]}</Text>
          </Group>
        </UnstyledButton>
      ))}
    </NavLink>
  )
}
