import { Badge, Button, Card, Checkbox, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Table, Text, TextInput, Title } from '@mantine/core'
import { useForm, useSelector } from '@tanstack/react-form'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type HumanPolicy, type HumanPolicyFields } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { notifyError, notifyOk } from '@/lib/notify'
import { typeLabel } from './badges'

type PolicyType = HumanPolicy['action_type']
const TYPES: PolicyType[] = ['all', 'question', 'approval', 'todo', 'message', 'custom']
const typeName = (t: PolicyType) => (t === 'all' ? 'Every type' : t === 'custom' ? 'Custom types' : typeLabel[t])
const ROLE_RE = /^[a-z][a-z0-9-]{0,47}$/

/** Minutes as a short duration: 90 → "1h 30m", 1440 → "1d". */
export function minutes(m: number): string {
  if (m <= 0) return '—'
  const d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60), r = m % 60
  return [d && `${d}d`, h && `${h}h`, r && `${r}m`].filter(Boolean).join(' ')
}

function sourceLabel(src?: string): string {
  if (!src || src === 'built-in') return 'built-in'
  const [scope, t] = src.split(':')
  return `${scope === 'space' ? 'this space' : 'default'} · ${typeName(t as PolicyType).toLowerCase()}`
}

/** One line per concern, for the table. */
export function policySummary(p: HumanPolicyFields) {
  const remind = p.remind_after_minutes > 0
    ? `after ${minutes(p.remind_after_minutes)}${p.remind_every_minutes > 0 && p.max_reminders > 1 ? `, then every ${minutes(p.remind_every_minutes)} (${p.max_reminders}×)` : ''}`
    : 'none'
  const escalate = p.escalate_after_minutes > 0
    ? `after ${minutes(p.escalate_after_minutes)}: ${[p.escalate_priority && `priority ${p.escalate_priority}`, p.escalate_assignee && `assign ${p.escalate_assignee}`].filter(Boolean).join(', ')}`
    : 'never'
  const ended = p.requester_ended === 'next_launch'
    ? `wait ${minutes(p.hold_minutes)} for its next run, then ${p.successor_role || 'control'}`
    : `to ${p.successor_role ? `${p.successor_role}, else control` : 'control'}`
  const inbox = p.response_inbox === 'always' ? 'every response becomes an inbox item'
    : p.response_inbox === 'routed' ? 'routed responses become inbox items' : ''
  return { remind, escalate, ended, inbox, rate: `${p.rate_per_run_hour}/run · ${p.rate_per_space_hour}/space per hour` }
}

/** The response_inbox choices (SM-INBOX-001, ADR-0049). */
const responseInboxOptions = [
  { value: 'off', label: 'Notification only' },
  { value: 'routed', label: 'Also an inbox item when routed to control or the operators' },
  { value: 'always', label: 'Also an inbox item for every response' },
]

/**
 * Reminders, escalation, rate limits and routing of responses (SM-HUM-009/011), per action type. Without spaceId it
 * edits the installation defaults; with it, the space's overrides. The most specific policy applies as a whole.
 */
export function PoliciesPanel({ spaceId, canWrite }: { spaceId?: string; canWrite: boolean }) {
  const qc = useQueryClient()
  const queryKey = keys.humanPolicies(spaceId ?? '')
  const list = useQuery({ queryKey, queryFn: async ({ signal }) => unwrap(spaceId
    ? await api.GET('/spaces/{space_id}/human-policies', { params: { path: { space_id: spaceId } }, signal })
    : await api.GET('/human-policies', { signal })) })
  const [editing, setEditing] = useState<PolicyType | null>(null)
  const stored = (t: PolicyType) => list.data?.items.find((p) => p.action_type === t)
  const effective = (t: PolicyType) => list.data?.effective.find((p) => p.action_type === t)
  const remove = async (t: PolicyType) => {
    try {
      const r = spaceId
        ? await api.DELETE('/spaces/{space_id}/human-policies/{action_type}', { params: { path: { space_id: spaceId, action_type: t } } })
        : await api.DELETE('/human-policies/{action_type}', { params: { path: { action_type: t } } })
      if (r.error) throw r.error
      void qc.invalidateQueries({ queryKey: ['human-policies'] })
      notifyOk(`${typeName(t)}: ${spaceId ? 'the default applies again' : 'the built-in policy applies again'}`)
    } catch (e) {
      notifyError(e, 'Could not remove the policy')
    }
  }
  return (
    <Stack gap="sm">
      <ErrorAlert error={list.error} />
      <Table.ScrollContainer minWidth={760}>
        <Table aria-label="Policies in force">
          <Table.Thead>
            <Table.Tr><Table.Th>Type</Table.Th><Table.Th>Reminders</Table.Th><Table.Th>Escalation</Table.Th><Table.Th>Requester ended</Table.Th><Table.Th>Rate limits</Table.Th><Table.Th /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(list.data?.effective ?? []).map((p) => {
              const s = policySummary(p)
              return (
                <Table.Tr key={p.action_type}>
                  <Table.Td><Text size="sm" fw={500}>{typeName(p.action_type)}</Text><Text size="xs" c="dimmed">{sourceLabel(p.source)}</Text></Table.Td>
                  <Table.Td><Text size="sm">{s.remind}</Text></Table.Td>
                  <Table.Td><Text size="sm">{s.escalate}</Text></Table.Td>
                  <Table.Td>
                    <Text size="sm">{s.ended}</Text>{!p.wake_idle && <Text size="xs" c="dimmed">no terminal notice</Text>}
                    {s.inbox && <Text size="xs" c="dimmed">{s.inbox}</Text>}
                  </Table.Td>
                  <Table.Td><Text size="sm">{s.rate}</Text></Table.Td>
                  <Table.Td>{canWrite && <Button size="xs" variant="default" onClick={() => setEditing(p.action_type)}>Edit</Button>}</Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {(list.data?.items ?? []).length > 0 && (
        <Group gap="xs">
          <Text size="xs" c="dimmed">{spaceId ? 'Overrides in this space:' : 'Stored defaults:'}</Text>
          {list.data!.items.map((p) => (
            <Badge key={p.action_type} variant="light" rightSection={canWrite && (
              <Button size="compact-xs" variant="transparent" aria-label={`Remove the ${typeName(p.action_type)} policy`} onClick={() => void remove(p.action_type)}>×</Button>
            )}>{typeName(p.action_type)} · v{p.version}</Badge>
          ))}
        </Group>
      )}
      {editing && (
        <PolicyModal spaceId={spaceId} type={editing} stored={stored(editing)} start={stored(editing) ?? effective(editing) ?? effective('question')!}
          onClose={() => setEditing(null)} onSaved={() => void qc.invalidateQueries({ queryKey: ['human-policies'] })} />
      )}
    </Stack>
  )
}

function PolicyModal({ spaceId, type, stored, start, onClose, onSaved }: {
  spaceId?: string; type: PolicyType; stored?: HumanPolicy; start: HumanPolicyFields; onClose: () => void; onSaved: () => void
}) {
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: {
      scope: type as PolicyType,
      remind_after_minutes: start.remind_after_minutes, remind_every_minutes: start.remind_every_minutes, max_reminders: start.max_reminders,
      escalate_after_minutes: start.escalate_after_minutes, escalate_priority: start.escalate_priority, escalate_assignee: start.escalate_assignee,
      requester_ended: start.requester_ended, hold_minutes: start.hold_minutes, successor_role: start.successor_role, wake_idle: start.wake_idle,
      rate_per_run_hour: start.rate_per_run_hour, rate_per_space_hour: start.rate_per_space_hour,
      response_inbox: start.response_inbox ?? 'off',
    } satisfies HumanPolicyFields & { scope: PolicyType },
    onSubmit: async ({ value: { scope, ...value } }) => {
      setError('')
      try {
        const header = stored && scope === type ? { 'If-Match': ifMatch(stored.version!) } : {}
        if (spaceId) {
          unwrap(await api.PUT('/spaces/{space_id}/human-policies/{action_type}', { params: { path: { space_id: spaceId, action_type: scope }, header }, body: value }))
        } else {
          unwrap(await api.PUT('/human-policies/{action_type}', { params: { path: { action_type: scope }, header }, body: value }))
        }
        onSaved()
        notifyOk('Policy saved. Pending actions use it now.')
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const num = (name: 'remind_after_minutes' | 'remind_every_minutes' | 'max_reminders' | 'escalate_after_minutes' | 'hold_minutes' | 'rate_per_run_hour' | 'rate_per_space_hour',
    label: string, description: string, min: number, max: number) => (
    <form.Field name={name}>
      {(f) => <NumberInput label={label} description={description} min={min} max={max} allowDecimal={false} value={f.state.value}
        onChange={(v) => f.handleChange(typeof v === 'number' ? v : 0)} />}
    </form.Field>
  )
  const scope = useSelector(form.store, (st) => st.values.scope)
  return (
    <Modal opened onClose={onClose} title={`${spaceId ? 'Space policy' : 'Default policy'}: ${typeName(scope)}`} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="scope">
            {(f) => <Select label="Applies to" allowDeselect={false} value={f.state.value} onChange={(v) => f.handleChange((v ?? type) as PolicyType)}
              description={spaceId ? 'A type-specific policy wins over "every type"; the installation defaults apply where the space has none.' : 'Spaces can override these.'}
              data={TYPES.map((t) => ({ value: t, label: typeName(t) }))} />}
          </form.Field>
          <Title order={5}>Reminders</Title>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            {num('remind_after_minutes', 'First after (min)', '0: no reminders', 0, 43200)}
            {num('remind_every_minutes', 'Then every (min)', '0: once', 0, 43200)}
            {num('max_reminders', 'At most', 'reminders', 1, 20)}
          </SimpleGrid>
          <Title order={5}>Escalation</Title>
          <Text size="xs" c="dimmed">Raises the priority (never lowers it) and/or assigns someone. The order of the queue does not change.</Text>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            {num('escalate_after_minutes', 'After (min)', '0: never', 0, 43200)}
            <form.Field name="escalate_priority">
              {(f) => <Select label="Raise priority to" allowDeselect={false} value={f.state.value || 'none'} onChange={(v) => f.handleChange((v === 'none' || !v ? '' : v) as HumanPolicyFields['escalate_priority'])}
                data={[{ value: 'none', label: 'Keep' }, { value: 'high', label: 'High' }, { value: 'urgent', label: 'Urgent' }]} />}
            </form.Field>
            <form.Field name="escalate_assignee">
              {(f) => <TextInput label="Assign to" description="A user ID" placeholder="usr_…" value={f.state.value} onChange={(e) => f.handleChange(e.target.value.trim())} />}
            </form.Field>
          </SimpleGrid>
          <Title order={5}>When the requester's run has ended</Title>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="requester_ended">
              {(f) => <Select label="Response goes" allowDeselect={false} value={f.state.value} onChange={(v) => f.handleChange((v ?? 'reroute') as HumanPolicyFields['requester_ended'])}
                data={[{ value: 'reroute', label: 'On at once' }, { value: 'next_launch', label: 'To its next run' }]} />}
            </form.Field>
            {num('hold_minutes', 'Wait for the next run (min)', 'then route on', 1, 10080)}
            <form.Field name="successor_role" validators={{ onChange: ({ value }) => (value && !ROLE_RE.test(value) ? 'a role name: lowercase letters, digits, dashes' : undefined) }}>
              {(f) => <TextInput label="Successor role" description="Before control; empty: control" placeholder="reviewer" value={f.state.value}
                error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined} onChange={(e) => f.handleChange(e.target.value.trim())} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="wake_idle">
            {(f) => <Checkbox label="Wake an idle run with a one-line terminal notice" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <form.Field name="response_inbox">
            {(f) => <Select label="Responses in the inbox" allowDeselect={false} value={f.state.value}
              onChange={(v) => f.handleChange((v ?? 'off') as NonNullable<HumanPolicyFields['response_inbox']>)}
              description="An inbox item names the action and how it was routed, never the response itself, which stays on the action for the runs it concerns. Control triages it like any other input."
              data={responseInboxOptions} />}
          </form.Field>
          <Title order={5}>Rate limits for agents</Title>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            {num('rate_per_run_hour', 'Per run per hour', 'new actions of this type', 1, 1000)}
            {num('rate_per_space_hour', 'Per space per hour', 'new actions of this type', 1, 10000)}
          </SimpleGrid>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

/** The installation defaults, as a card for the human action types page. */
export function DefaultPoliciesCard({ canWrite }: { canWrite: boolean }) {
  return (
    <Card mt="lg">
      <Title order={4} mb={4}>Reminders, escalation and routing</Title>
      <Text size="sm" c="dimmed" mb="sm">
        Installation defaults per action type. Spaces override them on their Human actions tab. Agents never wait in a loop: they are notified of
        every response, and a response for a run that has ended goes to the successor role or control.
      </Text>
      <PoliciesPanel canWrite={canWrite} />
    </Card>
  )
}
