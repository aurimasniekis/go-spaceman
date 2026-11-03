import { Badge, Button, Card, Code, Drawer, Group, Modal, MultiSelect, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useForm, useSelector } from '@tanstack/react-form'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type SpaceRole } from '@/api/client'
import { keys, useMe, useRoleOptions, useSpaceMcp, useSpaceRoles } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'
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
  const mcp = useSpaceMcp(spaceId).data ?? []
  const qc = useQueryClient()
  const [adding, setAdding] = useState(false)
  // Starter roles (ADR-0051): ready-made control, planner, implementor, reviewer, kb and dreamer roles from the starter templates.
  const addStarters = async () => {
    setAdding(true)
    try {
      const r = unwrap(await api.POST('/spaces/{space_id}/starter-roles', { params: { path: { space_id: spaceId } } }))
      void qc.invalidateQueries({ queryKey: keys.spaceRoles(spaceId) })
      notifyOk(r.added.length > 0 ? `Added ${r.added.join(', ')}` : 'The space already has every starter role')
    } catch (e) {
      notifyError(e, 'Could not add the starter roles')
    } finally {
      setAdding(false)
    }
  }
  return (
    <Card mb="md">
      <Group justify="space-between" mb="xs">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Roles</Text>
        {canWrite && (
          <Group gap="xs">
            <Button size="xs" variant="default" loading={adding} onClick={() => void addStarters()}>Add starter roles</Button>
            <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add role</Button>
          </Group>
        )}
      </Group>
      {(roles.data ?? []).length === 0 ? (
        <Text size="sm" c="dimmed">
          No roles configured. <b>Add starter roles</b> gives the space ready-made control, planner, implementor, reviewer, knowledge-base and
          dreamer roles to launch right away and adjust later. Runs can also be launched with a free role name, a kind and a harness.
        </Text>
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
      {mcp.length > 0 && (
        <Text size="xs" c="dimmed" mt="xs">
          MCP servers: {mcp.map((m) => `${m.name} (${m.role === '*' ? 'every role' : m.role}${m.enabled ? '' : ', disabled'})`).join(' · ')}
        </Text>
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
      {p.skills.length > 0 && (
        <Text size="sm">Skills: {p.skills.map((sk) => `${sk.slug} v${sk.number}${sk.pinned ? ' (pinned)' : ''}`).join(', ')}</Text>
      )}
      <Text size="sm" fw={600}>System text</Text>
      <Code block className="mono" tabIndex={0} aria-label="System text" style={{ whiteSpace: 'pre-wrap' }}>{p.system_text}</Code>
    </Stack>
  )
}

function RoleModal({ spaceId, role, onClose }: { spaceId: string; role?: SpaceRole; onClose: () => void }) {
  const qc = useQueryClient()
  const options = useRoleOptions(spaceId)
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { name: role?.name ?? '', template_id: role?.template_id ?? '', profile_id: role?.profile_id ?? '', prompt_id: role?.prompt_id ?? '',
      prompt_pin: role?.prompt_pin ?? '', model: role?.model ?? '', instructions: role?.instructions ?? '', settings: (role?.settings ?? {}) as Record<string, string>,
      skills: role?.skills ?? [] },
    onSubmit: async ({ value: v }) => {
      setError('')
      try {
        unwrap(await api.PUT('/spaces/{space_id}/roles/{name}', { params: { path: { space_id: spaceId, name: v.name } },
          body: { template_id: v.template_id, profile_id: v.profile_id, prompt_id: v.prompt_id, prompt_pin: v.prompt_pin, model: v.model, settings: v.settings,
            instructions: v.instructions, skills: v.skills } }))
        void qc.invalidateQueries({ queryKey: keys.spaceRoles(spaceId) })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const promptId = useSelector(form.store, (st) => st.values.prompt_id)
  const versions = options.data?.prompts.find((p) => p.id === promptId)?.versions ?? []
  const remove = async () => {
    try {
      const r = await api.DELETE('/spaces/{space_id}/roles/{name}', { params: { path: { space_id: spaceId, name: form.state.values.name } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: keys.spaceRoles(spaceId) })
      onClose()
    } catch (e) {
      notifyError(e, 'Could not remove the role')
    }
  }
  return (
    <Modal opened onClose={onClose} title={role ? `Role ${role.name}` : 'Add a role'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Group grow>
            <form.Field name="name">
              {(f) => <TextInput label="Role name" description="e.g. implementor-backend" disabled={!!role} classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur}
                onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <form.Field name="template_id">
              {(f) => <Select label="Role template" allowDeselect={false} value={f.state.value || null} onChange={(x) => x && f.handleChange(x)}
                data={(options.data?.templates ?? []).map((t) => ({ value: t.id, label: `${t.name} (${t.kind})` }))} />}
            </form.Field>
          </Group>
          <Text size="xs" c="dimmed">Leave the fields below empty to use the template's values.</Text>
          <Group grow>
            <form.Field name="profile_id">
              {(f) => <Select label="Harness profile" clearable value={f.state.value || null} onChange={(x) => f.handleChange(x ?? '')}
                data={(options.data?.profiles ?? []).map((p) => ({ value: p.id, label: p.name }))} />}
            </form.Field>
            <form.Field name="model">{(f) => <TextInput label="Model" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          </Group>
          <Group grow>
            <form.Field name="prompt_id">
              {(f) => <Select label="Prompt" clearable searchable value={f.state.value || null} onChange={(x) => { f.handleChange(x ?? ''); form.setFieldValue('prompt_pin', '') }}
                data={(options.data?.prompts ?? []).map((p) => ({ value: p.id, label: p.name }))} />}
            </form.Field>
            <form.Field name="prompt_pin">
              {(f) => <Select label="Version" allowDeselect={false} disabled={!promptId} value={f.state.value || 'follow'} onChange={(x) => f.handleChange(x === 'follow' || !x ? '' : x)}
                data={[{ value: 'follow', label: 'Follow the latest' }, ...versions.map((x) => ({ value: x.id, label: `Pin v${x.number}` }))]} />}
            </form.Field>
          </Group>
          <form.Field name="skills">
            {(f) => <MultiSelect label="Extra skills" description="Added to the template's skills" searchable value={f.state.value.map((x) => x.skill_id)}
              onChange={(ids) => f.handleChange(ids.map((id) => f.state.value.find((x) => x.skill_id === id) ?? { skill_id: id }))}
              data={(options.data?.skills ?? []).filter((x) => (x.versions ?? []).length > 0).map((x) => ({ value: x.id, label: x.name }))} />}
          </form.Field>
          <form.Field name="settings">{(f) => <SettingsEditor value={f.state.value} onChange={(settings) => f.handleChange(settings)} />}</form.Field>
          <form.Field name="instructions">
            {(f) => <Textarea label="Instructions for this role in this space" description="Appended after the role's prompt" autosize minRows={2} value={f.state.value}
              onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="space-between">
            {role ? <Button variant="subtle" color="owlRed" onClick={() => void remove()}>Remove role</Button> : <span />}
            <Group gap="xs">
              <Button variant="default" onClick={onClose}>Cancel</Button>
              <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save</Button>}</form.Subscribe>
            </Group>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
