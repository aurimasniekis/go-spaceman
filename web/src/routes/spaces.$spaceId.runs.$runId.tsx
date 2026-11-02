import { Anchor, Badge, Breadcrumbs, Button, Card, Group, Menu, SegmentedControl, SimpleGrid, Stack, Text, TextInput, Tooltip } from '@mantine/core'
import { IconChevronDown, IconSend, IconPlayerStop } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, unwrap } from '@/api/client'
import { keys, useMe, useRun, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { RunState } from '@/components/StatusBadge'
import { TerminalView } from '@/features/terminal/TerminalView'
import { atLeast, roleIn } from '@/lib/access'
import { datetime, isSet } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/runs/$runId')({
  validateSearch: (s: Record<string, unknown>): { view?: 'live' | 'replay' } => (s.view === 'replay' || s.view === 'live' ? { view: s.view } : {}),
  component: RunPage,
})

const ENDED = ['exited', 'failed', 'lost']

function RunPage() {
  const { spaceId, runId } = Route.useParams()
  const { view } = Route.useSearch()
  const navigate = Route.useNavigate()
  const qc = useQueryClient()
  const space = useSpace(spaceId)
  const run = useRun(spaceId, runId)
  const me = useMe().data
  const [input, setInput] = useState('')
  const operator = atLeast(roleIn(me, spaceId), 'operator')

  if (run.error) return <ErrorAlert error={run.error} />
  if (!run.data) return null
  const r = run.data
  const ended = ENDED.includes(r.observed)
  const mode = view ?? (ended ? 'replay' : 'live')

  const stop = async (m: 'interrupt' | 'kill') => {
    try {
      qc.setQueryData(keys.run(spaceId, runId), unwrap(await api.POST('/spaces/{space_id}/runs/{run_id}/stop', { params: { path: { space_id: spaceId, run_id: runId } }, body: { mode: m, reason: 'stopped from the web UI' } })))
      notifyOk(m === 'kill' ? 'Stop requested' : 'Interrupt requested')
    } catch (e) {
      notifyError(e, 'Could not stop the run')
    }
  }

  const send = async () => {
    try {
      check(await api.POST('/spaces/{space_id}/runs/{run_id}/input', { params: { path: { space_id: spaceId, run_id: runId } }, body: { text: input } }))
      setInput('')
      notifyOk('Delivered when the agent is idle')
    } catch (e) {
      notifyError(e, 'Could not send input')
    }
  }

  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/spaces" size="sm">Spaces</Anchor>
        <AnchorLink to="/spaces/$spaceId" params={{ spaceId }} size="sm">{space.data?.name ?? spaceId}</AnchorLink>
        <Text size="sm" className="mono">{runId}</Text>
      </Breadcrumbs>
      <Group justify="space-between" wrap="nowrap">
        <Group gap="xs">
          <Text fw={600} size="lg">{r.role}</Text>
          <Badge variant="outline" color="gray">{r.harness}</Badge>
          <RunState run={r} />
          {r.status && <Badge color="owlPurple" variant="outline">{r.status}</Badge>}
          {r.adopted && <Tooltip label="Adopted from the node after a restart"><Badge color="gray" variant="outline">adopted</Badge></Tooltip>}
        </Group>
        <Group gap="xs">
          <SegmentedControl value={mode} onChange={(v) => void navigate({ search: { view: v as 'live' | 'replay' } })}
            data={[{ value: 'live', label: 'Live', disabled: ended }, { value: 'replay', label: 'Replay' }]} aria-label="Terminal view" />
          {operator && !ended && (
            <Menu position="bottom-end">
              <Menu.Target>
                <Button color="owlRed" variant="light" leftSection={<IconPlayerStop size={14} />} rightSection={<IconChevronDown size={12} />}>Stop</Button>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Item onClick={() => void stop('interrupt')}>Interrupt (keeps the session resumable)</Menu.Item>
                <Menu.Item color="owlRed" onClick={() => void stop('kill')}>Stop the program</Menu.Item>
              </Menu.Dropdown>
            </Menu>
          )}
        </Group>
      </Group>

      <TerminalView key={mode} spaceId={spaceId} runId={runId} mode={mode} />

      {operator && !ended && (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (input.trim()) void send()
          }}
        >
          <Group gap="xs" wrap="nowrap">
            <TextInput flex={1} placeholder="Message the agent (sent at its next idle point)" value={input} onChange={(e) => setInput(e.target.value)} aria-label="Message the agent" />
            <Button type="submit" leftSection={<IconSend size={14} />} disabled={!input.trim()}>Send</Button>
          </Group>
        </form>
      )}

      <SimpleGrid cols={{ base: 2, sm: 4 }}>
        <Detail label="Kind" value={r.kind} />
        <Detail label="Instance" value={r.instance_id} mono />
        <Detail label="Session" value={r.session_id || '—'} mono />
        <Detail label="Lease generation" value={String(r.generation)} mono />
        <Detail label="Launched by" value={r.created_by} mono />
        <Detail label="Created" value={datetime(r.created_at)} />
        <Detail label="Started" value={isSet(r.started_at) ? datetime(r.started_at) : '—'} />
        <Detail label="Ended" value={isSet(r.ended_at) ? `${datetime(r.ended_at)}${r.end_reason ? ` (${r.end_reason})` : ''}` : '—'} />
        <Detail label="Exit code" value={r.exit_code !== undefined && ended ? String(r.exit_code) : '—'} mono />
      </SimpleGrid>
      {r.error && <ErrorAlert error={new Error(r.error)} title="Run error" />}
    </Stack>
  )
}

function Detail({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <Card p="sm">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm" className={mono ? 'mono' : undefined} style={{ wordBreak: 'break-all' }}>{value}</Text>
    </Card>
  )
}
