import { Badge, Button, Card, Code, Group, Popover, Table, Text, TextInput } from '@mantine/core'
import { useDebouncedValue } from '@mantine/hooks'
import { IconShieldCheck } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type AuditRecord } from '@/api/client'
import { useAudit } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/audit')({ component: AuditPage })

function AuditPage() {
  const [action, setAction] = useState('')
  const [actor, setActor] = useState('')
  const [target, setTarget] = useState('')
  const [cursors, setCursors] = useState<string[]>([''])
  const [filters] = useDebouncedValue({ action, actor_id: actor, target_id: target }, 300)
  const cursor = cursors[cursors.length - 1]
  const page = useAudit({ ...filters, cursor })
  const [verifying, setVerifying] = useState(false)

  const verify = async () => {
    setVerifying(true)
    try {
      const r = unwrap(await api.POST('/audit/verify'))
      if (r.ok) notifyOk(`Audit chain intact (${r.events} events)`)
      else notifyError(new Error(`${r.problem ?? 'chain broken'} at event ${r.broken_at ?? '?'}`), 'Audit chain verification failed')
    } catch (e) {
      notifyError(e)
    } finally {
      setVerifying(false)
    }
  }

  const reset = (fn: (v: string) => void) => (e: React.ChangeEvent<HTMLInputElement>) => {
    fn(e.target.value)
    setCursors([''])
  }

  return (
    <>
      <PageHeader title="Audit log" description="Hash-chained record of every security-relevant action. Secret values are never recorded."
        actions={<Button variant="default" leftSection={<IconShieldCheck size={14} />} loading={verifying} onClick={() => void verify()}>Verify chain</Button>} />
      <Group mb="sm" gap="xs">
        <TextInput placeholder="Action, e.g. run.launched" value={action} onChange={reset(setAction)} aria-label="Filter by action" />
        <TextInput placeholder="Actor ID" value={actor} onChange={reset(setActor)} aria-label="Filter by actor" />
        <TextInput placeholder="Target ID" value={target} onChange={reset(setTarget)} aria-label="Filter by target" />
      </Group>
      <ErrorAlert error={page.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Time</Table.Th>
              <Table.Th>Action</Table.Th>
              <Table.Th>Actor</Table.Th>
              <Table.Th>Target</Table.Th>
              <Table.Th>Outcome</Table.Th>
              <Table.Th>Details</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(page.data?.items ?? []).map((r) => <Row key={r.id} r={r} />)}
            {page.isSuccess && page.data.items.length === 0 && <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed" ta="center" p="md">No matching events.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>
      </Card>
      <Group justify="space-between" mt="sm">
        <Button variant="default" disabled={cursors.length < 2} onClick={() => setCursors(cursors.slice(0, -1))}>Newer</Button>
        <Button variant="default" disabled={!page.data?.next_cursor} onClick={() => page.data?.next_cursor && setCursors([...cursors, page.data.next_cursor])}>Older</Button>
      </Group>
    </>
  )
}

function Row({ r }: { r: AuditRecord }) {
  const hasDetails = r.details && Object.keys(r.details).length > 0
  return (
    <Table.Tr>
      <Table.Td><Text size="xs" style={{ whiteSpace: 'nowrap' }}>{datetime(r.at)}</Text></Table.Td>
      <Table.Td className="mono">{r.action}</Table.Td>
      <Table.Td><Text size="xs">{r.actor_type}</Text><Text size="xs" c="dimmed" className="mono">{r.actor_id}</Text></Table.Td>
      <Table.Td><Text size="xs">{r.target_type}</Text><Text size="xs" c="dimmed" className="mono">{r.target_id}</Text></Table.Td>
      <Table.Td><Badge color={r.outcome === 'success' ? 'owlGreen' : 'owlRed'} variant="light">{r.outcome}</Badge></Table.Td>
      <Table.Td>
        {hasDetails ? (
          <Popover width={420} position="left" withArrow>
            <Popover.Target><Button size="compact-xs" variant="subtle">View</Button></Popover.Target>
            <Popover.Dropdown><Code block className="mono">{JSON.stringify(r.details, null, 2)}</Code></Popover.Dropdown>
          </Popover>
        ) : <Text size="xs" c="dimmed">—</Text>}
      </Table.Td>
    </Table.Tr>
  )
}
