import { Badge, Button, Group, Modal, SegmentedControl, Stack, Table, Text } from '@mantine/core'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Schemas } from '@/api/client'
import { useSpaceHumanTypes } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { useAppForm } from '@/components/form/appForm'
import { notifyError, notifyOk } from '@/lib/notify'
import { typeLabel } from './badges'

type TypePermission = Schemas['HumanTypePermission']

const BUILTIN = ['question', 'approval', 'todo', 'message'] as const
const permKey = (spaceId: string) => ['human-type-permissions', spaceId] as const

/**
 * Who may see, ask with and respond to each human-action type in a space (SM-HUI-008). Roles are minimums over the
 * space's roles and only narrow what the roles already allow; a type without a stored permission uses the defaults.
 * Changing permissions needs the admin role.
 */
export function TypePermissionsModal({ spaceId, canAdmin, onClose }: { spaceId: string; canAdmin: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const list = useQuery({ queryKey: permKey(spaceId),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/human-type-permissions', { params: { path: { space_id: spaceId } } })).items })
  const custom = useSpaceHumanTypes(spaceId).data ?? []
  const [editing, setEditing] = useState<{ ref: string; label: string; current?: TypePermission } | null>(null)
  const stored = list.data ?? []
  const rows: { ref: string; label: string; current?: TypePermission }[] = [
    ...BUILTIN.map((t) => ({ ref: t as string, label: typeLabel[t], current: stored.find((p) => p.action_type === t) })),
    ...custom.map((c) => ({ ref: c.type_id, label: c.label || c.name, current: stored.find((p) => p.action_type === c.type_id) })),
    ...stored.filter((p) => !BUILTIN.includes(p.action_type as (typeof BUILTIN)[number]) && !custom.some((c) => c.type_id === p.action_type))
      .map((p) => ({ ref: p.action_type, label: p.type_slug || p.action_type, current: p })),
  ]
  const remove = async (ref: string) => {
    try {
      const r = await api.DELETE('/spaces/{space_id}/human-type-permissions/{action_type}', { params: { path: { space_id: spaceId, action_type: ref } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: permKey(spaceId) })
      notifyOk('The defaults apply again')
    } catch (e) {
      notifyError(e, 'Could not remove the permission')
    }
  }
  return (
    <Modal opened onClose={onClose} title="Who may see, ask and respond, per type" size="xl">
      <Stack gap="sm">
        <Text size="sm" c="dimmed">
          Without a permission, viewers see a type, operators ask and respond, and any agent may ask. A permission raises those minimums
          (it never widens the roles' own permissions; approvals still need approval:decide). Changing permissions needs the admin role.
        </Text>
        <ErrorAlert error={list.error} />
        <Table.ScrollContainer minWidth={560}>
          <Table aria-label="Type permissions">
            <Table.Thead>
              <Table.Tr><Table.Th>Type</Table.Th><Table.Th>Sees</Table.Th><Table.Th>Asks</Table.Th><Table.Th>Responds</Table.Th><Table.Th>Agents</Table.Th><Table.Th /></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((r) => {
                const p = r.current
                return (
                  <Table.Tr key={r.ref}>
                    <Table.Td><Text size="sm" fw={500}>{r.label}</Text>{p ? <Badge size="xs" color="owlPurple">set</Badge> : <Badge size="xs" variant="outline" color="gray">defaults</Badge>}</Table.Td>
                    <Table.Td>{p?.view_role ?? 'viewer'}</Table.Td>
                    <Table.Td>{p?.create_role ?? 'operator'}</Table.Td>
                    <Table.Td>{p?.respond_role ?? 'operator'}</Table.Td>
                    <Table.Td>{!p || p.agents === 'any' ? 'any' : p.agents === 'none' ? 'none' : p.agent_roles.join(', ')}</Table.Td>
                    <Table.Td>
                      {canAdmin && (
                        <Group gap={4} justify="flex-end" wrap="nowrap">
                          <Button size="xs" variant="default" onClick={() => setEditing(r)} aria-label={`Edit the permission of ${r.label}`}>Edit</Button>
                          {p && <Button size="xs" variant="subtle" color="owlRed" onClick={() => void remove(r.ref)} aria-label={`Reset ${r.label} to the defaults`}>Reset</Button>}
                        </Group>
                      )}
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Stack>
      {editing && <PermissionForm spaceId={spaceId} target={editing} onClose={() => setEditing(null)} />}
    </Modal>
  )
}

function PermissionForm({ spaceId, target, onClose }: { spaceId: string; target: { ref: string; label: string; current?: TypePermission }; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const c = target.current
  const form = useAppForm({
    defaultValues: { view_role: c?.view_role ?? 'viewer', create_role: c?.create_role ?? 'operator', respond_role: c?.respond_role ?? 'operator',
      agents: c?.agents ?? 'any', agent_roles: c?.agent_roles ?? [] } as Required<Schemas['HumanTypePermissionInput']> & { agent_roles: string[] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        // Without If-Match the permission is created; with it, the stored one is updated at its version.
        unwrap(await api.PUT('/spaces/{space_id}/human-type-permissions/{action_type}', {
          params: { path: { space_id: spaceId, action_type: target.ref }, header: c ? { 'If-Match': ifMatch(c.version) } : {} },
          body: { ...value, agent_roles: value.agents === 'roles' ? value.agent_roles : [] } }))
        void qc.invalidateQueries({ queryKey: permKey(spaceId) })
        notifyOk(`${target.label}: permission saved`)
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const roleSelect = (label: string, data: string[]) => ({ label, allowDeselect: false, data: data.map((r) => ({ value: r, label: r })) })
  return (
    <Modal opened onClose={onClose} title={`Permission: ${target.label}`}>
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <form.AppForm>
          <Stack gap="sm">
            <form.AppField name="view_role">{(f) => <f.SelectField {...roleSelect('Weakest role that sees it', ['viewer', 'operator', 'admin'])} />}</form.AppField>
            <form.AppField name="create_role">{(f) => <f.SelectField {...roleSelect('Weakest role that asks with it', ['operator', 'admin'])} />}</form.AppField>
            <form.AppField name="respond_role">
              {(f) => <f.SelectField {...roleSelect('Weakest role that responds', ['operator', 'admin'])} description="Also assigning, commenting, asking for clarification, moving and cancelling" />}
            </form.AppField>
            <form.Field name="agents">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500} id="perm-agents-label">Agents that may ask</Text>
                  <SegmentedControl aria-labelledby="perm-agents-label" value={f.state.value} onChange={(v) => f.handleChange(v as 'any' | 'roles' | 'none')}
                    data={[{ value: 'any', label: 'Any' }, { value: 'roles', label: 'Only these roles' }, { value: 'none', label: 'None' }]} />
                </Stack>
              )}
            </form.Field>
            <form.Subscribe selector={(s) => s.values.agents}>
              {(agents) => agents === 'roles' && (
                <form.AppField name="agent_roles">{(f) => <f.TagsField label="Space roles whose agents may ask" placeholder="e.g. deployer" />}</form.AppField>
              )}
            </form.Subscribe>
            {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
            <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><form.SubmitButton>Save</form.SubmitButton></Group>
          </Stack>
        </form.AppForm>
      </form>
    </Modal>
  )
}

