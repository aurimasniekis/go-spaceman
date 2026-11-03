import { ActionIcon, Badge, Button, Card, Drawer, Group, Modal, Stack, Table, Tabs, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus, IconTrash, IconUpload } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Skill } from '@/api/client'
import { keys, useMe, useSkills } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/skills')({ component: SkillsPage })

/** Central, versioned skills (SM-MCP-004): guidance files written into a harness's skills folder, never permissions. */
function SkillsPage() {
  const skills = useSkills()
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const current = skills.data?.find((s) => s.id === open) ?? null
  return (
    <>
      <PageHeader title="Skills" description="Guidance files (SKILL.md and references) given to agents through roles. Skills inform; they never grant access, and nothing in them runs when they are installed."
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New skill</Button>} />
      <ErrorAlert error={skills.error} />
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead><Table.Tr><Table.Th>Skill</Table.Th><Table.Th>Latest</Table.Th><Table.Th>Files</Table.Th><Table.Th>Source</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(skills.data ?? []).map((s) => {
              const latest = s.versions?.find((v) => v.state === 'published' && v.number === s.latest)
              return (
                <Table.Tr key={s.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(s.id)}>
                  <Table.Td><Text size="sm" fw={500}>{s.name}</Text><Text size="xs" c="dimmed" className="mono">{s.slug}</Text></Table.Td>
                  <Table.Td>{s.latest > 0 ? `v${s.latest}` : <Text size="sm" c="dimmed">unpublished</Text>}</Table.Td>
                  <Table.Td><Text size="xs" className="mono">{Object.keys(latest?.files ?? {}).join(', ')}</Text></Table.Td>
                  <Table.Td>{s.source === 'builtin' ? <Badge variant="light">built-in</Badge> : <Badge variant="outline">custom</Badge>}
                    {s.source_ref && <Text size="xs" c="dimmed">{s.source_ref}</Text>}</Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
      </Card>
      {creating && <NewSkillModal onClose={() => setCreating(false)} />}
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="xl" title={current?.name ?? ''}>
        {current && <SkillEditor skill={current} canWrite={canWrite} />}
      </Drawer>
    </>
  )
}

const template = (slug: string) => `---\nname: ${slug}\ndescription: Use when …\n---\n\n# ${slug}\n\n`

function NewSkillModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { slug: '', name: '', description: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.POST('/skills', { body: { ...value, files: { 'SKILL.md': template(value.slug) } } }))
        void qc.invalidateQueries({ queryKey: keys.skills })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title="New skill">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="name">{(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          <form.Field name="slug">
            {(f) => <TextInput label="Slug" description="Also the skill's name in SKILL.md" classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="description">{(f) => <Textarea label="Description" autosize minRows={2} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Create</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

function SkillEditor({ skill, canWrite }: { skill: Skill; canWrite: boolean }) {
  const qc = useQueryClient()
  const draft = skill.versions?.find((v) => v.state === 'draft')
  const base = draft ?? skill.versions?.find((v) => v.state === 'published')
  const [active, setActive] = useState<string>('SKILL.md')
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.skills })
  const form = useForm({
    defaultValues: { files: (base?.files ?? {}) as Record<string, string>, newPath: '' },
    onSubmit: async ({ value }) => {
      try {
        unwrap(await api.PUT('/skills/{skill_id}/draft', { params: { path: { skill_id: skill.id }, header: draft ? { 'If-Match': ifMatch(draft.version) } : {} }, body: { files: value.files } }))
        refresh()
        notifyOk('Draft saved')
      } catch (e) {
        notifyError(e, 'Could not save the draft')
      }
    },
  })
  const addFile = () => {
    const { files, newPath } = form.state.values
    if (!newPath || newPath in files) return
    form.setFieldValue('files', { ...files, [newPath]: '' })
    setActive(newPath)
    form.setFieldValue('newPath', '')
  }
  const publish = async () => {
    try {
      unwrap(await api.POST('/skills/{skill_id}/publish', { params: { path: { skill_id: skill.id }, header: { 'If-Match': ifMatch(draft!.version) } } }))
      refresh()
      notifyOk('Published')
    } catch (e) {
      notifyError(e, 'Could not publish')
    }
  }
  return (
    <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
      <Stack gap="sm">
        <Text size="sm" c="dimmed"><span className="mono">{skill.slug}</span>{skill.description ? ` · ${skill.description}` : ''}</Text>
        <Group gap={4}>{(skill.versions ?? []).filter((v) => v.state === 'published').map((v) => <Badge key={v.id} variant="outline">v{v.number} · {datetime(v.published_at)} · {v.sha256.slice(0, 8)}</Badge>)}</Group>
        <form.Field name="files">
          {(f) => {
            const files = f.state.value
            const paths = Object.keys(files).sort((a, b) => (a === 'SKILL.md' ? -1 : b === 'SKILL.md' ? 1 : a.localeCompare(b)))
            return (
              <Tabs value={active} onChange={(v) => v && setActive(v)}>
                <Tabs.List>
                  {paths.map((p) => <Tabs.Tab key={p} value={p} className="mono">{p}</Tabs.Tab>)}
                </Tabs.List>
                {paths.map((p) => (
                  <Tabs.Panel key={p} value={p} pt="xs">
                    <Group align="flex-start" wrap="nowrap">
                      <Textarea aria-label={`Contents of ${p}`} classNames={{ input: 'mono' }} autosize minRows={12} maxRows={30} style={{ flex: 1 }} readOnly={!canWrite}
                        value={files[p]} onChange={(e) => f.handleChange({ ...files, [p]: e.target.value })} />
                      {canWrite && p !== 'SKILL.md' && (
                        <ActionIcon aria-label={`Remove ${p}`} onClick={() => { const n = { ...files }; delete n[p]; f.handleChange(n); setActive('SKILL.md') }}><IconTrash size={14} /></ActionIcon>
                      )}
                    </Group>
                  </Tabs.Panel>
                ))}
              </Tabs>
            )
          }}
        </form.Field>
        {canWrite && (
          <>
            <Group align="flex-end">
              <form.Field name="newPath">
                {(f) => <TextInput label="Add a file" placeholder="references/commands.md" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)}
                  onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addFile() } }} />}
              </form.Field>
              <form.Subscribe selector={(st) => st.values}>
                {(v) => (
                  <Button variant="default" disabled={!v.newPath || v.newPath in v.files} onClick={addFile}>Add</Button>
                )}
              </form.Subscribe>
            </Group>
            <Group justify="flex-end">
              <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" variant="default" loading={busy}>Save draft</Button>}</form.Subscribe>
              <Button leftSection={<IconUpload size={14} />} disabled={!draft} onClick={() => void publish()}>Publish</Button>
            </Group>
          </>
        )}
      </Stack>
    </form>
  )
}
