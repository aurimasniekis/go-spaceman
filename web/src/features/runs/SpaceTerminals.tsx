import { Alert, Box, Button, Group, Menu, ScrollArea, Stack, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { IconChevronDown, IconRocket } from '@tabler/icons-react'
import { useEffect, useMemo, useState } from 'react'
import type { Run } from '@/api/client'
import { useRun, useRuns, useSpace } from '@/api/queries'
import { RunWorkbench, useNow } from './RunWorkbench'
import { isEnded, RunDot, runLabels, runStateText, runTitle, sortRuns } from './runInfo'

const DAY = 24 * 3600 * 1000

/**
 * A space's agents as tabs (UX review §4.3): every live agent has a tab with its status dot, title and state; ended runs
 * of the last day are in the "Ended" menu, and one chosen there opens as a temporary tab. The selected agent's
 * workbench (terminal, message box, inspector) fills the rest. The selection is in the URL (?run=).
 */
export function SpaceTerminals({ spaceId, runId, mode, onSelect, onMode }: {
  spaceId: string
  runId?: string
  mode?: 'live' | 'replay'
  onSelect: (runId: string) => void
  onMode: (m: 'live' | 'replay') => void
}) {
  const runs = useRuns(spaceId)
  const space = useSpace(spaceId)
  const now = useNow(5000)
  const all = useMemo(() => sortRuns(runs.data ?? []), [runs.data])
  const live = all.filter((r) => !isEnded(r))
  const ended = all.filter((r) => isEnded(r) && now - Date.parse(r.ended_at ?? r.created_at) < DAY)
  const [pinnedEnded, setPinnedEnded] = useState<string | null>(null)
  const selected = runId ?? live[0]?.id ?? ended[0]?.id
  useEffect(() => {
    if (selected && runId === undefined) onSelect(selected)
  }, [selected, runId, onSelect])
  useEffect(() => {
    if (runId && !live.some((r) => r.id === runId)) setPinnedEnded(runId)
  }, [runId, live])
  // Keyboard: [ and ] move between tabs (not while typing, and never inside a taken-over terminal: keys go to it).
  const tabs = useMemo(() => {
    const t = [...live]
    const extra = pinnedEnded ? all.find((r) => r.id === pinnedEnded) : undefined
    if (extra && !t.some((r) => r.id === extra.id)) t.push(extra)
    return t
  }, [live, all, pinnedEnded])
  // Agents with the same title (two kb-support sessions) are numbered in start order so their tabs differ.
  const labels = useMemo(() => runLabels(tabs), [tabs])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      if (!el || el.closest('input, textarea, [contenteditable], .sm-terminal')) return
      if (e.key !== '[' && e.key !== ']') return
      const i = tabs.findIndex((r) => r.id === selected)
      const next = tabs[(i + (e.key === ']' ? 1 : tabs.length - 1)) % tabs.length]
      if (next) onSelect(next.id)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [tabs, selected, onSelect])

  if (runs.isSuccess && all.length === 0) {
    return (
      <Stack align="center" gap="sm" py="xl">
        <Text fw={600}>No agents have run here yet</Text>
        <Text size="sm" c="dimmed" ta="center" maw={460}>Control coordinates the space: it triages the inbox, plans tasks and starts workers. Use Launch run above to start it.</Text>
      </Stack>
    )
  }
  return (
    <Stack gap="sm">
      {space.data?.state === 'paused' && (
        <Alert color="owlYellow" p="xs">The space is paused ({space.data.pause_mode}): {space.data.pause_mode === 'hold' ? 'agents keep running but get no new work.' : 'its agents were stopped.'}</Alert>
      )}
      {live.length === 0 && ended.length > 0 && (
        <Alert color="gray" p="xs" icon={<IconRocket size={16} />}>No agent is running. The tabs below show the last runs; launch control to start again.</Alert>
      )}
      <Group gap={0} wrap="nowrap" align="stretch" style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
        <ScrollArea type="auto" scrollbarSize={6} style={{ flex: 1 }}>
          <Group gap={0} wrap="nowrap" role="tablist" aria-label="Agents">
            {tabs.map((r) => <AgentTab key={r.id} run={r} label={labels.get(r.id) ?? runTitle(r)} active={r.id === selected} now={now} onClick={() => onSelect(r.id)} />)}
          </Group>
        </ScrollArea>
        {ended.length > 0 && (
          <Menu position="bottom-end">
            <Menu.Target>
              <Button variant="subtle" color="gray" size="xs" my="auto" rightSection={<IconChevronDown size={12} />}>Ended ({ended.length})</Button>
            </Menu.Target>
            <Menu.Dropdown>
              {ended.map((r) => (
                <Menu.Item key={r.id} leftSection={<RunDot run={r} />} onClick={() => { setPinnedEnded(r.id); onSelect(r.id) }}>
                  <Text size="sm">{runTitle(r)}</Text>
                  <Text size="xs" c="dimmed">{runStateText(r, now)}</Text>
                </Menu.Item>
              ))}
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>
      {selected && <SelectedRun spaceId={spaceId} runId={selected} mode={mode} onMode={onMode} />}
    </Stack>
  )
}

function AgentTab({ run: r, label, active, now, onClick }: { run: Run; label: string; active: boolean; now: number; onClick: () => void }) {
  return (
    <Tooltip label={`${r.role} · ${r.id}${r.started_at ? ` · started ${new Date(r.started_at).toLocaleTimeString()}` : ''}`} openDelay={600}>
    <UnstyledButton role="tab" aria-selected={active} onClick={onClick} px="sm" py={6}
      style={{ borderBottom: `2px solid ${active ? 'var(--mantine-primary-color-filled)' : 'transparent'}`, marginBottom: -1, minWidth: 150, maxWidth: 240,
        background: active ? 'var(--sm-surface)' : undefined }}>
      <Group gap={6} wrap="nowrap">
        <RunDot run={r} />
        <Text size="sm" fw={active ? 600 : 500} truncate>{label}</Text>
      </Group>
      <Text size="xs" c="dimmed" truncate pl={14}>{runStateText(r, now)}</Text>
    </UnstyledButton>
    </Tooltip>
  )
}

function SelectedRun({ spaceId, runId, mode, onMode }: { spaceId: string; runId: string; mode?: 'live' | 'replay'; onMode: (m: 'live' | 'replay') => void }) {
  const run = useRun(spaceId, runId)
  if (!run.data) return <Box h={200} />
  const m = mode ?? (isEnded(run.data) ? 'replay' : 'live')
  return <RunWorkbench spaceId={spaceId} run={run.data} mode={isEnded(run.data) ? 'replay' : m} onMode={onMode} inspectorDefault={false} />
}
