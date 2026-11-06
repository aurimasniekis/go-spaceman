import { Badge, Button, Card, Checkbox, Drawer, Group, Modal, NumberInput, SegmentedControl, Select, SimpleGrid, Stack, Switch, Table, Text, Textarea, TextInput, Title, Tooltip } from '@mantine/core'
import { useDebouncedValue } from '@mantine/hooks'
import { IconPlus, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useEffect, useMemo, useState } from 'react'
import { api, ApiError, ifMatch, unwrap, type Schedule, type ScheduleInput } from '@/api/client'
import { keys, useMe, useSchedule, useSchedules } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { MISSED_LABEL, scheduleWhen } from '@/features/limits/format'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { fieldError, required } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/schedules')({ component: SchedulesPage })

const OUTCOME_COLOR: Record<string, string> = { created: 'owlGreen', duplicate: 'gray', skipped: 'owlBlue', missed: 'owlYellow', failed: 'owlRed' }

function zones(): string[] {
  try {
    const z = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? []
    return z.includes('UTC') ? z : ['UTC', ...z]
  } catch {
    return ['UTC']
  }
}

function SchedulesPage() {
  const { spaceId } = Route.useParams()
  const list = useSchedules(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [editing, setEditing] = useState<Schedule | null>(null)

  return (
    <>
      <SectionHeader title="Schedules" description="Recurring tasks and inbox items for this space"
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New schedule</Button>} />
      <ErrorAlert error={list.error} />
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Schedule</Table.Th>
              <Table.Th>When</Table.Th>
              <Table.Th>Next</Table.Th>
              <Table.Th>Last</Table.Th>
              <Table.Th>Missed runs</Table.Th>
              <Table.Th>Creates</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(list.data ?? []).map((s) => (
              <Table.Tr key={s.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(s.id)} onKeyDown={(e) => e.key === 'Enter' && setOpen(s.id)} tabIndex={0}>
                <Table.Td>
                  <Group gap={6}><Text size="sm" fw={500}>{s.name}</Text>{!s.enabled && <Badge size="xs" color="gray">disabled</Badge>}</Group>
                  {s.description && <Text size="xs" c="dimmed">{s.description}</Text>}
                </Table.Td>
                <Table.Td><Text size="sm" className="mono">{scheduleWhen(s)}</Text></Table.Td>
                <Table.Td>
                  {s.next_at ? <Tooltip label={datetime(s.next_at)}><Text size="sm">{ago(s.next_at)}</Text></Tooltip> : <Text size="sm" c="dimmed">—</Text>}
                </Table.Td>
                <Table.Td>
                  {s.last_outcome ? <Badge variant="light" color={OUTCOME_COLOR[s.last_outcome]}>{s.last_outcome}</Badge> : <Text size="sm" c="dimmed">not yet</Text>}
                  {s.last_at && <Text size="xs" c="dimmed">{ago(s.last_at)}</Text>}
                </Table.Td>
                <Table.Td><Text size="sm">{MISSED_LABEL[s.missed]}{s.missed === 'catch_up' ? ` (≤ ${s.catch_up_max})` : ''}</Text></Table.Td>
                <Table.Td><Text size="sm">{s.target === 'task' ? 'Task' : 'Inbox item'}: {s.title}</Text></Table.Td>
              </Table.Tr>
            ))}
            {list.isSuccess && list.data.length === 0 && (
              <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed" ta="center" p="md">No schedules yet.</Text></Table.Td></Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
      {creating && <ScheduleModal spaceId={spaceId} onClose={() => setCreating(false)} />}
      {editing && <ScheduleModal spaceId={spaceId} schedule={editing} onClose={() => setEditing(null)} />}
      <ScheduleDrawer spaceId={spaceId} id={open} canWrite={canWrite} onClose={() => setOpen(null)} onEdit={(s) => { setOpen(null); setEditing(s) }} />
    </>
  )
}

function ScheduleDrawer({ spaceId, id, canWrite, onClose, onEdit }: { spaceId: string; id: string | null; canWrite: boolean; onClose: () => void; onEdit: (s: Schedule) => void }) {
  const qc = useQueryClient()
  const d = useSchedule(spaceId, id)
  const s = d.data
  const remove = async () => {
    if (!s || !window.confirm(`Delete the schedule "${s.name}"? What it created stays.`)) return
    try {
      unwrap(await api.DELETE('/spaces/{space_id}/schedules/{schedule_id}', { params: { path: { space_id: spaceId, schedule_id: s.id } } }))
      void qc.invalidateQueries({ queryKey: keys.schedules(spaceId) })
      notifyOk('Schedule deleted')
      onClose()
    } catch (e) {
      notifyError(e, 'Could not delete the schedule')
    }
  }
  return (
    <Drawer opened={!!id} onClose={onClose} position="right" size="lg" title={s?.name ?? 'Schedule'}>
      <ErrorAlert error={d.error} />
      {s && (
        <Stack gap="sm">
          <Text size="sm" className="mono">{scheduleWhen(s)}</Text>
          <Text size="sm">Creates {s.target === 'task' ? 'a ready task' : 'an inbox item'} titled <b>{s.title}</b>{s.priority !== 'normal' ? ` (${s.priority})` : ''}
            {s.assignee_role ? ` for ${s.assignee_role}` : ''}.</Text>
          <Text size="sm">Missed runs: {MISSED_LABEL[s.missed]}{s.missed === 'catch_up' ? `, at most ${s.catch_up_max}` : ''}. {s.skip_if_open ? 'Skipped while the previous one is still open.' : 'Created even when the previous one is open.'}</Text>
          <div>
            <Title order={6}>Upcoming</Title>
            {(s.upcoming ?? []).length === 0 ? <Text size="sm" c="dimmed">None (disabled)</Text>
              : (s.upcoming ?? []).map((t) => <Text key={t} size="sm">{datetime(t)} <Text span size="xs" c="dimmed">({ago(t)})</Text></Text>)}
          </div>
          <div>
            <Title order={6}>Recent occurrences</Title>
            <Table>
              <Table.Thead><Table.Tr><Table.Th>Time</Table.Th><Table.Th>Outcome</Table.Th><Table.Th>Created</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(s.occurrences ?? []).map((o) => (
                  <Table.Tr key={o.at}>
                    <Table.Td><Text size="xs">{datetime(o.at)}</Text></Table.Td>
                    <Table.Td>
                      <Group gap={4}><Badge size="sm" variant="light" color={OUTCOME_COLOR[o.outcome]}>{o.outcome}</Badge>{o.late && <Badge size="sm" color="owlYellow" variant="outline">late</Badge>}</Group>
                      {o.detail && <Text size="xs" c="dimmed">{o.detail}</Text>}
                    </Table.Td>
                    <Table.Td>
                      {o.target_id && o.target_kind === 'inbox_item'
                        ? <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId: o.target_id }} size="xs" className="mono">{o.target_id}</AnchorLink>
                        : o.target_id ? <AnchorLink to="/spaces/$spaceId/queue" params={{ spaceId }} size="xs" className="mono">{o.target_id}</AnchorLink> : <Text size="xs" c="dimmed">—</Text>}
                    </Table.Td>
                  </Table.Tr>
                ))}
                {(s.occurrences ?? []).length === 0 && <Table.Tr><Table.Td colSpan={3}><Text size="xs" c="dimmed">None yet.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </div>
          {canWrite && (
            <Group justify="flex-end">
              <Button variant="default" color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => void remove()}>Delete</Button>
              <Button onClick={() => onEdit(s)}>Edit</Button>
            </Group>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

const UNITS = [{ value: '60', label: 'minutes' }, { value: '3600', label: 'hours' }, { value: '86400', label: 'days' }]

function ScheduleModal({ spaceId, schedule, onClose }: { spaceId: string; schedule?: Schedule; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const tzs = useMemo(zones, [])
  const unit = schedule?.interval_seconds ? (schedule.interval_seconds % 86400 === 0 ? 86400 : schedule.interval_seconds % 3600 === 0 ? 3600 : 60) : 3600
  const form = useForm({
    defaultValues: {
      name: schedule?.name ?? '', description: schedule?.description ?? '', enabled: schedule?.enabled ?? true, kind: (schedule?.kind ?? 'cron') as ScheduleInput['kind'],
      cron: schedule?.cron ?? '0 9 * * 1-5', every: schedule?.interval_seconds ? schedule.interval_seconds / unit : 1, unit: String(unit), timezone: schedule?.timezone ?? 'UTC',
      missed: (schedule?.missed ?? 'skip') as NonNullable<ScheduleInput['missed']>, catch_up_max: schedule?.catch_up_max ?? 3, skip_if_open: schedule?.skip_if_open ?? true,
      target: (schedule?.target ?? 'inbox') as ScheduleInput['target'], title: schedule?.title ?? '', body: schedule?.body ?? '',
      priority: (schedule?.priority ?? 'normal') as NonNullable<ScheduleInput['priority']>, assignee_role: schedule?.assignee_role ?? '',
    },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const body = toInput(value)
        if (schedule) {
          unwrap(await api.PUT('/spaces/{space_id}/schedules/{schedule_id}', { params: { path: { space_id: spaceId, schedule_id: schedule.id }, header: { 'If-Match': ifMatch(schedule.version) } }, body }))
        } else {
          unwrap(await api.POST('/spaces/{space_id}/schedules', { params: { path: { space_id: spaceId } }, body }))
        }
        void qc.invalidateQueries({ queryKey: keys.schedules(spaceId) })
        notifyOk(schedule ? 'Schedule saved' : 'Schedule created')
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={schedule ? `Edit ${schedule.name}` : 'New schedule'} size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <form.Field name="name" validators={{ onBlur: required('Name') }}>
              {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="description">
              {(f) => <TextInput label="Description" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </SimpleGrid>
          <Title order={6}>When</Title>
          <form.Field name="kind">
            {(f) => <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as ScheduleInput['kind'])} data={[{ value: 'cron', label: 'Cron' }, { value: 'interval', label: 'Interval' }]} />}
          </form.Field>
          <form.Subscribe selector={(st) => st.values.kind}>
            {(kind) => kind === 'cron' ? (
              <SimpleGrid cols={{ base: 1, sm: 2 }}>
                <form.Field name="cron">
                  {(f) => <TextInput label="Cron expression" description="minute hour day month weekday, or @daily, @weekly…" className="mono" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
                </form.Field>
                <form.Field name="timezone">
                  {(f) => <Select label="Time zone" description="IANA; daylight-saving changes are followed" searchable allowDeselect={false} data={tzs} value={f.state.value} onChange={(v) => v && f.handleChange(v)} />}
                </form.Field>
              </SimpleGrid>
            ) : (
              <SimpleGrid cols={{ base: 1, sm: 2 }}>
                <form.Field name="every">
                  {(f) => <NumberInput label="Every" min={1} allowDecimal={false} value={f.state.value} onChange={(v) => f.handleChange(typeof v === 'number' ? v : 1)} />}
                </form.Field>
                <form.Field name="unit">
                  {(f) => <Select label="Unit" allowDeselect={false} data={UNITS} value={f.state.value} onChange={(v) => v && f.handleChange(v)} />}
                </form.Field>
              </SimpleGrid>
            )}
          </form.Subscribe>
          <form.Subscribe selector={(st) => st.values}>
            {(v) => <Preview spaceId={spaceId} input={toInput(v)} />}
          </form.Subscribe>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="missed">
              {(f) => <Select label="Missed runs" description="While central was stopped or the space archived" allowDeselect={false} value={f.state.value}
                onChange={(v) => v && f.handleChange(v as NonNullable<ScheduleInput['missed']>)}
                data={[{ value: 'skip', label: 'Skip them' }, { value: 'run_once', label: 'Run the latest once' }, { value: 'catch_up', label: 'Catch up (bounded)' }]} />}
            </form.Field>
            <form.Subscribe selector={(st) => st.values.missed}>
              {(m) => m === 'catch_up' && (
                <form.Field name="catch_up_max">
                  {(f) => <NumberInput label="Catch up at most" min={1} max={100} allowDecimal={false} value={f.state.value} onChange={(v) => f.handleChange(typeof v === 'number' ? v : 1)} />}
                </form.Field>
              )}
            </form.Subscribe>
            <form.Field name="skip_if_open">
              {(f) => <Checkbox mt="lg" label="Skip while the previous one is open" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          </SimpleGrid>
          <Title order={6}>What it creates</Title>
          <form.Field name="target">
            {(f) => <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as ScheduleInput['target'])} data={[{ value: 'inbox', label: 'Inbox item (control decides)' }, { value: 'task', label: 'Ready task' }]} />}
          </form.Field>
          <form.Field name="title" validators={{ onBlur: required('Title') }}>
            {(f) => <TextInput label="Title" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="body">
            {(f) => <Textarea label="Body" description="Markdown: the item's body or the task's objective" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="priority">
              {(f) => <Select label="Priority" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as NonNullable<ScheduleInput['priority']>)}
                data={['low', 'normal', 'high', 'urgent']} />}
            </form.Field>
            <form.Subscribe selector={(st) => st.values.target}>
              {(t) => t === 'task' && (
                <form.Field name="assignee_role">
                  {(f) => <TextInput label="Assignee role" placeholder="implementor" value={f.state.value} onChange={(e) => f.handleChange(e.target.value.trim())} />}
                </form.Field>
              )}
            </form.Subscribe>
            <form.Field name="enabled">
              {(f) => <Switch mt="lg" label="Enabled" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          </SimpleGrid>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button type="submit">{schedule ? 'Save' : 'Create'}</Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

interface FormValues {
  name: string; description: string; enabled: boolean; kind: ScheduleInput['kind']; cron: string; every: number; unit: string; timezone: string
  missed: NonNullable<ScheduleInput['missed']>; catch_up_max: number; skip_if_open: boolean; target: ScheduleInput['target']; title: string; body: string
  priority: NonNullable<ScheduleInput['priority']>; assignee_role: string
}

function toInput(v: FormValues): ScheduleInput {
  return {
    name: v.name.trim(), description: v.description, enabled: v.enabled, kind: v.kind, cron: v.kind === 'cron' ? v.cron.trim() : undefined,
    interval_seconds: v.kind === 'interval' ? v.every * Number(v.unit) : undefined, timezone: v.timezone, missed: v.missed,
    catch_up_max: v.missed === 'catch_up' ? v.catch_up_max : undefined, skip_if_open: v.skip_if_open, target: v.target, title: v.title.trim(), body: v.body,
    priority: v.priority, assignee_role: v.target === 'task' ? v.assignee_role || undefined : undefined,
  }
}

/** The next occurrences of the timing being edited, validated by the server. */
function Preview({ spaceId, input }: { spaceId: string; input: ScheduleInput }) {
  const timing = JSON.stringify({ kind: input.kind, cron: input.cron, interval_seconds: input.interval_seconds, timezone: input.timezone })
  const [debounced] = useDebouncedValue(timing, 300)
  const [state, setState] = useState<{ occ: string[]; error: string }>({ occ: [], error: '' })
  useEffect(() => {
    let live = true
    const t = JSON.parse(debounced) as Partial<ScheduleInput>
    void (async () => {
      try {
        const r = unwrap(await api.POST('/spaces/{space_id}/schedules/preview', { params: { path: { space_id: spaceId } },
          body: { ...t, kind: t.kind ?? 'cron', name: 'preview', title: 'preview', target: 'inbox' } }))
        if (live) setState({ occ: r.occurrences ?? [], error: '' })
      } catch (e) {
        const msg = e instanceof ApiError && Object.keys(e.fields).length ? Object.entries(e.fields).map(([k, m]) => `${k.replace(/^body\./, '')}: ${m}`).join('; ') : errorMessage(e)
        if (live) setState({ occ: [], error: msg })
      }
    })()
    return () => { live = false }
  }, [debounced, spaceId])
  return (
    <Card withBorder p="xs">
      <Text size="xs" fw={600} mb={2}>Next occurrences (your local time)</Text>
      {state.error ? <Text size="xs" c="owlRed" role="alert">{state.error}</Text>
        : <Text size="xs" c="dimmed">{state.occ.map((o) => datetime(o)).join(' · ') || '…'}</Text>}
    </Card>
  )
}
