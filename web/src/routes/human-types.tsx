import { Badge, Button, Card, Drawer, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Table, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus, IconUpload } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { api, ifMatch, unwrap, type CatalogHumanType } from '@/api/client'
import { keys, useHumanTypes, useMe } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SchemaForm } from '@/features/human/SchemaForm'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/human-types')({ component: HumanTypesPage })

/** The human-action type catalog (SM-HTYPE-001/002): schemas, safe form metadata and outcomes, versioned. */
function HumanTypesPage() {
  const types = useHumanTypes()
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const current = types.data?.find((t) => t.id === open) ?? null
  return (
    <>
      <PageHeader title="Human action types" description="Custom request types for people: what is asked (request schema), how it is answered (response schema) and how it can end (outcomes). Spaces enable them."
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New type</Button>} />
      <ErrorAlert error={types.error} />
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead><Table.Tr><Table.Th>Type</Table.Th><Table.Th>Latest</Table.Th><Table.Th>Outcomes</Table.Th><Table.Th>Draft</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(types.data ?? []).map((t) => {
              const latest = t.versions?.find((v) => v.state === 'published' && v.number === t.latest)
              return (
                <Table.Tr key={t.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(t.id)}>
                  <Table.Td><Text size="sm" fw={500}>{t.name}</Text><Text size="xs" c="dimmed" className="mono">{t.slug}</Text></Table.Td>
                  <Table.Td>{t.latest > 0 ? `v${t.latest}` : <Text size="sm" c="dimmed">unpublished</Text>}</Table.Td>
                  <Table.Td><Group gap={4}>{(latest?.outcomes ?? []).map((o) => <Badge key={o} variant="light">{o}</Badge>)}</Group></Table.Td>
                  <Table.Td>{t.versions?.some((v) => v.state === 'draft') ? <Badge variant="outline" color="owlYellow">draft</Badge> : null}</Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
        {types.isSuccess && types.data.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No custom types yet. The built-in question, approval, to-do and message types are always available.</Text>}
      </Card>
      {creating && <NewTypeModal onClose={() => setCreating(false)} />}
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="xl" title={current?.name ?? ''}>
        {current && <TypeEditor t={current} canWrite={canWrite} />}
      </Drawer>
    </>
  )
}

function NewTypeModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [v, setV] = useState({ slug: '', name: '', description: '' })
  const [error, setError] = useState('')
  const create = async () => {
    try {
      unwrap(await api.POST('/human-types', { body: { ...v, request_schema: { type: 'object', properties: {} }, response_schema: { type: 'object', properties: {} }, outcomes: ['done'] } }))
      void qc.invalidateQueries({ queryKey: keys.humanTypes })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title="New human action type">
      <Stack gap="sm">
        <TextInput label="Name" value={v.name} onChange={(e) => setV({ ...v, name: e.target.value })} />
        <TextInput label="Slug" classNames={{ input: 'mono' }} value={v.slug} onChange={(e) => setV({ ...v, slug: e.target.value })} />
        <Textarea label="Description" autosize minRows={2} value={v.description} onChange={(e) => setV({ ...v, description: e.target.value })} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void create()}>Create</Button></Group>
      </Stack>
    </Modal>
  )
}

const pretty = (v: unknown) => JSON.stringify(v ?? {}, null, 2)

function TypeEditor({ t, canWrite }: { t: CatalogHumanType; canWrite: boolean }) {
  const qc = useQueryClient()
  const draft = t.versions?.find((v) => v.state === 'draft')
  const base = draft ?? t.versions?.find((v) => v.state === 'published')
  const [req, setReq] = useState(pretty(base?.request_schema))
  const [resp, setResp] = useState(pretty(base?.response_schema))
  const [ui, setUi] = useState(pretty(base?.ui))
  const [outcomes, setOutcomes] = useState<string[]>(base?.outcomes ?? ['done'])
  const [priority, setPriority] = useState<string>(base?.default_priority ?? 'normal')
  const [expiry, setExpiry] = useState<number>(base?.default_expiry_minutes ?? 0)
  const [notes, setNotes] = useState(draft?.notes ?? '')
  const [sample, setSample] = useState<Record<string, unknown>>({})
  const parsed = useMemo(() => {
    try {
      return { req: JSON.parse(req) as unknown, resp: JSON.parse(resp) as unknown, ui: JSON.parse(ui) as unknown, error: '' }
    } catch (e) {
      return { req: {}, resp: {}, ui: {}, error: (e as Error).message }
    }
  }, [req, resp, ui])
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.humanTypes })
  const save = async () => {
    try {
      unwrap(await api.PUT('/human-types/{type_id}/draft', { params: { path: { type_id: t.id }, header: draft ? { 'If-Match': ifMatch(draft.version) } : {} },
        body: { request_schema: parsed.req, response_schema: parsed.resp, ui: parsed.ui, outcomes, default_priority: priority as 'normal', default_expiry_minutes: expiry, notes } }))
      refresh()
      notifyOk('Draft saved')
    } catch (e) {
      notifyError(e, 'Could not save the draft')
    }
  }
  const publish = async () => {
    try {
      unwrap(await api.POST('/human-types/{type_id}/publish', { params: { path: { type_id: t.id }, header: { 'If-Match': ifMatch(draft!.version) } } }))
      refresh()
      notifyOk('Published')
    } catch (e) {
      notifyError(e, 'Could not publish')
    }
  }
  return (
    <Stack gap="sm">
      <Text size="sm" c="dimmed"><span className="mono">{t.slug}</span>{t.description ? ` · ${t.description}` : ''}</Text>
      <Group gap={4}>
        {(t.versions ?? []).filter((v) => v.state === 'published').map((v) => <Badge key={v.id} variant="outline">v{v.number} · {datetime(v.published_at)}</Badge>)}
      </Group>
      <Text size="xs" c="dimmed">{draft ? 'Editing the draft.' : 'Saving starts a new draft from the latest version.'} Published versions never change; actions keep the version they were created with.</Text>
      <SimpleGrid cols={{ base: 1, md: 2 }}>
        <Textarea label="Request schema" description="JSON Schema 2020-12; root type object" classNames={{ input: 'mono' }} autosize minRows={8} maxRows={18} value={req} onChange={(e) => setReq(e.target.value)} readOnly={!canWrite} />
        <Textarea label="Response schema" description="What the person fills in" classNames={{ input: 'mono' }} autosize minRows={8} maxRows={18} value={resp} onChange={(e) => setResp(e.target.value)} readOnly={!canWrite} />
      </SimpleGrid>
      <Textarea label="Form metadata" description='{"order": [...], "fields": {"name": {"label", "help", "widget", "placeholder"}}}; nothing else is accepted' classNames={{ input: 'mono' }}
        autosize minRows={3} maxRows={10} value={ui} onChange={(e) => setUi(e.target.value)} readOnly={!canWrite} />
      <Group grow align="flex-start">
        <TagsInput label="Outcomes" description="How a person can finish it" value={outcomes} onChange={setOutcomes} disabled={!canWrite} />
        <Select label="Default priority" allowDeselect={false} value={priority} onChange={(v) => v && setPriority(v)} data={['low', 'normal', 'high', 'urgent']} disabled={!canWrite} />
        <NumberInput label="Expires after (minutes)" description="0: never" min={0} value={expiry} onChange={(v) => setExpiry(typeof v === 'number' ? v : 0)} disabled={!canWrite} />
      </Group>
      <TextInput label="Notes" value={notes} onChange={(e) => setNotes(e.target.value)} readOnly={!canWrite} />
      {parsed.error ? <Text c="owlRed" size="sm" role="alert">Not valid JSON: {parsed.error}</Text> : (
        <Card withBorder>
          <Text size="xs" c="dimmed" mb="xs">Preview of the request form</Text>
          <SchemaForm label="Request" schema={parsed.req} ui={parsed.ui} value={sample} onChange={setSample} />
        </Card>
      )}
      {canWrite && (
        <Group justify="flex-end">
          <Button variant="default" disabled={!!parsed.error} onClick={() => void save()}>Save draft</Button>
          <Button leftSection={<IconUpload size={14} />} disabled={!draft} onClick={() => void publish()}>Publish</Button>
        </Group>
      )}
    </Stack>
  )
}
