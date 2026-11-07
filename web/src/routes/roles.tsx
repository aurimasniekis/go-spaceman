import { Badge, Button, Card, Group, Modal, MultiSelect, Select, Stack, Table, Tabs, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useForm, useSelector } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Profile, type RoleTemplate } from '@/api/client'
import { keys, useMe, useProfiles, usePrompt, usePrompts, useRoleTemplates, useSkills } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ClaudeSettingsEditor } from '@/features/catalog/ClaudeSettingsEditor'
import { claudeSettingsForm, claudeSettingsKeys, claudeSettingsValue } from '@/features/catalog/claudeSettings'
import { SettingsEditor } from '@/features/catalog/SettingsEditor'
import { hasGlobal } from '@/lib/access'

export const Route = createFileRoute('/roles')({ component: RolesPage })

/** Role templates and harness profiles (SM-PROMPT-002): reusable configuration that space roles build on. */
function RolesPage() {
  const canWrite = hasGlobal(useMe().data, 'admin')
  const templates = useRoleTemplates()
  const profiles = useProfiles()
  const prompts = usePrompts()
  const [editT, setEditT] = useState<RoleTemplate | 'new' | null>(null)
  const [editP, setEditP] = useState<Profile | 'new' | null>(null)
  const promptName = (id?: string) => prompts.data?.find((p) => p.id === id)?.name ?? ''
  const profileName = (id?: string) => profiles.data?.find((p) => p.id === id)?.name ?? ''
  return (
    <>
      <PageHeader title="Roles & profiles" description="Role templates define what a role does (its prompt and memory conventions); harness profiles define how it runs (harness, model, settings). Spaces configure roles from them." />
      <Tabs defaultValue="templates">
        <Tabs.List mb="sm"><Tabs.Tab value="templates">Role templates</Tabs.Tab><Tabs.Tab value="profiles">Harness profiles</Tabs.Tab></Tabs.List>
        <Tabs.Panel value="templates">
          {canWrite && <Group justify="flex-end" mb="xs"><Button leftSection={<IconPlus size={14} />} onClick={() => setEditT('new')}>New role template</Button></Group>}
          <ErrorAlert error={templates.error} />
          <Card p={0}>
            <Table highlightOnHover>
              <Table.Thead><Table.Tr><Table.Th>Template</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Prompt</Table.Th><Table.Th>Profile</Table.Th><Table.Th>Revision</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(templates.data ?? []).map((t) => (
                  <Table.Tr key={t.id} style={{ cursor: canWrite ? 'pointer' : undefined }} onClick={() => canWrite && setEditT(t)}>
                    <Table.Td><Text size="sm" fw={500}>{t.name}</Text><Text size="xs" c="dimmed" className="mono">{t.slug}</Text></Table.Td>
                    <Table.Td><Badge variant="light">{t.kind}</Badge></Table.Td>
                    <Table.Td><Text size="sm">{promptName(t.prompt_id) || '—'}</Text>{t.prompt_id && <Text size="xs" c="dimmed">{t.prompt_pin ? 'pinned' : 'follows latest'}</Text>}</Table.Td>
                    <Table.Td><Text size="sm">{profileName(t.profile_id) || '—'}</Text></Table.Td>
                    <Table.Td>{t.revision}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            {templates.isSuccess && templates.data.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No role templates yet.</Text>}
          </Card>
        </Tabs.Panel>
        <Tabs.Panel value="profiles">
          {canWrite && <Group justify="flex-end" mb="xs"><Button leftSection={<IconPlus size={14} />} onClick={() => setEditP('new')}>New profile</Button></Group>}
          <ErrorAlert error={profiles.error} />
          <Card p={0}>
            <Table highlightOnHover>
              <Table.Thead><Table.Tr><Table.Th>Profile</Table.Th><Table.Th>Harness</Table.Th><Table.Th>Model</Table.Th><Table.Th>Settings</Table.Th><Table.Th>Revision</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(profiles.data ?? []).map((p) => (
                  <Table.Tr key={p.id} style={{ cursor: canWrite ? 'pointer' : undefined }} onClick={() => canWrite && setEditP(p)}>
                    <Table.Td><Text size="sm" fw={500}>{p.name}</Text><Text size="xs" c="dimmed" className="mono">{p.slug}</Text></Table.Td>
                    <Table.Td><Badge variant="light">{p.harness === 'codex' ? 'Codex' : 'Claude Code'}</Badge></Table.Td>
                    <Table.Td><Text size="sm" className="mono">{p.model || '—'}</Text></Table.Td>
                    <Table.Td>
                      <Text size="xs" className="mono">{Object.entries(p.settings).map(([k, v]) => `${k}=${v}`).join(' ') || '—'}</Text>
                      {claudeSettingsKeys(p.claude_settings).length > 0 && (
                        <Text size="xs" c="dimmed" className="mono">settings.json: {claudeSettingsKeys(p.claude_settings).join(', ')}</Text>
                      )}
                    </Table.Td>
                    <Table.Td>{p.revision}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            {profiles.isSuccess && profiles.data.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No harness profiles yet.</Text>}
          </Card>
        </Tabs.Panel>
      </Tabs>
      {editT && <TemplateModal t={editT === 'new' ? undefined : editT} onClose={() => setEditT(null)} />}
      {editP && <ProfileModal p={editP === 'new' ? undefined : editP} onClose={() => setEditP(null)} />}
    </>
  )
}

const submitOn = (handle: () => Promise<void>) => (e: React.FormEvent) => {
  e.preventDefault()
  void handle()
}

function ProfileModal({ p, onClose }: { p?: Profile; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [claudeError, setClaudeError] = useState('')
  const form = useForm({
    defaultValues: { slug: p?.slug ?? '', name: p?.name ?? '', harness: (p?.harness ?? 'claude-code') as Profile['harness'], model: p?.model ?? '', description: p?.description ?? '',
      settings: (p?.settings ?? {}) as Record<string, string>, claude: claudeSettingsForm(p?.claude_settings) },
    onSubmit: async ({ value }) => {
      setError('')
      const { claude, ...rest } = value
      setClaudeError('')
      const cs = rest.harness === 'claude-code' ? claudeSettingsValue(claude) : undefined
      if (cs && 'error' in cs) {
        setClaudeError(cs.error)
        return
      }
      try {
        unwrap(await api.PUT('/harness-profiles/{profile_id}', { params: { path: { profile_id: p?.id ?? 'new' }, header: { 'If-Match': ifMatch(p?.version ?? 0) } },
          body: { ...rest, claude_settings: cs?.value } }))
        void qc.invalidateQueries({ queryKey: keys.profiles })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const harness = useSelector(form.store, (st) => st.values.harness)
  // Profiles a role template or space role uses cannot be deleted; the server says which (ADR-0051).
  const remove = async () => {
    if (!p) return
    setError('')
    try {
      const r = await api.DELETE('/harness-profiles/{profile_id}', { params: { path: { profile_id: p.id } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: keys.profiles })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={p ? `Edit ${p.name}` : 'New harness profile'} size="lg">
      <form onSubmit={submitOn(form.handleSubmit)}>
        <Stack gap="sm">
          <Group grow>
            <form.Field name="name">{(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            <form.Field name="slug">{(f) => <TextInput label="Slug" disabled={!!p} classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          </Group>
          <Group grow>
            <form.Field name="harness">
              {(f) => <Select label="Harness" disabled={!!p} allowDeselect={false} value={f.state.value} onChange={(x) => x && f.handleChange(x as Profile['harness'])}
                data={[{ value: 'claude-code', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }]} />}
            </form.Field>
            <form.Field name="model">
              {(f) => <TextInput label="Model" description="Harness default when empty" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </Group>
          <form.Field name="settings">{(f) => <SettingsEditor value={f.state.value} onChange={(settings) => f.handleChange(settings)} />}</form.Field>
          {harness === 'claude-code' && <form.Field name="claude">{(f) => <ClaudeSettingsEditor value={f.state.value} onChange={(c) => f.handleChange(c)} error={claudeError} />}</form.Field>}
          <form.Field name="description">{(f) => <Textarea label="Description" autosize minRows={2} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          {p && <Text size="xs" c="dimmed">Saving creates revision {p.revision + 1}; it applies to later launches.</Text>}
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="space-between">
            {p ? <Button variant="subtle" color="owlRed" onClick={() => void remove()}>Delete profile</Button> : <span />}
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

function TemplateModal({ t, onClose }: { t?: RoleTemplate; onClose: () => void }) {
  const qc = useQueryClient()
  const prompts = usePrompts()
  const profiles = useProfiles()
  const skills = useSkills()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { slug: t?.slug ?? '', name: t?.name ?? '', kind: (t?.kind ?? 'worker') as RoleTemplate['kind'], description: t?.description ?? '', profile_id: t?.profile_id ?? '',
      prompt_id: t?.prompt_id ?? '', prompt_pin: t?.prompt_pin ?? '', memory_conventions: t?.memory_conventions ?? '', settings: (t?.settings ?? {}) as Record<string, string>,
      skills: t?.skills ?? [] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.PUT('/role-templates/{template_id}', { params: { path: { template_id: t?.id ?? 'new' }, header: { 'If-Match': ifMatch(t?.version ?? 0) } },
          body: value }))
        void qc.invalidateQueries({ queryKey: keys.roleTemplates })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const promptId = useSelector(form.store, (st) => st.values.prompt_id)
  const prompt = usePrompt(promptId || '_none')
  // Templates a space role uses cannot be deleted; the server says which (ADR-0051).
  const remove = async () => {
    if (!t) return
    setError('')
    try {
      const r = await api.DELETE('/role-templates/{template_id}', { params: { path: { template_id: t.id } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: keys.roleTemplates })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={t ? `Edit ${t.name}` : 'New role template'} size="lg">
      <form onSubmit={submitOn(form.handleSubmit)}>
        <Stack gap="sm">
          <Group grow>
            <form.Field name="name">{(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            <form.Field name="slug">{(f) => <TextInput label="Slug" disabled={!!t} classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          </Group>
          <Group grow>
            <form.Field name="kind">
              {(f) => <Select label="Run kind" disabled={!!t} allowDeselect={false} value={f.state.value} onChange={(x) => x && f.handleChange(x as RoleTemplate['kind'])}
                data={['control', 'worker', 'kb', 'dreamer', 'diagnostic']} />}
            </form.Field>
            <form.Field name="profile_id">
              {(f) => <Select label="Harness profile" clearable value={f.state.value || null} onChange={(x) => f.handleChange(x ?? '')}
                data={(profiles.data ?? []).map((p) => ({ value: p.id, label: p.name }))} />}
            </form.Field>
          </Group>
          <Group grow>
            <form.Field name="prompt_id">
              {(f) => <Select label="Prompt" clearable searchable value={f.state.value || null} onChange={(x) => { f.handleChange(x ?? ''); form.setFieldValue('prompt_pin', '') }}
                data={(prompts.data ?? []).map((p) => ({ value: p.id, label: p.name }))} />}
            </form.Field>
            <form.Field name="prompt_pin">
              {(f) => <Select label="Version" allowDeselect={false} disabled={!promptId} value={f.state.value || 'follow'} onChange={(x) => f.handleChange(x === 'follow' || !x ? '' : x)}
                data={[{ value: 'follow', label: 'Follow the latest' }, ...(prompt.data?.versions ?? []).filter((x) => x.state === 'published').map((x) => ({ value: x.id, label: `Pin v${x.number}` }))]} />}
            </form.Field>
          </Group>
          <form.Field name="skills">
            {(f) => <MultiSelect label="Skills" description="Each follows its latest published version" searchable value={f.state.value.map((x) => x.skill_id)}
              onChange={(ids) => f.handleChange(ids.map((id) => f.state.value.find((x) => x.skill_id === id) ?? { skill_id: id }))}
              data={(skills.data ?? []).filter((x) => x.latest > 0).map((x) => ({ value: x.id, label: x.name }))} />}
          </form.Field>
          <form.Field name="memory_conventions">
            {(f) => <Textarea label="Memory conventions" description="Where and how this role keeps memory" autosize minRows={2} value={f.state.value}
              onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="settings">{(f) => <SettingsEditor value={f.state.value} onChange={(settings) => f.handleChange(settings)} />}</form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="space-between">
            {t ? <Button variant="subtle" color="owlRed" onClick={() => void remove()}>Delete template</Button> : <span />}
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
