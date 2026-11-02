import { Badge, Button, Card, Checkbox, Drawer, Group, Modal, NumberInput, Select, SimpleGrid, Stack, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { IconMoonStars, IconPlus } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type DreamConfig } from '@/api/client'
import { useDreams, useMe, useSpaceRoles } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

const roleList = (l: string[]) => (l.includes('*') ? 'every role' : l.join(', '))

const stateColor: Record<string, string> = { running: 'owlBlue', reported: 'owlBlue', succeeded: 'owlGreen', failed: 'owlRed', timed_out: 'owlYellow' }

/**
 * The dreamer (SM-DREAM-*): configurations that consolidate role memory from new evidence, run manually or on a
 * schedule within their budgets, and their recent dreams with summaries and findings.
 */
export function DreamsCard({ spaceId }: { spaceId: string }) {
  const qc = useQueryClient()
  const dreams = useDreams(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const [open, setOpen] = useState<DreamConfig | null>(null)
  const [editing, setEditing] = useState<DreamConfig | 'new' | null>(null)
  const start = async (d: DreamConfig) => {
    try {
      unwrap(await api.POST('/spaces/{space_id}/dreams/{dream_id}/start', { params: { path: { space_id: spaceId, dream_id: d.id } } }))
      void qc.invalidateQueries({ queryKey: ['dreams', spaceId] })
      notifyOk('Dreaming')
    } catch (e) {
      notifyError(e, 'Could not start the dream')
    }
  }
  return (
    <Card mb="md">
      <Group justify="space-between" mb="xs">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Dreams</Text>
        {canWrite && <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add dream</Button>}
      </Group>
      {(dreams.data ?? []).length === 0 && <Text size="sm" c="dimmed">No dreams. A dream consolidates role memory from new transcripts, on demand or on a schedule.</Text>}
      <Stack gap="xs">
        {(dreams.data ?? []).map((d) => (
          <Group key={d.id} justify="space-between" wrap="nowrap">
            <div style={{ minWidth: 0 }}>
              <Group gap={6}>
                <Button variant="subtle" size="compact-sm" px={0} onClick={() => setOpen(d)}>{d.name}</Button>
                {!d.enabled && <Badge size="xs" color="gray">disabled</Badge>}
                {d.last_run && <Badge size="xs" variant="light" color={stateColor[d.last_run.state]}>{d.last_run.state.replace('_', ' ')}{d.last_run.no_change ? ' · no change' : ''}</Badge>}
              </Group>
              <Text size="xs" c="dimmed">
                {roleList(d.memory_roles)} · {d.interval_hours > 0 ? `every ${d.interval_hours} h` : 'manual'} · {d.max_minutes} min, {d.max_files} files
                {d.last_run && ` · last ${ago(d.last_run.started_at)}`}
              </Text>
            </div>
            {canWrite && <Button size="xs" variant="light" leftSection={<IconMoonStars size={14} />} onClick={() => void start(d)}>Dream now</Button>}
          </Group>
        ))}
      </Stack>
      <Drawer opened={open !== null} onClose={() => setOpen(null)} position="right" size="lg" title={open ? `Dreams: ${open.name}` : ''}>
        {open && <DreamRuns spaceId={spaceId} d={open} canWrite={canWrite} onEdit={() => setEditing(open)} />}
      </Drawer>
      {editing && <DreamModal spaceId={spaceId} d={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </Card>
  )
}

function DreamRuns({ spaceId, d, canWrite, onEdit }: { spaceId: string; d: DreamConfig; canWrite: boolean; onEdit: () => void }) {
  const runs = useQuery({ queryKey: ['dreams', spaceId, d.id, d.last_run?.state], queryFn: async () =>
    unwrap(await api.GET('/spaces/{space_id}/dreams/{dream_id}/runs', { params: { path: { space_id: spaceId, dream_id: d.id } } })).items })
  return (
    <Stack gap="sm">
      <Text size="sm" c="dimmed">
        Consolidates {roleList(d.memory_roles)} memory from {roleList(d.transcript_roles)} transcripts{d.include_own ? '' : ' (not its own)'}. Evidence advances only after a
        dream's report is checkpointed; a dream that changes nothing is a success.
      </Text>
      {canWrite && <Group><Button size="xs" variant="default" onClick={onEdit}>Edit…</Button></Group>}
      {(runs.data ?? []).map((r) => (
        <Card key={r.id} withBorder p="sm">
          <Group justify="space-between">
            <Text size="xs" c="dimmed">{datetime(r.started_at)} · {r.trigger}</Text>
            <Badge variant="light" color={stateColor[r.state]}>{r.state.replace('_', ' ')}{r.no_change ? ' · no change' : ''}</Badge>
          </Group>
          {r.summary && <MarkdownView source={r.summary} spaceId={spaceId} />}
          {r.findings.map((f, i) => (
            <Text key={i} size="sm" mt={4}><Badge size="xs" variant="outline" mr={6}>{f.kind.replace('_', ' ')}</Badge>{f.summary}</Text>
          ))}
          {r.detail && <Text size="xs" c="dimmed" mt={4}>{r.detail}</Text>}
        </Card>
      ))}
      {runs.isSuccess && runs.data.length === 0 && <Text size="sm" c="dimmed">No dreams yet.</Text>}
    </Stack>
  )
}

function DreamModal({ spaceId, d, onClose }: { spaceId: string; d: DreamConfig | null; onClose: () => void }) {
  const qc = useQueryClient()
  const roles = useSpaceRoles(spaceId).data ?? []
  const [v, setV] = useState({ slug: d?.slug ?? '', name: d?.name ?? '', space_role: d?.space_role ?? '', memory_roles: d?.memory_roles ?? ['*'],
    transcript_roles: d?.transcript_roles ?? ['*'], include_own: d?.include_own ?? false, instructions: d?.instructions ?? '', interval_hours: d?.interval_hours ?? 0,
    max_minutes: d?.max_minutes ?? 30, max_tokens: d?.max_tokens ?? 0, max_files: d?.max_files ?? 20, max_bytes: d?.max_bytes ?? 2097152, enabled: d?.enabled ?? true })
  const [error, setError] = useState('')
  const save = async () => {
    const body = { ...v, slug: d ? undefined : v.slug }
    try {
      if (d) unwrap(await api.PUT('/spaces/{space_id}/dreams/{dream_id}', { params: { path: { space_id: spaceId, dream_id: d.id }, header: { 'If-Match': `"${d.version}"` } }, body }))
      else unwrap(await api.POST('/spaces/{space_id}/dreams', { params: { path: { space_id: spaceId } }, body }))
      void qc.invalidateQueries({ queryKey: ['dreams', spaceId] })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title={d ? `Edit ${d.name}` : 'Add a dream'} size="lg">
      <Stack gap="sm">
        <SimpleGrid cols={2}>
          <TextInput label="Name" value={v.name} onChange={(e) => setV({ ...v, name: e.target.value })} />
          <TextInput label="Slug" classNames={{ input: 'mono' }} disabled={d !== null} value={v.slug} onChange={(e) => setV({ ...v, slug: e.target.value })} />
        </SimpleGrid>
        <Select label="Dreams as" description="A space role whose template is of kind dreamer" data={roles.map((r) => r.name)} value={v.space_role || null}
          onChange={(x) => setV({ ...v, space_role: x ?? '' })} />
        <TagsInput label="Memories to consolidate" description="Role names; * for every role" value={v.memory_roles} onChange={(memory_roles) => setV({ ...v, memory_roles })} />
        <TagsInput label="Transcripts it may read" description="Role names; * for every role" value={v.transcript_roles} onChange={(transcript_roles) => setV({ ...v, transcript_roles })} />
        <Checkbox label="Also read the dreamer's own transcripts" checked={v.include_own} onChange={(e) => setV({ ...v, include_own: e.currentTarget.checked })} />
        <Textarea label="Instructions" autosize minRows={2} value={v.instructions} onChange={(e) => setV({ ...v, instructions: e.target.value })} />
        <SimpleGrid cols={3}>
          <NumberInput label="Every (hours)" description="0: manual" min={0} max={720} value={v.interval_hours} onChange={(n) => setV({ ...v, interval_hours: Number(n) || 0 })} />
          <NumberInput label="Time budget (min)" min={5} max={240} value={v.max_minutes} onChange={(n) => setV({ ...v, max_minutes: Number(n) || 30 })} />
          <NumberInput label="Token budget" description="Told to the dreamer" min={0} value={v.max_tokens} onChange={(n) => setV({ ...v, max_tokens: Number(n) || 0 })} />
          <NumberInput label="Evidence files" min={1} max={200} value={v.max_files} onChange={(n) => setV({ ...v, max_files: Number(n) || 20 })} />
          <NumberInput label="Evidence bytes" min={65536} max={67108864} step={65536} value={v.max_bytes} onChange={(n) => setV({ ...v, max_bytes: Number(n) || 2097152 })} />
        </SimpleGrid>
        <Checkbox label="Enabled" checked={v.enabled} onChange={(e) => setV({ ...v, enabled: e.currentTarget.checked })} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
      </Stack>
    </Modal>
  )
}
