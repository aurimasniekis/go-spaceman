import { Accordion, ActionIcon, Alert, Badge, Box, Button, CopyButton, Group, Menu, ScrollArea, SegmentedControl, Stack, Text, Title, Tooltip } from '@mantine/core'
import {
  IconCheck, IconChevronDown, IconCopy, IconLayoutSidebarRightCollapse, IconLayoutSidebarRightExpand, IconPlayerStop, IconSend,
} from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState, type ReactNode } from 'react'
import { api, check, unwrap, type Run } from '@/api/client'
import { keys, useHumanActions, useMe, useNodes, useQueue, useSpaceAccounts } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { useAppForm } from '@/components/form/appForm'
import { holderRef, TakeoverPanel, useTakeoverInput } from '@/features/terminal/Takeover'
import { resizeBlocker } from '@/features/terminal/TerminalSize'
import { TerminalView } from '@/features/terminal/TerminalView'
import { atLeast, roleIn } from '@/lib/access'
import { useNarrow } from '@/lib/media'
import { datetime, isSet } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { harnessLabel, isEnded, kindLabel, RunDot, runDuration, runStateText } from './runInfo'
import { type Dependency, dependencyText, runHarnessVersion } from './harnessVersion'

/** A clock that ticks every second, for live durations. */
export function useNow(ms = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms)
    return () => clearInterval(t)
  }, [ms])
  return now
}

function readInspector(): boolean {
  try {
    return localStorage.getItem('sm.inspector') !== 'closed'
  } catch {
    return true
  }
}

/**
 * One agent's workbench (UX review §4.2), used by the run page and the Terminals views: a header bar with who the agent
 * is and what it does, the terminal, the message box, and an inspector with its task, usage, placement and
 * configuration. Everything secondary is collapsed or one click away; the terminal gets the space.
 */
export function RunWorkbench({ spaceId, run: r, mode, onMode, crumbs, inspectorDefault }: {
  spaceId: string
  run: Run
  mode: 'live' | 'replay'
  onMode: (m: 'live' | 'replay') => void
  /** Shown left of the header (for example a link back to all terminals). */
  crumbs?: ReactNode
  /** Whether the inspector starts open (remembered per browser once toggled). */
  inspectorDefault?: boolean
}) {
  const qc = useQueryClient()
  const me = useMe().data
  const now = useNow()
  const operator = atLeast(roleIn(me, spaceId), 'operator')
  const canTakeOver = atLeast(roleIn(me, spaceId), 'admin') // terminal:takeover; the API enforces it
  const ended = isEnded(r)
  const narrow = useNarrow()
  const [inspector, setInspector] = useState(() => (inspectorDefault === undefined ? readInspector() : inspectorDefault))
  const toggle = () => {
    setInspector((v) => {
      try {
        localStorage.setItem('sm.inspector', v ? 'closed' : 'open')
      } catch {
        /* private mode: not remembered */
      }
      return !v
    })
  }
  const onKeys = useTakeoverInput(spaceId, r.id, r, me)
  const stop = async (m: 'interrupt' | 'kill') => {
    try {
      qc.setQueryData(keys.run(spaceId, r.id), unwrap(await api.POST('/spaces/{space_id}/runs/{run_id}/stop', {
        params: { path: { space_id: spaceId, run_id: r.id } }, body: { mode: m, reason: 'stopped from the web UI' } })))
      notifyOk(m === 'kill' ? 'Stop requested' : 'Interrupt requested')
    } catch (e) {
      notifyError(e, 'Could not stop the run')
    }
  }
  const inputForm = useAppForm({
    defaultValues: { text: '' },
    onSubmit: async ({ value }) => {
      try {
        check(await api.POST('/spaces/{space_id}/runs/{run_id}/input', { params: { path: { space_id: spaceId, run_id: r.id } }, body: { text: value.text } }))
        inputForm.reset()
        notifyOk('Delivered when the agent is idle')
      } catch (e) {
        notifyError(e, 'Could not send input')
      }
    },
  })
  const duration = runDuration(r, now)

  return (
    <Stack gap="xs" style={{ minWidth: 0 }}>
      {crumbs}
      {/* Who and what on the left, the actions on the right. The role name truncates first; the turn and the state never
          shrink. On a phone the actions move to a second row of icon buttons. */}
      <Group justify="space-between" wrap="wrap" gap="xs" data-testid="run-header">
        <Group gap="xs" wrap="nowrap" data-testid="run-header-who" style={{ minWidth: 0, flex: '1 1 auto' }}>
          <Box style={{ flexShrink: 0, display: 'flex' }}><RunDot run={r} size={10} /></Box>
          <Title order={3} data-testid="run-header-role" style={{ minWidth: '4ch', flexShrink: 100, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{r.role}</Title>
          {!narrow && <Text size="sm" c="dimmed" truncate style={{ minWidth: 0, flexShrink: 1 }}>{kindLabel(r.kind)} · {harnessLabel(r.harness)}</Text>}
          {r.turn > 0 && <Badge variant="light" color="gray" data-testid="run-header-turn" style={{ flexShrink: 0, overflow: 'visible' }}>#{r.turn}</Badge>}
          <Text size="sm" data-testid="run-header-state" style={{ flexShrink: 0, whiteSpace: 'nowrap' }}>{runStateText(r, now)}</Text>
          {r.adopted && <Tooltip label="Adopted from the node after a restart"><Badge color="gray" variant="outline" style={{ flexShrink: 0 }}>adopted</Badge></Tooltip>}
        </Group>
        <Group gap="xs" wrap="nowrap" data-testid="run-header-actions" style={narrow ? { flexBasis: '100%' } : { flexShrink: 0, marginLeft: 'auto' }}>
          <SegmentedControl value={mode} onChange={(v) => onMode(v as 'live' | 'replay')} aria-label="Terminal view"
            data={[{ value: 'live', label: 'Live', disabled: ended }, { value: 'replay', label: 'Replay' }]} />
          <TakeoverPanel spaceId={spaceId} runId={r.id} run={r} me={me} canTakeOver={canTakeOver} ended={ended} part="button" compact={narrow} />
          {operator && !ended && (
            <Menu position="bottom-end">
              <Menu.Target>
                {narrow ? (
                  <ActionIcon color="owlRed" variant="light" aria-label="Stop"><IconPlayerStop size={16} /></ActionIcon>
                ) : (
                  <Button color="owlRed" variant="light" leftSection={<IconPlayerStop size={14} />} rightSection={<IconChevronDown size={12} />}>Stop</Button>
                )}
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Item onClick={() => void stop('interrupt')}>Interrupt (keeps the session resumable)</Menu.Item>
                <Menu.Item color="owlRed" onClick={() => void stop('kill')}>Stop the program</Menu.Item>
              </Menu.Dropdown>
            </Menu>
          )}
          <Tooltip label={inspector ? 'Hide details' : 'Show details'}>
            <ActionIcon variant="default" aria-label={inspector ? 'Hide details' : 'Show details'} onClick={toggle} ml={narrow ? 'auto' : undefined}>
              {inspector ? <IconLayoutSidebarRightCollapse size={16} /> : <IconLayoutSidebarRightExpand size={16} />}
            </ActionIcon>
          </Tooltip>
        </Group>
      </Group>

      <TakeoverPanel spaceId={spaceId} runId={r.id} run={r} me={me} canTakeOver={canTakeOver} ended={ended} part="banner" />
      {r.error && <Alert color="owlRed" title="Run error" p="xs">{r.error}</Alert>}

      {/* On a phone the inspector goes below the terminal instead of beside it. */}
      <Box data-testid="run-body" style={{ display: 'grid', gridTemplateColumns: inspector && !narrow ? 'minmax(0, 1fr) 300px' : 'minmax(0, 1fr)', gap: 'var(--mantine-spacing-sm)', alignItems: 'start' }}>
        <Stack gap="xs" style={{ minWidth: 0 }}>
          <TerminalView key={`${r.id}:${mode}`} spaceId={spaceId} runId={r.id} mode={mode} onInput={mode === 'live' ? onKeys : null}
            resize={operator ? { run: r, blocker: resizeBlocker(r, canTakeOver, holderRef(me), ended) } : undefined} />
          {ended ? (
            <Group gap="xs" justify="space-between" p="xs" style={{ border: '1px solid var(--mantine-color-default-border)', borderRadius: 'var(--mantine-radius-sm)' }}>
              <Text size="sm" c="dimmed">
                Ended {isSet(r.ended_at) ? `${datetime(r.ended_at)}` : ''}{r.end_reason ? ` · ${r.end_reason}` : ''}
                {r.exit_code !== undefined ? ` · exit ${r.exit_code}` : ''}{duration ? ` · ran ${duration}` : ''}
              </Text>
              {mode !== 'replay' && <Button size="xs" variant="default" onClick={() => onMode('replay')}>Replay from start</Button>}
            </Group>
          ) : operator && !r.takeover && (
            <form onSubmit={(e) => { e.preventDefault(); if (inputForm.state.values.text.trim()) void inputForm.handleSubmit() }}>
              <Group gap="xs" wrap="nowrap">
                <inputForm.AppField name="text">
                  {(f) => <f.TextField flex={1} placeholder={`Message ${r.role} (delivered at its next idle point)`} aria-label="Message the agent" />}
                </inputForm.AppField>
                <inputForm.Subscribe selector={(st) => st.values.text.trim() !== ''}>
                  {(has) => <Button type="submit" leftSection={<IconSend size={14} />} disabled={!has}>Send</Button>}
                </inputForm.Subscribe>
              </Group>
            </form>
          )}
        </Stack>
        {inspector && <Inspector spaceId={spaceId} run={r} now={now} />}
      </Box>
    </Stack>
  )
}

function Section({ title, children, right }: { title: string; children: ReactNode; right?: ReactNode }) {
  return (
    <Box py="xs" style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
      <Group justify="space-between" mb={4}>
        <Text size="xs" fw={600} c="dimmed" tt="uppercase">{title}</Text>
        {right}
      </Group>
      {children}
    </Box>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Group gap="xs" wrap="nowrap" align="flex-start">
      <Text size="sm" c="dimmed" w={84} style={{ flexShrink: 0 }}>{label}</Text>
      <Box style={{ minWidth: 0, flex: 1 }}>{typeof children === 'string' ? <Text size="sm" style={{ wordBreak: 'break-word' }}>{children}</Text> : children}</Box>
    </Group>
  )
}

const n = (v: number) => v.toLocaleString('en-US')

/** The run's inspector: now, task, usage, placement, then the launch configuration and identifiers, collapsed. */
function Inspector({ spaceId, run: r, now }: { spaceId: string; run: Run; now: number }) {
  const queue = useQueue(spaceId)
  const nodes = useNodes()
  const accounts = useSpaceAccounts(spaceId)
  const human = useHumanActions(spaceId)
  const usage = useQuery({ queryKey: ['usage', spaceId, r.id], queryFn: async () =>
    unwrap(await api.GET('/spaces/{space_id}/runs/{run_id}/usage', { params: { path: { space_id: spaceId, run_id: r.id } } })) })
  const task = (queue.data?.tasks ?? []).find((t) => t.assignee_run === r.id)
  const node = nodes.data?.find((x) => x.id === r.node_id)
  const account = (accounts.data ?? []).find((a) => a.account_id === r.launch.account_ref)
  const asking = (human.data?.items ?? []).filter((a) => a.requester_run === r.id && (a.status === 'open' || a.status === 'in_progress'))
  const u = usage.data
  const tokens = u?.known ? u.models.reduce((acc, m) => ({ inp: acc.inp + (m.input_tokens ?? 0), out: acc.out + (m.output_tokens ?? 0), cr: acc.cr + (m.cache_read_tokens ?? 0) }), { inp: 0, out: 0, cr: 0 }) : null
  const m = (r.manifest ?? {}) as Manifest
  return (
    <ScrollArea.Autosize mah="calc(100vh - 220px)" type="auto" style={{ border: '1px solid var(--mantine-color-default-border)', borderRadius: 'var(--mantine-radius-sm)' }}>
      <Box px="sm">
        <Section title="Now">
          <Stack gap={4}>
            <Row label="State">{runStateText(r, now)}</Row>
            {r.status && <Row label="Agent says">{r.status}{r.status_kind ? ` (${r.status_kind})` : ''}</Row>}
            <Row label="Turn">{r.turn > 0 ? String(r.turn) : 'none yet'}</Row>
            {runDuration(r, now) && <Row label={isEnded(r) ? 'Ran for' : 'Running'}>{runDuration(r, now)!}</Row>}
            {asking.length > 0 && (
              <Row label="Waiting">
                <AnchorLink to="/spaces/$spaceId/human" params={{ spaceId }} size="sm" c="owlYellow">{asking.length} human action{asking.length > 1 ? 's' : ''} →</AnchorLink>
              </Row>
            )}
          </Stack>
        </Section>
        <Section title="Task">
          {task ? (
            <Stack gap={2}>
              <AnchorLink to="/spaces/$spaceId/queue" params={{ spaceId }} size="sm" fw={500}>{task.title}</AnchorLink>
              <Text size="xs" c="dimmed">{task.status} · {task.priority} priority · #{task.position} in the queue</Text>
            </Stack>
          ) : <Text size="sm" c="dimmed">No task: {r.kind === 'control' ? 'control coordinates the space' : `launched by ${r.created_by.replace(/^user:/, 'a user ')}`}.</Text>}
        </Section>
        <Section title="Usage" right={u?.known ? <Text size="xs" c="dimmed">{u.final ? 'from the transcript' : 'so far'}</Text> : undefined}>
          {!u ? <Text size="sm" c="dimmed">Loading…</Text> : !tokens ? <Text size="sm" c="dimmed">{u.note && !/no usage reported/i.test(u.note) ? `No usage reported yet: ${u.note}.` : 'No usage reported yet.'}</Text> : (
            <Stack gap={2}>
              <Text size="sm">{n(tokens.inp)} in · {n(tokens.out)} out</Text>
              <Text size="xs" c="dimmed">{n(tokens.cr)} cache read</Text>
              {u.estimate && <Text size="sm">≈ ${u.estimate.usd.toFixed(2)} <Text span size="xs" c="dimmed">estimate</Text></Text>}
            </Stack>
          )}
        </Section>
        <Section title="Placement">
          <Stack gap={4}>
            <Row label="Node">
              <AnchorLink to="/nodes" size="sm">{node?.name ?? r.node_id}</AnchorLink>
              {node && <Text span size="xs" c="dimmed"> · {node.connected ? 'connected' : 'offline'}</Text>}
            </Row>
            <Row label="Account">{account?.name ?? (r.launch.account_ref ? 'set at launch' : 'none')}</Row>
            {r.launch.worktree && (
              <Row label="Worktree">
                <Text size="sm" className="mono" style={{ wordBreak: 'break-all' }}>{r.launch.worktree.branch}</Text>
                <AnchorLink to="/spaces/$spaceId/repositories" params={{ spaceId }} size="xs">in {r.launch.worktree.repo}</AnchorLink>
              </Row>
            )}
            {r.launch.model && <Row label="Model">{r.launch.model}</Row>}
            {r.harness_version && <Row label="Version"><Text size="sm" className="mono">{runHarnessVersion(r)}</Text></Row>}
          </Stack>
        </Section>
        <Accordion variant="default" multiple chevronPosition="right" styles={{ control: { paddingInline: 0 }, content: { paddingInline: 0 } }}>
          <Accordion.Item value="launch">
            <Accordion.Control><Text size="xs" fw={600} c="dimmed" tt="uppercase">Launch and configuration</Text></Accordion.Control>
            <Accordion.Panel>
              <Stack gap={4}>
                <Row label="Launched by">{r.created_by}</Row>
                <Row label="Created">{datetime(r.created_at)}</Row>
                {isSet(r.started_at) && <Row label="Started">{datetime(r.started_at)}</Row>}
                {isSet(r.ended_at) && <Row label="Ended">{`${datetime(r.ended_at)}${r.end_reason ? ` (${r.end_reason})` : ''}`}</Row>}
                {isEnded(r) && r.exit_code !== undefined && <Row label="Exit code">{String(r.exit_code)}</Row>}
                {m.space_role && <Row label="Role">{`${m.space_role.name} r${m.space_role.revision}`}</Row>}
                {m.template && <Row label="Template">{`${m.template.slug} r${m.template.revision}`}</Row>}
                {m.profile && <Row label="Profile">{`${m.profile.slug} r${m.profile.revision}`}</Row>}
                {m.prompt && <Row label="Prompt">{`${m.prompt.slug} v${m.prompt.number}${m.prompt.pinned ? ' (pinned)' : ''}`}</Row>}
                {(m.skills?.length ?? 0) > 0 && <Row label="Skills">{m.skills!.map((s) => `${s.slug} v${s.number}`).join(', ')}</Row>}
                {(m.mcp_servers?.length ?? 0) > 0 && <Row label="MCP">{m.mcp_servers!.map((x) => x.slug).join(', ')}</Row>}
                {m.policy && <Row label="Permissions">{`up to ${m.policy.max_permission_mode}`}</Row>}
                {Object.entries(m.effective ?? {}).filter(([k]) => k !== 'system_text_sha256').map(([k, f]) => (
                  <Row key={k} label={k}><Text size="xs" className="mono" style={{ wordBreak: 'break-all' }}>{f.value} <Text span c="dimmed">({f.origin.replace('_', ' ')})</Text></Text></Row>
                ))}
                {m.dependencies && <Text size="xs" c="dimmed">{Object.entries(m.dependencies).filter(([, v]) => v !== '' && v !== undefined).map(([k, v]) => dependencyText(k, v)).join(' · ')}</Text>}
                {(m.skills_unavailable?.length ?? 0) > 0 && <Text size="xs" c="owlYellow">Not installed: {m.skills_unavailable!.join(', ')}</Text>}
              </Stack>
            </Accordion.Panel>
          </Accordion.Item>
          <Accordion.Item value="ids">
            <Accordion.Control><Text size="xs" fw={600} c="dimmed" tt="uppercase">Identifiers</Text></Accordion.Control>
            <Accordion.Panel>
              <Stack gap={4}>
                <Id label="Run" value={r.id} />
                <Id label="Instance" value={r.instance_id} />
                {r.session_id && <Id label="Session" value={r.session_id} />}
                {r.terminal_id && <Id label="Terminal" value={r.terminal_id} />}
                <Id label="Lease gen." value={String(r.generation)} />
              </Stack>
            </Accordion.Panel>
          </Accordion.Item>
        </Accordion>
      </Box>
    </ScrollArea.Autosize>
  )
}

function Id({ label, value }: { label: string; value: string }) {
  return (
    <Row label={label}>
      <Group gap={4} wrap="nowrap">
        <Text size="xs" className="mono" truncate>{value}</Text>
        <CopyButton value={value}>
          {({ copied, copy }) => (
            <ActionIcon size="sm" aria-label={`Copy ${label}`} onClick={copy}>{copied ? <IconCheck size={12} /> : <IconCopy size={12} />}</ActionIcon>
          )}
        </CopyButton>
      </Group>
    </Row>
  )
}

type Manifest = {
  effective?: Record<string, { value: string; origin: string }>
  prompt?: { slug: string; number: number; pinned: boolean; sha256: string }
  space_role?: { name: string; revision: number }
  template?: { slug: string; revision: number }
  profile?: { slug: string; revision: number }
  dependencies?: Record<string, Dependency>
  policy?: { version: number; max_permission_mode: string }
  skills?: { slug: string; number: number; pinned: boolean }[] | null
  skills_unavailable?: string[]
  mcp_servers?: { slug: string; version: number; tools?: string[] | null }[] | null
}
