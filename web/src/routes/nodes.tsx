import { Badge, Button, Code, Drawer, Group, Menu, Modal, Stack, Text, TextInput, Tooltip } from '@mantine/core'
import { IconDots, IconPlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type Node } from '@/api/client'
import { keys, useMe, useNodes } from '@/api/queries'
import { DataTable, type ColumnDef } from '@/components/DataTable'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SecretModal } from '@/components/SecretModal'
import { NodeState } from '@/components/StatusBadge'
import { capabilitiesStale, NodeHarnesses } from '@/features/nodes/NodeHarnesses'
import { hasGlobal } from '@/lib/access'
import { ago, datetime, isSet } from '@/lib/format'
import { fieldError } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/nodes')({ component: NodesPage })

const NODE_RE = /^[a-z0-9][a-z0-9-]{0,62}$/

function NodesPage() {
  const nodes = useNodes()
  const me = useMe().data
  const qc = useQueryClient()
  const admin = hasGlobal(me, 'admin')
  const [creating, setCreating] = useState(false)
  const [enrollment, setEnrollment] = useState<{ token: string; node: string; expires: string } | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const detail = nodes.data?.find((n) => n.id === selected)

  const reissue = async (n: Node) => {
    try {
      const e = unwrap(await api.POST('/nodes/{node_id}/enrollment', { params: { path: { node_id: n.id } } }))
      setEnrollment({ token: e.token, node: n.name, expires: e.expires_at })
    } catch (err) {
      notifyError(err, 'Could not issue an enrollment token')
    }
  }
  const setState = async (n: Node, state: 'active' | 'disabled' | 'revoked') => {
    try {
      unwrap(await api.POST('/nodes/{node_id}/state', { params: { path: { node_id: n.id } }, body: { state } }))
      void qc.invalidateQueries({ queryKey: keys.nodes })
      notifyOk(`Node ${n.name} ${state}`)
    } catch (err) {
      notifyError(err, 'Could not change the node state')
    }
  }

  return (
    <>
      <PageHeader title="Nodes" description="Machines running spaceman-agent. Each space is pinned to one node."
        actions={admin && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>Add node</Button>} />
      <ErrorAlert error={nodes.error} />
      <DataTable view="nodes" data={nodes.data ?? []} getRowId={(n) => n.id} filterLabel="Filter nodes" caption="Nodes"
        onRowClick={(n) => setSelected(n.id)} rowLabel={(n) => `Node ${n.name}`}
        defaultSort={[{ id: 'name', desc: false }]} empty="No nodes yet. Add one, then enroll spaceman-agent on it." columns={[
          {
            id: 'name', header: 'Name', accessorFn: (n) => n.name,
            cell: ({ row: { original: n } }) => <><Text size="sm" fw={500}>{n.name}</Text><Text size="xs" c="dimmed" className="mono">{n.id}</Text></>,
          },
          { id: 'state', header: 'State', accessorFn: (n) => `${n.state} ${n.connected ? 'connected' : 'offline'}`, cell: ({ row: { original: n } }) => <NodeState node={n} /> },
          {
            id: 'agent', header: 'Agent', accessorFn: (n) => n.agent_version,
            cell: ({ row: { original: n } }) => <><Text size="sm" className="mono">{n.agent_version || '—'}</Text><Text size="xs" c="dimmed">{n.capabilities.os}/{n.capabilities.arch}</Text></>,
          },
          {
            id: 'runner', header: 'Runner', accessorFn: (n) => `${n.capabilities.runner.kind} ${n.capabilities.runner.version}`,
            cell: ({ row: { original: n } }) => n.capabilities.runner.kind ? (
              <Tooltip label={n.capabilities.runner.error || `core ${n.capabilities.runner.core_id}`}>
                <Badge color={n.capabilities.runner.error ? 'owlRed' : 'gray'} variant="outline">{n.capabilities.runner.kind} {n.capabilities.runner.version}</Badge>
              </Tooltip>
            ) : '—',
          },
          {
            id: 'harnesses', header: 'Harnesses', enableSorting: false, accessorFn: (n) => n.capabilities.harnesses.map((h) => h.kind).join(' '),
            cell: ({ row: { original: n } }) => (
              <Group gap={4}>
                {n.capabilities.harnesses.map((h) => (
                  <Tooltip key={h.kind} label={h.available ? `${h.version}${h.managed ? ` (managed, ${h.channel})` : ''} · ${h.capabilities.join(', ')}` : h.error || 'not available'}>
                    <Badge color={h.available ? 'owlBlue' : 'gray'} variant={h.available ? 'light' : 'outline'}>{h.kind}</Badge>
                  </Tooltip>
                ))}
                {n.capabilities.harnesses.length === 0 && <Text size="xs" c="dimmed">—</Text>}
                {capabilitiesStale(n) && <Tooltip label="The last capability report is over an hour old"><Badge color="owlYellow" variant="light">stale</Badge></Tooltip>}
              </Group>
            ),
          },
          {
            id: 'seen', header: 'Last seen', accessorFn: (n) => (isSet(n.last_seen_at) ? Date.parse(n.last_seen_at!) : 0), sortDescFirst: true,
            cell: ({ row: { original: n } }) => <Tooltip label={datetime(n.last_seen_at)}><Text size="xs" c="dimmed">{isSet(n.last_seen_at) ? ago(n.last_seen_at) : 'never'}</Text></Tooltip>,
          },
          ...(admin ? [{
            id: 'actions', header: '', enableSorting: false, size: 40,
            cell: ({ row: { original: n } }: { row: { original: Node } }) => (
              // The menu (and its portal) sits inside the clickable row: keep its clicks and keys from opening the detail.
              <div onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
                <Menu position="bottom-end">
                  <Menu.Target><Button variant="subtle" size="compact-xs" aria-label={`Actions for ${n.name}`}><IconDots size={14} /></Button></Menu.Target>
                  <Menu.Dropdown>
                    <Menu.Item onClick={() => void reissue(n)} disabled={n.state === 'revoked'}>New enrollment token</Menu.Item>
                    {n.state === 'disabled' ? (
                      <Menu.Item onClick={() => void setState(n, 'active')}>Enable</Menu.Item>
                    ) : (
                      <Menu.Item onClick={() => void setState(n, 'disabled')} disabled={n.state === 'revoked'}>Disable</Menu.Item>
                    )}
                    <Menu.Item color="owlRed" onClick={() => void setState(n, 'revoked')} disabled={n.state === 'revoked'}>Revoke credential</Menu.Item>
                  </Menu.Dropdown>
                </Menu>
              </div>
            ),
          } satisfies ColumnDef<Node, unknown>] : []),
        ] satisfies ColumnDef<Node, unknown>[]} />
      <Drawer opened={!!detail} onClose={() => setSelected(null)} position="right" size="lg" title={detail ? `Node ${detail.name}` : 'Node'}>
        {detail && <NodeHarnesses node={detail} />}
      </Drawer>
      <CreateNodeModal opened={creating} onClose={() => setCreating(false)} onCreated={(token, node, expires) => setEnrollment({ token, node, expires })} />
      <SecretModal title={`Enrollment token for ${enrollment?.node ?? ''}`} secret={enrollment?.token ?? null} onClose={() => setEnrollment(null)}>
        <Text size="sm">On the node, with <Code>central.url</Code> set in its configuration, run:</Text>
        <Code block className="mono">{`spaceman-agent enroll --token '${enrollment?.token ?? ''}'`}</Code>
        <Text size="xs" c="dimmed">Single use; expires {enrollment ? datetime(enrollment.expires) : ''}.</Text>
      </SecretModal>
    </>
  )
}

function CreateNodeModal({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (token: string, node: string, expires: string) => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const form = useForm({
    defaultValues: { name: '', description: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const e = unwrap(await api.POST('/nodes', { params: { header: { 'Idempotency-Key': key } }, body: value }))
        void qc.invalidateQueries({ queryKey: keys.nodes })
        setKey(idempotencyKey())
        form.reset()
        onClose()
        onCreated(e.token, e.node.name, e.expires_at)
      } catch (err) {
        setError(errorMessage(err))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title="Add node">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="name" validators={{ onBlur: ({ value }) => (!NODE_RE.test(value) ? 'Lowercase letters, digits and dashes' : undefined) }}>
            {(f) => <TextInput label="Name" description="DNS-like, unique" classNames={{ input: "mono" }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="description">
            {(f) => <TextInput label="Description" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Create and issue token</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
