import { Alert, Badge, Button, Card, Group, Modal, SimpleGrid, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconTrashX } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap } from '@/api/client'
import { ErrorAlert } from '@/components/ErrorAlert'
import { limitValue } from '@/features/limits/format'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const retentionKey = ['system', 'retention'] as const

const KIND_LABEL: Record<string, string> = {
  recordings: 'recordings',
  transcripts: 'transcripts',
  attachments: 'attachments',
  human_actions: 'human actions',
  audit_events: 'audit events',
}

const stateColor: Record<string, string> = { running: 'owlBlue', succeeded: 'owlGreen', failed: 'owlRed', interrupted: 'owlYellow' }

/** What one pass deleted, in words: "3 recordings, 1 attachment" or "nothing". */
export function deletedSummary(d: Record<string, number> | undefined): string {
  const parts = Object.entries(d ?? {}).filter(([, n]) => n > 0).map(([k, n]) => `${n} ${KIND_LABEL[k] ?? k}`)
  return parts.length ? parts.join(', ') : 'nothing'
}

/**
 * Retention (ADR-0045): the installation defaults of the per-space retention periods, audit retention, the next daily
 * pass and recent passes, with a manual pass for administrators. Spaces override the periods in their limits.
 */
export function RetentionCard() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: retentionKey, queryFn: async ({ signal }) => unwrap(await api.GET('/retention', { signal })) })
  const [confirming, setConfirming] = useState(false)
  const [running, setRunning] = useState(false)
  const s = q.data
  const run = async () => {
    setRunning(true)
    try {
      const p = unwrap(await api.POST('/retention/run'))
      if (p.state === 'succeeded') notifyOk(`Retention pass done: deleted ${deletedSummary(p.deleted)}`)
      else notifyError(new Error(p.error || p.state), 'Retention pass did not finish cleanly')
      setConfirming(false)
      void qc.invalidateQueries({ queryKey: retentionKey })
    } catch (e) {
      notifyError(e)
    } finally {
      setRunning(false)
    }
  }
  return (
    <Card mb="md">
      <Stack gap="sm">
        <Group justify="space-between">
          <Group gap="xs">
            <Text fw={600}>Retention</Text>
            {s?.busy && <Badge color="owlBlue">running</Badge>}
          </Group>
          <Button size="xs" variant="default" color="owlRed" leftSection={<IconTrashX size={12} />} disabled={!s || s.busy || !!s.blocked}
            onClick={() => setConfirming(true)}>Run a pass now</Button>
        </Group>
        <ErrorAlert error={q.error} />
        {s?.blocked && <Alert color="owlYellow" variant="light" title="Retention is paused">{s.blocked}</Alert>}
        {s && (
          <>
            <SimpleGrid cols={{ base: 2, sm: 3, md: 6 }}>
              {s.defaults.map((d) => (
                <Stack key={d.key} gap={2}>
                  <Text size="xs" c="dimmed">{d.label}</Text>
                  <Text fw={600} className="mono">{limitValue(d.days, 'days', true)}</Text>
                </Stack>
              ))}
              <Stack gap={2}>
                <Text size="xs" c="dimmed">Audit retention</Text>
                <Text fw={600} className="mono">{limitValue(s.audit_days, 'days', true)}</Text>
              </Stack>
              <Stack gap={2}>
                <Text size="xs" c="dimmed">Next daily pass</Text>
                <Text fw={600} className="mono">{s.next_at ? ago(s.next_at) : '—'}</Text>
              </Stack>
            </SimpleGrid>
            <Text size="xs" c="dimmed">
              Installation defaults; each space can override its periods on its Limits page. One pass a day deletes what is older. It never deletes
              what an open item uses, every deletion is audited, and nothing runs while a restore is not acknowledged.
            </Text>
            {s.passes.length > 0 && (
              <Table>
                <Table.Thead>
                  <Table.Tr><Table.Th>Pass</Table.Th><Table.Th>State</Table.Th><Table.Th>Started</Table.Th><Table.Th>Deleted</Table.Th></Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {s.passes.slice(0, 5).map((p) => (
                    <Table.Tr key={p.id}>
                      <Table.Td><Text size="sm" className="mono">{p.id}</Text><Text size="xs" c="dimmed">{p.trigger}</Text></Table.Td>
                      <Table.Td>
                        {p.error ? <Tooltip label={p.error} multiline w={360}><Badge color={stateColor[p.state]}>{p.state}</Badge></Tooltip>
                          : <Badge color={stateColor[p.state]}>{p.state}</Badge>}
                      </Table.Td>
                      <Table.Td><Text size="sm">{datetime(p.started_at)}</Text></Table.Td>
                      <Table.Td><Text size="sm">{deletedSummary(p.deleted)}</Text></Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
          </>
        )}
      </Stack>
      <Modal opened={confirming} onClose={() => setConfirming(false)} title="Run a retention pass now?">
        <Stack gap="sm">
          <Text size="sm">
            Everything older than its retention period is deleted now, in every space: recordings, transcripts, attachments of finished items,
            finished human actions and, when audit retention is set, old audit events. Deleted data comes back only from a backup.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirming(false)}>Cancel</Button>
            <Button color="owlRed" loading={running} onClick={() => void run()}>Delete expired data</Button>
          </Group>
        </Stack>
      </Modal>
    </Card>
  )
}
