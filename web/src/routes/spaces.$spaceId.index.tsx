import { Badge, Button, Card, Group, Menu, SimpleGrid, Stack, Table, Tabs, Text, Title, Tooltip } from '@mantine/core'
import { IconChevronDown, IconPencil, IconPlayerPause, IconPlayerPlay, IconRocket } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Space } from '@/api/client'
import { keys, useMe, useNodes, useRuns, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { NodeState, RunState, SpaceState } from '@/components/StatusBadge'
import { SpaceAccountsCard } from '@/features/accounts/SpaceAccountsCard'
import { LaunchRunModal } from '@/features/spaces/LaunchRunModal'
import { SpaceFormModal } from '@/features/spaces/SpaceFormModal'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime, isSet } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/')({ component: SpacePage })

const PAUSE_MODES: { mode: 'hold' | 'interrupt' | 'emergency'; label: string; detail: string }[] = [
  { mode: 'hold', label: 'Hold', detail: 'No new work starts; running agents continue' },
  { mode: 'interrupt', label: 'Interrupt', detail: 'Interrupt running agents and stop them' },
  { mode: 'emergency', label: 'Emergency stop', detail: 'Stop every run now' },
]

function SpacePage() {
  const { spaceId } = Route.useParams()
  const qc = useQueryClient()
  const space = useSpace(spaceId)
  const runs = useRuns(spaceId)
  const nodes = useNodes()
  const me = useMe().data
  const [launching, setLaunching] = useState(false)
  const [editing, setEditing] = useState(false)
  const role = roleIn(me, spaceId)

  if (space.error) return <ErrorAlert error={space.error} />
  if (!space.data) return null
  const s = space.data
  const node = nodes.data?.find((n) => n.id === s.node_id)
  const active = (runs.data ?? []).filter((r) => !['exited', 'failed', 'lost'].includes(r.observed))

  const setState = async (state: Space['state'], mode?: 'hold' | 'interrupt' | 'emergency') => {
    try {
      const next = unwrap(await api.POST('/spaces/{space_id}/state', { params: { path: { space_id: s.id }, header: { 'If-Match': ifMatch(s.version) } }, body: { state, mode } }))
      qc.setQueryData(keys.space(s.id), next)
      notifyOk(state === 'paused' ? `Space paused (${mode})` : state === 'active' ? 'Space resumed' : `Space ${state}`)
    } catch (e) {
      notifyError(e, 'Could not change the space state')
    }
  }

  return (
    <>
      <PageHeader
        title={<Group gap="xs">{s.name}<SpaceState space={s} /></Group>}
        description={<><span className="mono">{s.slug}</span>{s.description ? ` · ${s.description}` : ''}</>}
        actions={
          <>
            {atLeast(role, 'admin') && <Button variant="default" leftSection={<IconPencil size={14} />} onClick={() => setEditing(true)}>Edit</Button>}
            {atLeast(role, 'operator') && s.state === 'active' && (
              <Menu position="bottom-end">
                <Menu.Target>
                  <Button variant="default" leftSection={<IconPlayerPause size={14} />} rightSection={<IconChevronDown size={12} />}>Pause</Button>
                </Menu.Target>
                <Menu.Dropdown>
                  {PAUSE_MODES.map((p) => (
                    <Menu.Item key={p.mode} color={p.mode === 'emergency' ? 'red' : undefined} onClick={() => void setState('paused', p.mode)}>
                      <Text size="sm">{p.label}</Text>
                      <Text size="xs" c="dimmed">{p.detail}</Text>
                    </Menu.Item>
                  ))}
                </Menu.Dropdown>
              </Menu>
            )}
            {atLeast(role, 'operator') && s.state === 'paused' && <Button variant="default" leftSection={<IconPlayerPlay size={14} />} onClick={() => void setState('active')}>Resume</Button>}
            {atLeast(role, 'operator') && (
              <Tooltip label={s.state !== 'active' ? 'The space is not active' : 'Start an agent in this space'}>
                <Button leftSection={<IconRocket size={14} />} disabled={s.state !== 'active'} onClick={() => setLaunching(true)}>Launch run</Button>
              </Tooltip>
            )}
          </>
        }
      />

      <SpaceNav spaceId={spaceId} />
      <SimpleGrid cols={{ base: 1, sm: 3 }} mb="md">
        <Card>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Node</Text>
          <Group gap="xs" mt={4}>
            <Text fw={500}>{node?.name ?? s.node_id}</Text>
            {node && <NodeState node={node} />}
          </Group>
          {s.desired_node_id && s.desired_node_id !== s.node_id && <Text size="xs" c="owlYellow">Relocating to {s.desired_node_id}</Text>}
        </Card>
        <Card>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Execution lease</Text>
          {s.lease ? (
            <Group gap="xs" mt={4}>
              <Badge color={s.lease.valid ? 'owlGreen' : 'owlRed'}>{s.lease.valid ? 'valid' : s.lease.state}</Badge>
              <Text size="sm" className="mono">gen {s.lease.generation}</Text>
              <Text size="xs" c="dimmed">expires {ago(s.lease.expires_at)}</Text>
            </Group>
          ) : (
            <Text size="sm" c="dimmed" mt={4}>None — granted when the node connects</Text>
          )}
        </Card>
        <Card>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Active runs</Text>
          <Text fw={600} size="xl" mt={2}>{active.length}</Text>
        </Card>
      </SimpleGrid>
      <SpaceAccountsCard spaceId={s.id} />

      <Tabs defaultValue="runs" keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="runs">Runs</Tabs.Tab>
          <Tabs.Tab value="about">Goals &amp; instructions</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="runs" pt="sm">
          <ErrorAlert error={runs.error} />
          <Card p={0}>
            <Table>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Run</Table.Th>
                  <Table.Th>Role</Table.Th>
                  <Table.Th>Harness</Table.Th>
                  <Table.Th>State</Table.Th>
                  <Table.Th>Agent status</Table.Th>
                  <Table.Th>Started</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {(runs.data ?? []).map((r) => (
                  <Table.Tr key={r.id}>
                    <Table.Td>
                      <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: r.id }} className="mono" size="sm">{r.id}</AnchorLink>
                    </Table.Td>
                    <Table.Td>{r.role} <Text span size="xs" c="dimmed">({r.kind})</Text></Table.Td>
                    <Table.Td>{r.harness}</Table.Td>
                    <Table.Td><RunState run={r} /></Table.Td>
                    <Table.Td>{r.status ? <Badge variant="outline" color="gray">{r.status}</Badge> : <Text size="xs" c="dimmed">—</Text>}</Table.Td>
                    <Table.Td><Tooltip label={datetime(r.started_at ?? r.created_at)}><Text size="xs" c="dimmed">{ago(isSet(r.started_at) ? r.started_at : r.created_at)}</Text></Tooltip></Table.Td>
                  </Table.Tr>
                ))}
                {runs.isSuccess && runs.data.length === 0 && (
                  <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed" ta="center" p="md">No runs yet.</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Card>
        </Tabs.Panel>
        <Tabs.Panel value="about" pt="sm">
          <Stack gap="md">
            <div>
              <Title order={5}>Goals</Title>
              <Text size="sm" style={{ whiteSpace: 'pre-wrap' }} c={s.goals ? undefined : 'dimmed'}>{s.goals || 'None set.'}</Text>
            </div>
            <div>
              <Title order={5}>Standing instructions</Title>
              <Text size="sm" style={{ whiteSpace: 'pre-wrap' }} c={s.standing_instructions ? undefined : 'dimmed'}>{s.standing_instructions || 'None set.'}</Text>
            </div>
            <div>
              <Title order={5}>Working directory</Title>
              <Text size="sm" className="mono">{s.workdir || '—'}</Text>
              <Text size="xs" c="dimmed">On disconnect beyond the grace period, runs are {s.disconnect_policy === 'kill' ? 'stopped' : 'interrupted'}.</Text>
            </div>
          </Stack>
        </Tabs.Panel>
      </Tabs>

      <LaunchRunModal space={s} node={node} opened={launching} onClose={() => setLaunching(false)} />
      {editing && <SpaceFormModal space={s} opened onClose={() => setEditing(false)} />}
    </>
  )
}
