import { ActionIcon, Box, Checkbox, Group, NavLink, ScrollArea, Stack, Text, TextInput, Tooltip, UnstyledButton } from '@mantine/core'
import { IconExternalLink, IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand, IconSearch } from '@tabler/icons-react'
import { useQueries } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import type { Run, Space } from '@/api/client'
import { runsQuery, useNodes, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
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
  const visible = useMemo(() => (spaces.data ?? []).filter((s) => s.state !== 'archived').sort((a, b) => a.name.localeCompare(b.name)), [spaces.data])
  const runs = useQueries({ queries: visible.map((s) => runsQuery(s.id)) })
  const bySpace = new Map<string, Run[]>(visible.map((s, i) => [s.id, sortRuns(runs[i]?.data ?? [])]))
  const allLive = visible.flatMap((s) => (bySpace.get(s.id) ?? []).filter((r) => !isEnded(r)))
  const attention = allLive.filter((r) => runTone(r) === 'attention').length
  // The selected space: the URL's, else the first with a running agent, else the first space.
  const current = spaceId ?? visible.find((s) => (bySpace.get(s.id) ?? []).some((r) => !isEnded(r)))?.id ?? visible[0]?.id
  const q = filter.trim().toLowerCase()
  const toggleTree = () => setTree((v) => {
    try {
      localStorage.setItem('sm.workspace.tree', v ? 'closed' : 'open')
    } catch {
      /* not remembered */
    }
    return !v
  })

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
      <Box style={{ display: 'grid', gridTemplateColumns: tree ? '260px minmax(0, 1fr)' : 'minmax(0, 1fr)', gap: 'var(--mantine-spacing-md)', flex: 1, minHeight: 0 }}>
        {tree && (
          <Stack gap="xs" style={{ minHeight: 0, borderRight: '1px solid var(--mantine-color-default-border)', paddingRight: 'var(--mantine-spacing-sm)' }}>
            <Group justify="space-between">
              <Text fw={600}>Terminals</Text>
              <Tooltip label="Hide the space list">
                <ActionIcon aria-label="Hide the space list" onClick={toggleTree}><IconLayoutSidebarLeftCollapse size={16} /></ActionIcon>
              </Tooltip>
            </Group>
            <TextInput placeholder="Filter spaces and agents" leftSection={<IconSearch size={14} />} value={filter} onChange={(e) => setFilter(e.currentTarget.value)}
              aria-label="Filter spaces and agents" />
            <Checkbox size="xs" label="Running agents only" checked={runningOnly} onChange={(e) => setRunningOnly(e.currentTarget.checked)} />
            <ScrollArea style={{ flex: 1 }} type="auto">
              <Stack gap={2}>
                {visible.map((s) => (
                  <SpaceBranch key={s.id} space={s} runs={bySpace.get(s.id) ?? []} q={q} runningOnly={runningOnly} now={now}
                    active={s.id === current} selectedRun={s.id === current ? runId : undefined} onSelect={onSelect} />
                ))}
              </Stack>
            </ScrollArea>
            <Text size="xs" c="dimmed">
              {(nodes.data ?? []).map((n) => `${n.name} ${n.connected ? '●' : '○'}`).join('  ') || 'No nodes'}
            </Text>
          </Stack>
        )}
        <Stack gap="xs" style={{ minWidth: 0, minHeight: 0, overflow: 'auto' }}>
          {current ? (
            <>
              <Group justify="space-between" wrap="nowrap">
                <Group gap="xs" wrap="nowrap">
                  {!tree && (
                    <Tooltip label="Show the space list">
                      <ActionIcon aria-label="Show the space list" onClick={toggleTree}><IconLayoutSidebarLeftExpand size={16} /></ActionIcon>
                    </Tooltip>
                  )}
                  <Text fw={600}>{visible.find((s) => s.id === current)?.name}</Text>
                </Group>
                <AnchorLink to="/spaces/$spaceId" params={{ spaceId: current }} size="sm">
                  <Group gap={4}>Open space <IconExternalLink size={12} /></Group>
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
        <Text size="xs" c="dimmed" ml="auto">[ and ] switch agents</Text>
      </Group>
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
