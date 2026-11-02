import { Badge, Button, Card, Group, Modal, MultiSelect, Select, Stack, Table, Tabs, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Profile, type RoleTemplate } from '@/api/client'
import { keys, useMe, useProfiles, usePrompt, usePrompts, useRoleTemplates, useSkills } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
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
                    <Table.Td><Text size="xs" className="mono">{Object.entries(p.settings).map(([k, v]) => `${k}=${v}`).join(' ') || '—'}</Text></Table.Td>
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

function ProfileModal({ p, onClose }: { p?: Profile; onClose: () => void }) {
  const qc = useQueryClient()
  const [v, setV] = useState({ slug: p?.slug ?? '', name: p?.name ?? '', harness: p?.harness ?? 'claude-code', model: p?.model ?? '', description: p?.description ?? '',
    settings: p?.settings ?? {} })
  const [error, setError] = useState('')
  const save = async () => {
    try {
      unwrap(await api.PUT('/harness-profiles/{profile_id}', { params: { path: { profile_id: p?.id ?? 'new' }, header: { 'If-Match': ifMatch(p?.version ?? 0) } },
        body: v }))
      void qc.invalidateQueries({ queryKey: keys.profiles })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={p ? `Edit ${p.name}` : 'New harness profile'} size="lg">
      <Stack gap="sm">
        <Group grow>
          <TextInput label="Name" value={v.name} onChange={(e) => setV({ ...v, name: e.target.value })} />
          <TextInput label="Slug" disabled={!!p} classNames={{ input: 'mono' }} value={v.slug} onChange={(e) => setV({ ...v, slug: e.target.value })} />
        </Group>
        <Group grow>
          <Select label="Harness" disabled={!!p} allowDeselect={false} value={v.harness} onChange={(x) => x && setV({ ...v, harness: x as Profile['harness'] })}
            data={[{ value: 'claude-code', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }]} />
          <TextInput label="Model" description="Harness default when empty" classNames={{ input: 'mono' }} value={v.model} onChange={(e) => setV({ ...v, model: e.target.value })} />
        </Group>
        <SettingsEditor value={v.settings} onChange={(settings) => setV({ ...v, settings })} />
        <Textarea label="Description" autosize minRows={2} value={v.description} onChange={(e) => setV({ ...v, description: e.target.value })} />
        {p && <Text size="xs" c="dimmed">Saving creates revision {p.revision + 1}; it applies to later launches.</Text>}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
      </Stack>
    </Modal>
  )
}

function TemplateModal({ t, onClose }: { t?: RoleTemplate; onClose: () => void }) {
  const qc = useQueryClient()
  const prompts = usePrompts()
  const profiles = useProfiles()
  const [v, setV] = useState({ slug: t?.slug ?? '', name: t?.name ?? '', kind: t?.kind ?? 'worker', description: t?.description ?? '', profile_id: t?.profile_id ?? '',
    prompt_id: t?.prompt_id ?? '', prompt_pin: t?.prompt_pin ?? '', memory_conventions: t?.memory_conventions ?? '', settings: t?.settings ?? {}, skills: t?.skills ?? [] })
  const skills = useSkills()
  const prompt = usePrompt(v.prompt_id || '_none')
  const [error, setError] = useState('')
  const save = async () => {
    try {
      unwrap(await api.PUT('/role-templates/{template_id}', { params: { path: { template_id: t?.id ?? 'new' }, header: { 'If-Match': ifMatch(t?.version ?? 0) } },
        body: v }))
      void qc.invalidateQueries({ queryKey: keys.roleTemplates })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={t ? `Edit ${t.name}` : 'New role template'} size="lg">
      <Stack gap="sm">
        <Group grow>
          <TextInput label="Name" value={v.name} onChange={(e) => setV({ ...v, name: e.target.value })} />
          <TextInput label="Slug" disabled={!!t} classNames={{ input: 'mono' }} value={v.slug} onChange={(e) => setV({ ...v, slug: e.target.value })} />
        </Group>
        <Group grow>
          <Select label="Run kind" disabled={!!t} allowDeselect={false} value={v.kind} onChange={(x) => x && setV({ ...v, kind: x as RoleTemplate['kind'] })}
            data={['control', 'worker', 'kb', 'dreamer', 'diagnostic']} />
          <Select label="Harness profile" clearable value={v.profile_id || null} onChange={(x) => setV({ ...v, profile_id: x ?? '' })}
            data={(profiles.data ?? []).map((p) => ({ value: p.id, label: p.name }))} />
        </Group>
        <Group grow>
          <Select label="Prompt" clearable searchable value={v.prompt_id || null} onChange={(x) => setV({ ...v, prompt_id: x ?? '', prompt_pin: '' })}
            data={(prompts.data ?? []).map((p) => ({ value: p.id, label: p.name }))} />
          <Select label="Version" allowDeselect={false} disabled={!v.prompt_id} value={v.prompt_pin || 'follow'} onChange={(x) => setV({ ...v, prompt_pin: x === 'follow' || !x ? '' : x })}
            data={[{ value: 'follow', label: 'Follow the latest' }, ...(prompt.data?.versions ?? []).filter((x) => x.state === 'published').map((x) => ({ value: x.id, label: `Pin v${x.number}` }))]} />
        </Group>
        <MultiSelect label="Skills" description="Each follows its latest published version" searchable value={v.skills.map((x) => x.skill_id)}
          onChange={(ids) => setV({ ...v, skills: ids.map((id) => v.skills.find((x) => x.skill_id === id) ?? { skill_id: id }) })}
          data={(skills.data ?? []).filter((x) => x.latest > 0).map((x) => ({ value: x.id, label: x.name }))} />
        <Textarea label="Memory conventions" description="Where and how this role keeps memory" autosize minRows={2} value={v.memory_conventions}
          onChange={(e) => setV({ ...v, memory_conventions: e.target.value })} />
        <SettingsEditor value={v.settings} onChange={(settings) => setV({ ...v, settings })} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
      </Stack>
    </Modal>
  )
}
