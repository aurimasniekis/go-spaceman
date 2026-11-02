import { Anchor, Badge, Box, Breadcrumbs, Button, Card, Code, Group, Modal, Select, SimpleGrid, Stack, Text, Textarea, TextInput, Title } from '@mantine/core'
import { IconArrowBackUp, IconGitFork, IconUpload } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { api, ifMatch, unwrap, type Prompt, type PromptVersion } from '@/api/client'
import { keys, useMe, usePrompt, usePrompts } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { kindLabel } from './prompts.index'

export const Route = createFileRoute('/prompts/$promptId')({ component: PromptPage })

function PromptPage() {
  const { promptId } = Route.useParams()
  const prompt = usePrompt(promptId)
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [diff, setDiff] = useState<{ from: string; to: string } | null>(null)
  const [forking, setForking] = useState<PromptVersion | null>(null)
  const [viewing, setViewing] = useState<PromptVersion | null>(null)
  const qc = useQueryClient()
  if (prompt.error) return <ErrorAlert error={prompt.error} />
  if (!prompt.data) return null
  const p = prompt.data
  const draft = p.versions?.find((v) => v.state === 'draft')
  const published = (p.versions ?? []).filter((v) => v.state === 'published')
  const rollback = async (v: PromptVersion) => {
    try {
      unwrap(await api.POST('/prompts/{prompt_id}/rollback', { params: { path: { prompt_id: p.id } }, body: { version_id: v.id } }))
      void qc.invalidateQueries({ queryKey: keys.prompt(p.id) })
      notifyOk(`Version ${v.number}'s content published as a new version`)
    } catch (e) {
      notifyError(e, 'Could not roll back')
    }
  }
  return (
    <Stack gap="md">
      <Breadcrumbs><Anchor component={Link} to="/prompts" size="sm">Prompts</Anchor><Text size="sm">{p.name}</Text></Breadcrumbs>
      <Group gap="xs"><Title order={3}>{p.name}</Title><Badge variant="light">{kindLabel[p.kind]}</Badge>
        {p.latest > 0 && <Badge variant="outline">latest v{p.latest}</Badge>}</Group>
      <Text size="sm" c="dimmed"><span className="mono">{p.slug}</span>{p.description ? ` · ${p.description}` : ''}{p.forked_from ? ` · forked from ${p.forked_from}` : ''}</Text>
      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
        <DraftCard prompt={p} draft={draft} canWrite={canWrite} />
        <Card>
          <Text fw={600} mb="xs">Published versions</Text>
          <Text size="xs" c="dimmed" mb="sm">Immutable. Roles that follow this prompt use the latest from their next launch; pinned roles keep their version.</Text>
          <Stack gap={6}>
            {published.map((v, i) => (
              <Card key={v.id} p="xs" withBorder>
                <Group justify="space-between" wrap="nowrap">
                  <Stack gap={0}>
                    <Text size="sm" fw={600}>Version {v.number}{v.notes ? ` · ${v.notes}` : ''}</Text>
                    <Text size="xs" c="dimmed">{v.published_by} · {datetime(v.published_at)} · <span className="mono">{v.sha256.slice(0, 12)}</span>
                      {v.extends ? <> · extends <span className="mono">{v.extends}</span></> : null}</Text>
                  </Stack>
                  <Group gap={4} wrap="nowrap">
                    <Button size="xs" variant="subtle" onClick={() => setViewing(v)}>View</Button>
                    {published[i + 1] && <Button size="xs" variant="subtle" onClick={() => setDiff({ from: published[i + 1].id, to: v.id })}>Diff</Button>}
                    {canWrite && i > 0 && !draft && <Button size="xs" variant="subtle" leftSection={<IconArrowBackUp size={12} />} onClick={() => void rollback(v)}>Roll back</Button>}
                    {canWrite && <Button size="xs" variant="subtle" leftSection={<IconGitFork size={12} />} onClick={() => setForking(v)}>Fork</Button>}
                  </Group>
                </Group>
              </Card>
            ))}
            {published.length === 0 && <Text size="sm" c="dimmed">Nothing published yet.</Text>}
          </Stack>
        </Card>
      </SimpleGrid>
      {diff && <DiffModal versions={p.versions ?? []} initial={diff} onClose={() => setDiff(null)} />}
      {forking && <ForkModal version={forking} prompt={p} onClose={() => setForking(null)} />}
      <Modal opened={viewing !== null} onClose={() => setViewing(null)} title={`Version ${viewing?.number}`} size="xl">
        {viewing && <MarkdownView source={viewing.body} spaceId="" />}
      </Modal>
    </Stack>
  )
}

function DraftCard({ prompt, draft, canWrite }: { prompt: Prompt; draft?: PromptVersion; canWrite: boolean }) {
  const qc = useQueryClient()
  const prompts = usePrompts()
  const [body, setBody] = useState(draft?.body ?? '')
  const [notes, setNotes] = useState(draft?.notes ?? '')
  const [extendsPrompt, setExtendsPrompt] = useState<string | null>(null)
  const [extendsVersion, setExtendsVersion] = useState<string | null>(draft?.extends || null)
  const parent = useQuery({ queryKey: keys.prompt(extendsPrompt ?? ''), enabled: !!extendsPrompt,
    queryFn: async () => unwrap(await api.GET('/prompts/{prompt_id}', { params: { path: { prompt_id: extendsPrompt! } } })) })
  useEffect(() => { setBody(draft?.body ?? ''); setNotes(draft?.notes ?? ''); setExtendsVersion(draft?.extends || null) }, [draft?.id, draft?.version])
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.prompt(prompt.id) })
  const save = async () => {
    try {
      unwrap(await api.PUT('/prompts/{prompt_id}/draft', { params: { path: { prompt_id: prompt.id }, header: draft ? { 'If-Match': ifMatch(draft.version) } : {} },
        body: { body, notes, extends: extendsVersion ?? undefined } }))
      refresh()
      notifyOk('Draft saved')
    } catch (e) {
      notifyError(e, 'Could not save the draft')
    }
  }
  const publish = async () => {
    try {
      const v = unwrap(await api.POST('/prompts/{prompt_id}/publish', { params: { path: { prompt_id: prompt.id }, header: { 'If-Match': ifMatch(draft!.version) } } }))
      refresh()
      notifyOk(`Published version ${v.number}`)
    } catch (e) {
      notifyError(e, 'Could not publish')
    }
  }
  const discard = async () => {
    try {
      unwrap(await api.DELETE('/prompts/{prompt_id}/draft', { params: { path: { prompt_id: prompt.id } } }).then((r) => ({ ...r, data: {} })))
      refresh()
    } catch (e) {
      notifyError(e, 'Could not discard the draft')
    }
  }
  const dirty = draft ? body !== draft.body || notes !== (draft.notes ?? '') || (extendsVersion ?? '') !== (draft.extends ?? '') : body !== ''
  return (
    <Card>
      <Group justify="space-between" mb="xs">
        <Text fw={600}>{draft ? 'Draft' : 'New draft'}</Text>
        {draft && <Text size="xs" c="dimmed">saved {datetime(draft.created_at)}</Text>}
      </Group>
      <Stack gap="sm">
        <Textarea aria-label="Draft body" description="Markdown" autosize minRows={10} maxRows={28} classNames={{ input: 'mono' }} readOnly={!canWrite}
          value={body} onChange={(e) => setBody(e.target.value)} />
        <Group grow align="flex-end">
          <Select label="Extends (optional)" description="Its content comes first" clearable searchable value={extendsPrompt} onChange={setExtendsPrompt}
            data={(prompts.data ?? []).filter((x) => x.id !== prompt.id && x.latest > 0).map((x) => ({ value: x.id, label: x.name }))} disabled={!canWrite} />
          <Select label="Version" clearable value={extendsVersion} onChange={setExtendsVersion} disabled={!canWrite}
            placeholder={draft?.extends ? draft.extends : 'none'}
            data={(parent.data?.versions ?? []).filter((v) => v.state === 'published').map((v) => ({ value: v.id, label: `v${v.number}` }))} />
        </Group>
        <TextInput label="Notes" value={notes} onChange={(e) => setNotes(e.target.value)} readOnly={!canWrite} />
        {canWrite && (
          <Group justify="flex-end">
            {draft && <Button variant="subtle" color="owlRed" onClick={() => void discard()}>Discard</Button>}
            <Button variant="default" disabled={!dirty} onClick={() => void save()}>Save draft</Button>
            <Button leftSection={<IconUpload size={14} />} disabled={!draft || dirty} onClick={() => void publish()}>Publish</Button>
          </Group>
        )}
      </Stack>
    </Card>
  )
}

function DiffModal({ versions, initial, onClose }: { versions: PromptVersion[]; initial: { from: string; to: string }; onClose: () => void }) {
  const [from, setFrom] = useState(initial.from)
  const [to, setTo] = useState(initial.to)
  const diff = useQuery({ queryKey: ['prompt-diff', from, to], queryFn: async () => unwrap(await api.GET('/prompt-diff', { params: { query: { from, to } } })).lines })
  const opts = versions.map((v) => ({ value: v.id, label: v.state === 'draft' ? 'Draft' : `Version ${v.number}` }))
  const color = { add: 'var(--mantine-color-owlGreen-light)', del: 'var(--mantine-color-owlRed-light)', same: 'transparent' } as const
  return (
    <Modal opened onClose={onClose} title="Compare versions" size="xl">
      <Stack gap="sm">
        <Group grow>
          <Select label="From" allowDeselect={false} value={from} onChange={(v) => v && setFrom(v)} data={opts} />
          <Select label="To" allowDeselect={false} value={to} onChange={(v) => v && setTo(v)} data={opts} />
        </Group>
        <ErrorAlert error={diff.error} />
        <Code block className="mono" p={0} aria-label="Diff" tabIndex={0}>
          {(diff.data ?? []).map((l, i) => (
            <Box key={i} px="xs" style={{ background: color[l.op] }}>
              <Text span className="mono" size="xs" c={l.op === 'same' ? 'dimmed' : undefined}>{l.op === 'add' ? '+ ' : l.op === 'del' ? '- ' : '  '}{l.text || ' '}</Text>
            </Box>
          ))}
        </Code>
      </Stack>
    </Modal>
  )
}

function ForkModal({ version, prompt, onClose }: { version: PromptVersion; prompt: Prompt; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [slug, setSlug] = useState(`${prompt.slug}-fork`)
  const [name, setName] = useState(`${prompt.name} (fork)`)
  const [error, setError] = useState('')
  const fork = async () => {
    try {
      const p = unwrap(await api.POST('/prompt-versions/{version_id}/fork', { params: { path: { version_id: version.id } }, body: { slug, name } }))
      void qc.invalidateQueries({ queryKey: keys.prompts })
      onClose()
      void navigate({ to: '/prompts/$promptId', params: { promptId: p.id } })
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={`Fork version ${version.number}`}>
      <Stack gap="sm">
        <Text size="sm">The new prompt starts from this version and records where it came from. Forks never merge back automatically.</Text>
        <TextInput label="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <TextInput label="Slug" classNames={{ input: 'mono' }} value={slug} onChange={(e) => setSlug(e.target.value)} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void fork()}>Fork</Button></Group>
      </Stack>
    </Modal>
  )
}
