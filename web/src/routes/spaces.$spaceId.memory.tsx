import { Alert, Badge, Button, Card, Code, Drawer, Group, NavLink, ScrollArea, SimpleGrid, Stack, Table, Tabs, Text } from '@mantine/core'
import { IconAlertTriangle, IconInfoCircle } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, unwrap, type MemoryCollection } from '@/api/client'
import { useMe, useMemory, useSpace } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { PageHeader } from '@/components/PageHeader'
import { DreamsCard } from '@/features/memory/DreamsCard'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/memory')({ component: MemoryPage })

const short = (h?: string) => (h ? h.slice(0, 8) : '—')

/**
 * Role memory (SM-MEM-*): one Git history per role, written by the role's runs on the node and preserved centrally.
 * Central shows only acknowledged history and never edits it; the node's view says what is not preserved yet.
 */
function MemoryPage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const mem = useMemory(spaceId)
  const [open, setOpen] = useState<string | null>(null)
  const items = mem.data?.items ?? []
  const current = items.find((c) => c.role === open) ?? null
  return (
    <>
      <PageHeader title={space.data?.name ?? 'Space'} description="Role memory: notes each role keeps across runs, shared by every instance of the role and preserved centrally." />
      <SpaceNav spaceId={spaceId} />
      <ErrorAlert error={mem.error} />
      <DreamsCard spaceId={spaceId} />
      {mem.data && !mem.data.available && (
        <Alert color="owlYellow" icon={<IconAlertTriangle size={16} />} mb="md" title="Central cannot store memory">Install git on the central host; nodes keep their memory and sync once it can.</Alert>
      )}
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr><Table.Th>Role</Table.Th><Table.Th>Preserved centrally</Table.Th><Table.Th>On the node</Table.Th><Table.Th>Conflicts</Table.Th></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {items.map((c) => (
              <Table.Tr key={c.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(c.role)}>
                <Table.Td><Text size="sm" fw={500} className="mono">{c.role}</Text></Table.Td>
                <Table.Td>
                  {c.head ? <Text size="sm"><span className="mono">{short(c.head)}</span> · {c.head_at && ago(c.head_at)}</Text> : <Text size="sm" c="dimmed">nothing yet</Text>}
                </Table.Td>
                <Table.Td><NodeState c={c} /></Table.Td>
                <Table.Td>{c.open_conflicts > 0 ? <Badge color="owlYellow" variant="light">{c.open_conflicts} to reconcile</Badge> : <Text size="sm" c="dimmed">none</Text>}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {mem.isSuccess && items.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No memory yet. Each role starts its memory with its first run.</Text>}
      </Card>
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="xl" title={current ? `Memory of ${current.role}` : ''}>
        {current && <Collection spaceId={spaceId} c={current} />}
      </Drawer>
    </>
  )
}

function NodeState({ c }: { c: MemoryCollection }) {
  const n = c.node
  if (!n) return <Text size="sm" c="dimmed">not reported</Text>
  return (
    <Group gap={4}>
      {n.pending ? <Badge color="owlBlue" variant="light">{n.local_commits} commit(s) to sync</Badge> : <Badge color="owlGreen" variant="light">in sync</Badge>}
      {n.uncheckpointed > 0 && <Badge color="gray" variant="light">{n.uncheckpointed} unsaved file(s)</Badge>}
      {n.retries > 0 && <Badge color="owlRed" variant="light">{n.retries} retries</Badge>}
      <Text size="xs" c="dimmed">{n.instances} instance(s){n.last_ack_at && ` · acknowledged ${ago(n.last_ack_at)}`}</Text>
    </Group>
  )
}

function Collection({ spaceId, c }: { spaceId: string; c: MemoryCollection }) {
  const path = { space_id: spaceId, role: c.role }
  const files = useQuery({ queryKey: ['memory', spaceId, c.role, 'files', c.head], enabled: !!c.head,
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/memory/{role}/files', { params: { path } })).items })
  const history = useQuery({ queryKey: ['memory', spaceId, c.role, 'history', c.head], enabled: !!c.head,
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/memory/{role}/history', { params: { path } })).items })
  const conflicts = useQuery({ queryKey: ['memory', spaceId, c.role, 'conflicts', c.open_conflicts],
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/memory/{role}/conflicts', { params: { path } })).items })
  const [file, setFile] = useState<string | null>(null)
  const shown = file ?? files.data?.find((f) => f.toLowerCase() === 'readme.md') ?? files.data?.[0] ?? null
  const content = useQuery({ queryKey: ['memory', spaceId, c.role, 'file', c.head, shown], enabled: !!shown,
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/memory/{role}/file', { params: { path, query: { path: shown! } } })).content })
  const n = c.node
  return (
    <Stack gap="sm">
      <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />}>
        Central keeps the history the node has synced and never edits it. Runs of this role change memory; written by agents, it is untrusted content.
      </Alert>
      <SimpleGrid cols={{ base: 2, sm: 4 }}>
        <Fact label="Central head" value={short(c.head)} hint={c.head_at ? datetime(c.head_at) : undefined} />
        <Fact label="Node head" value={short(n?.head)} hint={n?.pending ? 'not yet acknowledged' : 'acknowledged'} />
        <Fact label="Unsaved files" value={String(n?.uncheckpointed ?? '—')} hint={n?.last_checkpoint_at ? `checkpoint ${ago(n.last_checkpoint_at)}` : undefined} />
        <Fact label="Sync" value={n?.retries ? `${n.retries} retries` : 'ok'} hint={n?.last_error} />
      </SimpleGrid>
      <Tabs defaultValue="files">
        <Tabs.List>
          <Tabs.Tab value="files">Files</Tabs.Tab>
          <Tabs.Tab value="history">History</Tabs.Tab>
          <Tabs.Tab value="conflicts">Conflicts{c.open_conflicts > 0 && ` (${c.open_conflicts})`}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="files" pt="sm">
          {!c.head ? <Text size="sm" c="dimmed">Nothing has been synced yet.</Text> : (
            <Group align="flex-start" wrap="nowrap" gap="sm">
              <ScrollArea.Autosize mah={480} w={220}>
                <nav aria-label="Memory files">
                  {(files.data ?? []).map((f) => <NavLink component="button" key={f} label={<span className="mono">{f}</span>} active={f === shown} onClick={() => setFile(f)} />)}
                </nav>
              </ScrollArea.Autosize>
              <Card withBorder style={{ flex: 1, minWidth: 0 }}>
                {shown && <Text size="xs" c="dimmed" mb="xs" className="mono">{shown}</Text>}
                {content.data !== undefined && (shown?.endsWith('.md') ? <MarkdownView source={content.data} spaceId={spaceId} /> : <Code block>{content.data}</Code>)}
              </Card>
            </Group>
          )}
        </Tabs.Panel>
        <Tabs.Panel value="history" pt="sm">
          <Table>
            <Table.Tbody>
              {(history.data ?? []).map((h) => (
                <Table.Tr key={h.id}>
                  <Table.Td><Text size="xs" className="mono">{short(h.id)}</Text></Table.Td>
                  <Table.Td><Text size="sm">{h.subject}</Text><Text size="xs" c="dimmed">{h.author}</Text></Table.Td>
                  <Table.Td><Text size="xs" c="dimmed">{datetime(h.at)}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Tabs.Panel>
        <Tabs.Panel value="conflicts" pt="sm">
          <Conflicts spaceId={spaceId} role={c.role} items={conflicts.data ?? []} />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  )
}

function Fact({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <Card p="xs" withBorder>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm" className="mono">{value}</Text>
      {hint && <Text size="xs" c="dimmed" lineClamp={2}>{hint}</Text>}
    </Card>
  )
}

type ConflictItem = { id: string; source: string; ref?: string; detail?: string; created_at: string; resolved_at?: string; resolved_by?: string }

function Conflicts({ spaceId, role, items }: { spaceId: string; role: string; items: ConflictItem[] }) {
  const qc = useQueryClient()
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const resolve = async (id: string) => {
    try {
      check(await api.POST('/spaces/{space_id}/memory/{role}/conflicts/{conflict_id}/resolve', { params: { path: { space_id: spaceId, role, conflict_id: id } } }))
      void qc.invalidateQueries({ queryKey: ['memory', spaceId] })
    } catch (e) {
      notifyError(e, 'Could not mark the conflict reconciled')
    }
  }
  if (items.length === 0) return <Text size="sm" c="dimmed">No conflicts. When two instances change the same notes at once, both versions are kept and listed here.</Text>
  return (
    <Stack gap="xs">
      {items.map((x) => (
        <Card key={x.id} withBorder p="sm">
          <Group justify="space-between">
            <Text size="sm" fw={500}>{x.source === 'node' ? 'Between instances on the node' : 'Not applied by central'}</Text>
            {x.resolved_at ? <Badge color="owlGreen" variant="light">reconciled</Badge> : canWrite && <Button size="xs" variant="default" onClick={() => void resolve(x.id)}>Mark reconciled</Button>}
          </Group>
          <Text size="sm">{x.detail}</Text>
          <Text size="xs" c="dimmed">{datetime(x.created_at)}{x.ref && <> · kept as <span className="mono">{x.ref}</span></>}</Text>
        </Card>
      ))}
    </Stack>
  )
}
