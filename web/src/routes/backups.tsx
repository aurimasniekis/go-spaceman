import { Alert, Badge, Button, Card, Group, List, SimpleGrid, Stack, Table, Text, Tooltip, VisuallyHidden } from '@mantine/core'
import { IconDatabaseExport, IconShieldCheck } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type BackupItem, type BackupStatus, type RestoreNotice } from '@/api/client'
import { keys, useBackups, useMe, useRestoreNotice } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { hasGlobal } from '@/lib/access'
import { ago, bytes, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/backups')({ component: BackupsPage })

const stateColor: Record<BackupItem['state'], string> = {
  running: 'owlBlue',
  succeeded: 'owlGreen',
  failed: 'owlRed',
  interrupted: 'owlYellow',
  deleted: 'gray',
}

function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed">{label}</Text>
      <Text fw={600} className="mono">{value}</Text>
    </Stack>
  )
}

function VerifyBadge({ it }: { it: BackupItem }) {
  if (it.verify_state === '') return <Text size="sm" c="dimmed">not verified</Text>
  const color = it.verify_state === 'ok' ? 'owlGreen' : it.verify_state === 'running' ? 'owlBlue' : 'owlRed'
  const badge = <Badge color={color} variant="light">{it.verify_state === 'ok' ? `verified ${ago(it.verified_at)}` : it.verify_state}</Badge>
  return it.verify_error ? <Tooltip label={it.verify_error} multiline w={360}>{badge}</Tooltip> : badge
}

function BackupsPage() {
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const q = useBackups(admin)
  const restore = useRestoreNotice(admin)
  const qc = useQueryClient()
  const [starting, setStarting] = useState(false)
  const s = q.data

  const create = async () => {
    setStarting(true)
    try {
      const it = unwrap(await api.POST('/system/backups'))
      notifyOk(`Backup ${it.id} started`)
      void qc.invalidateQueries({ queryKey: keys.backups })
    } catch (e) {
      notifyError(e)
    } finally {
      setStarting(false)
    }
  }
  const verify = async (id: string) => {
    try {
      unwrap(await api.POST('/system/backups/{backup_id}/verify', { params: { path: { backup_id: id } } }))
      notifyOk(`Verifying ${id}`)
      void qc.invalidateQueries({ queryKey: keys.backups })
    } catch (e) {
      notifyError(e)
    }
  }

  return (
    <>
      <PageHeader title="Backups" description="Sealed backups of the database, role memory, recordings, attachments and transcripts. Restoring runs offline with spaceman backup restore."
        actions={<Button leftSection={<IconDatabaseExport size={14} />} loading={starting} disabled={!s || s.busy} onClick={() => void create()}>Back up now</Button>} />
      <ErrorAlert error={q.error} />
      {restore.data?.restored && <RestoreCard n={restore.data} />}
      {s && <Overview s={s} />}
      {s && (
        <Card p={0} mb="md">
          <Table>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Backup</Table.Th>
                <Table.Th>State</Table.Th>
                <Table.Th>Taken</Table.Th>
                <Table.Th>Files</Table.Th>
                <Table.Th>Size</Table.Th>
                <Table.Th>Verification</Table.Th>
                <Table.Th><VisuallyHidden>Actions</VisuallyHidden></Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {s.items.map((it) => (
                <Table.Tr key={it.id}>
                  <Table.Td>
                    <Text size="sm" className="mono">{it.id}</Text>
                    <Text size="xs" c="dimmed">{it.trigger}{it.requested_by ? ` · ${it.requested_by}` : ''}{it.stored ? '' : ' · not in the target'}</Text>
                  </Table.Td>
                  <Table.Td>
                    {it.error ? <Tooltip label={it.error} multiline w={360}><Badge color={stateColor[it.state]}>{it.state}</Badge></Tooltip>
                      : <Badge color={stateColor[it.state]}>{it.state}</Badge>}
                  </Table.Td>
                  <Table.Td><Text size="sm">{datetime(it.created_at ?? it.started_at)}</Text></Table.Td>
                  <Table.Td><Text size="sm">{it.files || '—'}</Text></Table.Td>
                  <Table.Td><Text size="sm">{it.stored_bytes ? bytes(it.stored_bytes) : '—'}</Text></Table.Td>
                  <Table.Td><VerifyBadge it={it} /></Table.Td>
                  <Table.Td>
                    <Button size="xs" variant="default" leftSection={<IconShieldCheck size={12} />} disabled={!it.stored || s.busy}
                      aria-label={`Verify ${it.id}`} onClick={() => void verify(it.id)}>Verify</Button>
                  </Table.Td>
                </Table.Tr>
              ))}
              {s.items.length === 0 && (
                <Table.Tr><Table.Td colSpan={7}><Text size="sm" c="dimmed" ta="center" p="md">No backups yet.</Text></Table.Td></Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Card>
      )}
      {s && <Scope s={s} />}
    </>
  )
}

function Overview({ s }: { s: BackupStatus }) {
  return (
    <Card mb="md">
      <Stack>
        <Group justify="space-between">
          <Group gap="xs">
            <Text fw={600}>Target</Text>
            <Badge color="owlBlue" variant="light">{s.target}</Badge>
            {s.busy && <Badge color="owlBlue">working</Badge>}
          </Group>
          <Text size="xs" c="dimmed" className="mono">{s.location}</Text>
        </Group>
        {s.target_error && <Alert color="owlRed" title="The target could not be listed">{s.target_error}</Alert>}
        <SimpleGrid cols={{ base: 2, sm: 4 }}>
          <Stat label="Schedule" value={s.schedule ? `every ${s.interval}` : 'off (manual only)'} />
          <Stat label="Next scheduled" value={s.next_at ? ago(s.next_at) : '—'} />
          <Stat label="Retention" value={`newest ${s.keep_last}${s.max_age ? `, max age ${s.max_age}` : ''}`} />
          <Stat label="Complete backups" value={s.items.filter((i) => i.stored).length} />
        </SimpleGrid>
        <Text size="xs" c="dimmed">
          Every file is sealed under the key-encryption key (KEK). The KEK is never part of a backup: keep it backed up separately, or the backups cannot be restored.
        </Text>
      </Stack>
    </Card>
  )
}

function Scope({ s }: { s: BackupStatus }) {
  return (
    <SimpleGrid cols={{ base: 1, md: 2 }}>
      <Card>
        <Text fw={600} mb="xs">Included</Text>
        <List size="sm" spacing={4}>{s.included.map((x) => <List.Item key={x}>{x}</List.Item>)}</List>
      </Card>
      <Card>
        <Text fw={600} mb="xs">Not included</Text>
        <List size="sm" spacing={4}>{s.excluded.map((x) => <List.Item key={x}>{x}</List.Item>)}</List>
      </Card>
    </SimpleGrid>
  )
}

function RestoreCard({ n }: { n: RestoreNotice }) {
  const r = n.state
  if (!r) return null
  return (
    <Card mb="md">
      <Stack gap="xs">
        <Group gap="xs">
          <Text fw={600}>Last restore</Text>
          <Badge color={n.still_paused > 0 ? 'owlYellow' : 'owlGreen'}>{n.still_paused > 0 ? `${n.still_paused} space(s) still paused` : 'all spaces resumed'}</Badge>
        </Group>
        <Text size="sm">
          Restored backup <code>{r.backup_id}</code> (taken {datetime(r.backup_created_at)}), applied {datetime(r.applied_at)}.
        </Text>
        <SimpleGrid cols={{ base: 2, sm: 4 }}>
          <Stat label="Spaces paused" value={r.paused_spaces.length} />
          <Stat label="Leases renewed" value={r.leases_renewed} />
          <Stat label="Runs marked unknown" value={r.runs_unknown} />
          <Stat label="Commands cancelled" value={r.commands_cancelled} />
          <Stat label="Subscriptions replaying" value={r.subscriptions_reset} />
          <Stat label="Approvals revoked" value={r.approvals_revoked} />
          <Stat label="Approvals still pending" value={r.pending_approvals} />
          <Stat label="Open human actions" value={r.open_human_actions} />
        </SimpleGrid>
      </Stack>
    </Card>
  )
}
