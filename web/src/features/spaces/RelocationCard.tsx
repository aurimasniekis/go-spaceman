import { Badge, Button, Card, Group, List, Stack, Table, Text, Tooltip } from '@mantine/core'
import { useQueryClient } from '@tanstack/react-query'
import { api, unwrap, type Relocation } from '@/api/client'
import { keys, useNodes, useRelocation } from '@/api/queries'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

const STEPS = ['draining', 'checking', 'exporting', 'preparing_destination', 'finalizing', 'done'] as const

const stateColor: Record<Relocation['state'], string> = {
  requested: 'gray',
  draining: 'owlYellow',
  checking: 'owlYellow',
  blocked: 'owlRed',
  exporting: 'owlYellow',
  preparing_destination: 'owlYellow',
  finalizing: 'owlYellow',
  done: 'owlGreen',
  checked: 'owlBlue',
  failed: 'owlRed',
  cancelled: 'gray',
}

export const relocationLabel = (s: Relocation['state']) => s.replace('_', ' ')

/** The source's inspection: what a clean relocation would lose (exportable) and what stops it. */
export function RelocationReport({ r }: { r: Relocation }) {
  const repos = r.report?.repositories ?? []
  return (
    <Stack gap="xs">
      <Group gap="xs">
        <Text size="sm" fw={500}>Precheck</Text>
        {r.report && <Text size="xs" c="dimmed">inspected {ago(r.report.checked_at)}</Text>}
        {r.report && <Badge variant="outline" color={r.report.flushed ? 'owlGreen' : 'owlYellow'}>{r.report.flushed ? 'memory and uploads synced' : 'sync incomplete'}</Badge>}
      </Group>
      {repos.length > 0 && (
        <Table withTableBorder>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Repository</Table.Th>
              <Table.Th>Branch</Table.Th>
              <Table.Th>Changed</Table.Th>
              <Table.Th>Untracked</Table.Th>
              <Table.Th>Unpushed</Table.Th>
              <Table.Th>Stashes</Table.Th>
              <Table.Th>Worktrees</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {repos.map((x) => (
              <Table.Tr key={x.path}>
                <Table.Td className="mono">{x.path}</Table.Td>
                <Table.Td className="mono">{x.branch || (x.head ? `${x.head.slice(0, 8)} (detached)` : '—')}</Table.Td>
                <Table.Td>{x.dirty}</Table.Td>
                <Table.Td>{x.untracked}</Table.Td>
                <Table.Td>{(x.branches ?? []).reduce((n, b) => n + b.unpushed, 0) + x.detached_unpushed}</Table.Td>
                <Table.Td>{x.stashes}</Table.Td>
                <Table.Td>{(x.worktrees ?? []).length}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {r.report && repos.length === 0 && <Text size="sm" c="dimmed">No repositories in the space folder.</Text>}
      {r.issues.length === 0 && r.report ? (
        <Text size="sm" c="owlGreen">Clean: everything is pushed and synced; nothing would be lost.</Text>
      ) : (
        <List size="sm" spacing={2}>
          {r.issues.map((i, n) => (
            <List.Item key={n}>
              <Badge size="xs" mr={6} color={i.exportable ? 'owlYellow' : 'owlRed'}>{i.code.replace('_', ' ')}</Badge>
              {i.path && <Text span className="mono" size="sm">{i.path}: </Text>}
              {i.detail}
              {!i.exportable && <Text span size="xs" c="dimmed"> (blocks until resolved)</Text>}
            </List.Item>
          ))}
        </List>
      )}
      {(r.report?.instances ?? []).length > 0 && (
        <Text size="xs" c="dimmed">{r.report!.instances.length} native harness session folder(s) stay on the source: fresh session, project continuity only.</Text>
      )}
    </Stack>
  )
}

/** The space's relocation in progress or its latest one, with progress, the report and the operator's choices. */
export function RelocationCard({ spaceId, canMove }: { spaceId: string; canMove: boolean }) {
  const qc = useQueryClient()
  const rel = useRelocation(spaceId)
  const nodes = useNodes()
  const r = rel.data
  if (!r || r.mode === 'precheck') return null
  const name = (id: string) => nodes.data?.find((n) => n.id === id)?.name ?? id
  const active = !['done', 'failed', 'cancelled', 'checked'].includes(r.state)
  const act = async (what: 'retry' | 'cancel' | 'release', body?: { dirty_state?: 'refuse' | 'export' }) => {
    try {
      const params = { path: { space_id: spaceId } }
      const next =
        what === 'retry'
          ? unwrap(await api.POST('/spaces/{space_id}/relocation/retry', { params, body: body ?? {} }))
          : what === 'cancel'
            ? unwrap(await api.POST('/spaces/{space_id}/relocation/cancel', { params }))
            : unwrap(await api.POST('/spaces/{space_id}/relocation/release', { params }))
      qc.setQueryData(keys.relocation(spaceId), next)
      notifyOk(what === 'cancel' ? 'Relocation cancelled' : what === 'release' ? 'The source folder will be deleted' : 'Relocation resumed')
    } catch (e) {
      notifyError(e, 'Could not change the relocation')
    }
  }
  const step = STEPS.indexOf(r.state as (typeof STEPS)[number])
  return (
    <Card mb="md" withBorder>
      <Stack gap="xs">
        <Group justify="space-between">
          <Group gap="xs">
            <Text fw={600}>Relocation</Text>
            <Badge color={stateColor[r.state]}>{relocationLabel(r.state)}</Badge>
            {r.mode === 'source_unavailable' && <Badge color="owlRed" variant="outline">source unavailable</Badge>}
            <Text size="sm">{name(r.source_node_id)} → {name(r.destination_node_id)}</Text>
            <Tooltip label={datetime(r.created_at)}><Text size="xs" c="dimmed">requested {ago(r.created_at)}</Text></Tooltip>
          </Group>
          {canMove && (
            <Group gap="xs">
              {r.state === 'blocked' && r.dirty_state === 'refuse' && r.issues.some((i) => i.exportable) && !r.issues.some((i) => !i.exportable) && (
                <Button size="xs" onClick={() => void act('retry', { dirty_state: 'export' })}>Export and continue</Button>
              )}
              {r.state === 'blocked' && <Button size="xs" variant="default" onClick={() => void act('retry')}>Retry</Button>}
              {active && r.state !== 'finalizing' && <Button size="xs" variant="default" color="owlRed" onClick={() => void act('cancel')}>Cancel</Button>}
              {r.state === 'done' && (r.source_state === 'retained' || r.source_state === 'pending') && (
                <Button size="xs" variant="default" onClick={() => void act('release')}>Delete the retained source folder</Button>
              )}
            </Group>
          )}
        </Group>
        {active && step >= 0 && (
          <Group gap={4}>
            {STEPS.map((s, i) => (
              <Badge key={s} size="xs" variant={i <= step ? 'filled' : 'outline'} color={i < step ? 'owlGreen' : i === step ? 'owlYellow' : 'gray'}>{relocationLabel(s)}</Badge>
            ))}
          </Group>
        )}
        {r.error && <Text size="sm" c={r.state === 'done' ? undefined : 'owlRed'}>{r.error}</Text>}
        {r.mode === 'clean' && (r.report || r.issues.length > 0) && <RelocationReport r={r} />}
        {r.loss && (
          <Stack gap={2}>
            <Text size="sm">{r.loss.statement}</Text>
            <List size="xs">
              {r.loss.unknown.map((u) => <List.Item key={u}>{u}</List.Item>)}
              {r.loss.memory.filter((m) => m.pending || m.local_commits > 0 || m.uncheckpointed > 0).map((m) => (
                <List.Item key={m.role}>memory {m.role}: {m.local_commits} commit(s) and {m.uncheckpointed} file(s) not acknowledged at the last report</List.Item>
              ))}
              {r.loss.runs.length > 0 && <List.Item>{r.loss.runs.length} run(s) on the source marked lost</List.Item>}
            </List>
            {r.acknowledgement && <Text size="xs" c="dimmed">Acknowledged: “{r.acknowledgement}”</Text>}
          </Stack>
        )}
        {r.state === 'done' && (
          <Text size="xs" c="dimmed">
            Lease generation {r.generation_before} → {r.generation_after}. Source folder: {r.source_state || 'pending'}
            {r.retain_until ? ` until ${datetime(r.retain_until)}` : ''}. {r.continuity.label}.
          </Text>
        )}
      </Stack>
    </Card>
  )
}
