import { Badge, Button, Card, Code, Drawer, Group, Modal, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type SpaceRole } from '@/api/client'
import { keys, useMe, useRoleOptions, useSpaceRoles } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError } from '@/lib/notify'
import { SettingsEditor } from './SettingsEditor'

const originColor: Record<string, string> = { builtin: 'gray', profile: 'owlBlue', template: 'owlPurple', space_role: 'owlGreen', launch: 'owlYellow', policy: 'owlRed' }

/** The roles configured in a space (SM-PROMPT-002/003): template, overrides and a preview of what a launch resolves to. */
export function SpaceRolesCard({ spaceId }: { spaceId: string }) {
  const roles = useSpaceRoles(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const options = useRoleOptions(spaceId)
  const [editing, setEditing] = useState<SpaceRole | 'new' | null>(null)
  const [preview, setPreview] = useState<string | null>(null)
  const tplName = (id: string) => options.data?.templates.find((t) => t.id === id)?.name ?? id
  return (
    <Card mb="md">
      <Group justify="space-between" mb="xs">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Roles</Text>
        {canWrite && <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add role</Button>}
      </Group>
      {(roles.data ?? []).length === 0 ? (
        <Text size="sm" c="dimmed">No roles configured. Runs can still be launched with a free role name, a kind and a harness.</Text>
      ) : (
        <Table>
          <Table.Tbody>
            {(roles.data ?? []).map((r) => (
              <Table.Tr key={r.name}>
                <Table.Td><Text size="sm" fw={500} className="mono">{r.name}</Text></Table.Td>
                <Table.Td><Text size="sm">{tplName(r.template_id)}</Text></Table.Td>
                <Table.Td><Text size="xs" c="dimmed">revision {r.revision}</Text></Table.Td>
                <Table.Td>
                  <Group gap={4} justify="flex-end">
                    <Button size="xs" variant="subtle" onClick={() => setPreview(r.name)}>Effective configuration</Button>
                    {canWrite && <Button size="xs" variant="subtle" onClick={() => setEditing(r)}>Edit</Button>}
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {editing && <RoleModal spaceId={spaceId} role={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} />}
      <Drawer opened={preview !== null} onClose={() => setPreview(null)} position="right" size="xl" title={`Role ${preview ?? ''}`}>
        {preview && <RolePreview spaceId={spaceId} name={preview} />}
      </Drawer>
    </Card>
  )
}

function RolePreview({ spaceId, name }: { spaceId: string; name: string }) {
  const q = useQuery({ queryKey: ['role-preview', spaceId, name],
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/roles/{name}/preview', { params: { path: { space_id: spaceId, name } } })) })
  if (q.error) return <ErrorAlert error={q.error} />
  if (!q.data) return null
  const p = q.data
  return (
    <Stack gap="sm">
      <Text size="sm" c="dimmed">What a launch of this role gets now, and where each value comes from. Later layers override earlier ones; policy ceilings apply last.</Text>
      <Table>
        <Table.Thead><Table.Tr><Table.Th>Field</Table.Th><Table.Th>Value</Table.Th><Table.Th>From</Table.Th></Table.Tr></Table.Thead>
        <Table.Tbody>
          {Object.entries(p.effective).sort(([a], [b]) => a.localeCompare(b)).map(([k, f]) => (
            <Table.Tr key={k}>
              <Table.Td><Text size="xs" className="mono">{k}</Text></Table.Td>
              <Table.Td><Text size="xs" className="mono">{f.value.length > 40 ? `${f.value.slice(0, 40)}…` : f.value}</Text></Table.Td>
              <Table.Td><Badge size="xs" variant="light" color={originColor[f.origin]}>{f.origin.replace('_', ' ')}</Badge></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {p.prompt && <Text size="sm">Prompt <b>{p.prompt.slug}</b> v{p.prompt.number} ({p.prompt.pinned ? 'pinned' : 'follows the latest'})</Text>}
      <Text size="sm" fw={600}>System text</Text>
      <Code block className="mono" tabIndex={0} aria-label="System text" style={{ whiteSpace: 'pre-wrap' }}>{p.system_text}</Code>
    </Stack>
  )
}

function RoleModal({ spaceId, role, onClose }: { spaceId: string; role?: SpaceRole; onClose: () => void }) {
  const qc = useQueryClient()
  const options = useRoleOptions(spaceId)
  const [v, setV] = useState({ name: role?.name ?? '', template_id: role?.template_id ?? '', profile_id: role?.profile_id ?? '', prompt_id: role?.prompt_id ?? '',
    prompt_pin: role?.prompt_pin ?? '', model: role?.model ?? '', instructions: role?.instructions ?? '', settings: role?.settings ?? {} })
  const [error, setError] = useState('')
  const versions = options.data?.prompts.find((p) => p.id === v.prompt_id)?.versions ?? []
  const save = async () => {
    try {
      unwrap(await api.PUT('/spaces/{space_id}/roles/{name}', { params: { path: { space_id: spaceId, name: v.name } },
        body: { template_id: v.template_id, profile_id: v.profile_id, prompt_id: v.prompt_id, prompt_pin: v.prompt_pin, model: v.model, settings: v.settings,
          instructions: v.instructions } }))
      void qc.invalidateQueries({ queryKey: keys.spaceRoles(spaceId) })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  const remove = async () => {
    try {
      const r = await api.DELETE('/spaces/{space_id}/roles/{name}', { params: { path: { space_id: spaceId, name: v.name } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: keys.spaceRoles(spaceId) })
      onClose()
    } catch (e) {
      notifyError(e, 'Could not remove the role')
    }
  }
  return (
    <Modal opened onClose={onClose} title={role ? `Role ${role.name}` : 'Add a role'} size="lg">
      <Stack gap="sm">
        <Group grow>
          <TextInput label="Role name" description="e.g. implementor-backend" disabled={!!role} classNames={{ input: 'mono' }} value={v.name} onChange={(e) => setV({ ...v, name: e.target.value })} />
          <Select label="Role template" allowDeselect={false} value={v.template_id || null} onChange={(x) => x && setV({ ...v, template_id: x })}
            data={(options.data?.templates ?? []).map((t) => ({ value: t.id, label: `${t.name} (${t.kind})` }))} />
        </Group>
        <Text size="xs" c="dimmed">Leave the fields below empty to use the template's values.</Text>
        <Group grow>
          <Select label="Harness profile" clearable value={v.profile_id || null} onChange={(x) => setV({ ...v, profile_id: x ?? '' })}
            data={(options.data?.profiles ?? []).map((p) => ({ value: p.id, label: p.name }))} />
          <TextInput label="Model" classNames={{ input: 'mono' }} value={v.model} onChange={(e) => setV({ ...v, model: e.target.value })} />
        </Group>
        <Group grow>
          <Select label="Prompt" clearable searchable value={v.prompt_id || null} onChange={(x) => setV({ ...v, prompt_id: x ?? '', prompt_pin: '' })}
            data={(options.data?.prompts ?? []).map((p) => ({ value: p.id, label: p.name }))} />
          <Select label="Version" allowDeselect={false} disabled={!v.prompt_id} value={v.prompt_pin || 'follow'} onChange={(x) => setV({ ...v, prompt_pin: x === 'follow' || !x ? '' : x })}
            data={[{ value: 'follow', label: 'Follow the latest' }, ...versions.map((x) => ({ value: x.id, label: `Pin v${x.number}` }))]} />
        </Group>
        <SettingsEditor value={v.settings} onChange={(settings) => setV({ ...v, settings })} />
        <Textarea label="Instructions for this role in this space" description="Appended after the role's prompt" autosize minRows={2} value={v.instructions}
          onChange={(e) => setV({ ...v, instructions: e.target.value })} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="space-between">
          {role ? <Button variant="subtle" color="owlRed" onClick={() => void remove()}>Remove role</Button> : <span />}
          <Group gap="xs"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
        </Group>
      </Stack>
    </Modal>
  )
}
