import { Alert, Badge, Button, Card, Code, Drawer, Group, Modal, Select, SimpleGrid, Stack, Table, Tabs, Text } from '@mantine/core'
import { IconInfoCircle, IconPlus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type KBDetail, type KnowledgeBase } from '@/api/client'
import { useKB, useKBs, useMe, useSpace, useSpaceRoles } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { useAppForm } from '@/components/form/appForm'
import { PageHeader } from '@/components/PageHeader'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/knowledge')({ component: KnowledgePage })

const policyLabel: Record<string, string> = { read_only: 'Read-only', review: 'Reviewed proposals', serialized: 'Serialized acceptance' }
const reqColor: Record<string, string> = { queued: 'gray', dispatched: 'owlBlue', answered: 'owlGreen', failed: 'owlRed', cancelled: 'gray', timed_out: 'owlYellow' }
const propColor: Record<string, string> = { proposed: 'owlBlue', accepting: 'owlBlue', accepted: 'owlGreen', rejected: 'gray', conflict: 'owlYellow', failed: 'owlRed' }

/**
 * Knowledge bases (SM-KB-007): repositories, per-requester support sessions, pending questions with evidence,
 * proposals, capacity, failures and usage.
 */
function KnowledgePage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const kbs = useKBs(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const [open, setOpen] = useState<string | null>(null)
  const [editing, setEditing] = useState<KnowledgeBase | 'new' | null>(null)
  return (
    <>
      <PageHeader title={space.data?.name ?? 'Space'} description="Knowledge bases agents ask for context, decisions and documentation. Each requester gets its own support session."
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add knowledge base</Button>} />
      <SpaceNav spaceId={spaceId} />
      <ErrorAlert error={kbs.error} />
      <SimpleGrid cols={{ base: 1, md: 2 }}>
        {(kbs.data ?? []).map((k) => (
          <Card key={k.id} withBorder style={{ cursor: 'pointer' }} onClick={() => setOpen(k.id)}>
            <Group justify="space-between" mb={4}>
              <Text fw={600}>{k.name}</Text>
              <Group gap={4}>{!k.enabled && <Badge color="gray">disabled</Badge>}<Badge variant="light">{policyLabel[k.write_policy]}</Badge></Group>
            </Group>
            <Text size="xs" c="dimmed" mb="xs" className="mono">{k.repo} · {k.base_branch} · role {k.space_role}</Text>
            {k.description && <Text size="sm" mb="xs">{k.description}</Text>}
            <Group gap="lg" align="flex-start">
              <Stat label="Sessions" value={`${k.stats?.active_sessions ?? 0} of ${k.max_sessions} running`} hint={`${k.stats?.sessions ?? 0} requesters`} />
              <Stat label="Questions" value={`${k.stats?.open_requests ?? 0} open`} hint={`${k.stats?.answered ?? 0} answered · ${k.stats?.failed ?? 0} failed`} />
              <Stat label="Proposals" value={`${k.stats?.pending_proposals ?? 0} pending`} />
            </Group>
          </Card>
        ))}
      </SimpleGrid>
      {kbs.isSuccess && kbs.data.length === 0 && (
        <Card><Text size="sm" c="dimmed" ta="center">No knowledge bases. Add a Git repository from the space folder and the space role its sessions run as.</Text></Card>
      )}
      <Drawer opened={open !== null} onClose={() => setOpen(null)} position="right" size="xl" title="Knowledge base">
        {open && <KBDrawer spaceId={spaceId} id={open} canWrite={canWrite} onEdit={(k) => setEditing(k)} />}
      </Drawer>
      {editing && <KBModal spaceId={spaceId} kb={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </>
  )
}

function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm">{value}</Text>
      {hint && <Text size="xs" c="dimmed">{hint}</Text>}
    </div>
  )
}

function KBDrawer({ spaceId, id, canWrite, onEdit }: { spaceId: string; id: string; canWrite: boolean; onEdit: (k: KnowledgeBase) => void }) {
  const d = useKB(spaceId, id)
  if (!d.data) return <ErrorAlert error={d.error} />
  const k = d.data.kb
  return (
    <Stack gap="sm">
      <Group justify="space-between">
        <div>
          <Text fw={600}>{k.name}</Text>
          <Text size="xs" c="dimmed" className="mono">{k.slug} · {k.repo} → {k.base_branch}</Text>
        </div>
        {canWrite && <Button size="xs" variant="default" onClick={() => onEdit(k)}>Edit…</Button>}
      </Group>
      <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />}>
        Answers and proposals are written by agents. Knowledge is what is on {k.base_branch}; a proposal is not knowledge until it is accepted.
      </Alert>
      <Tabs defaultValue="requests">
        <Tabs.List>
          <Tabs.Tab value="requests">Questions ({d.data.requests.length})</Tabs.Tab>
          <Tabs.Tab value="sessions">Sessions ({d.data.sessions.length})</Tabs.Tab>
          <Tabs.Tab value="proposals">Proposals ({d.data.proposals.length})</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="requests" pt="sm"><Requests d={d.data} /></Tabs.Panel>
        <Tabs.Panel value="sessions" pt="sm"><Sessions spaceId={spaceId} d={d.data} /></Tabs.Panel>
        <Tabs.Panel value="proposals" pt="sm"><Proposals spaceId={spaceId} d={d.data} canWrite={canWrite} /></Tabs.Panel>
      </Tabs>
    </Stack>
  )
}

function Requests({ d }: { d: KBDetail }) {
  if (d.requests.length === 0) return <Text size="sm" c="dimmed">No questions yet.</Text>
  return (
    <Stack gap="xs">
      {d.requests.map((r) => (
        <Card key={r.id} withBorder p="sm">
          <Group justify="space-between" mb={4}>
            <Group gap={6}><Badge variant="light">{r.kind}</Badge><Text size="xs" c="dimmed">from {r.requester_role} · {ago(r.created_at)}{r.task_id && ` · task ${r.task_id}`}</Text></Group>
            <Badge color={reqColor[r.state]} variant="light">{r.state.replace('_', ' ')}</Badge>
          </Group>
          <Text size="sm" fw={500}>{r.question}</Text>
          {r.answer && (
            <Card mt="xs" p="xs" withBorder>
              <Group gap={6} mb={4}><Text size="xs" c="dimmed">Answer</Text>{r.basis && <Badge size="xs" color={r.basis === 'committed' ? 'owlGreen' : r.basis === 'proposal' ? 'owlYellow' : 'gray'}>{r.basis === 'committed' ? 'from accepted knowledge' : r.basis === 'proposal' ? 'from a proposal' : 'not known'}</Badge>}</Group>
              <MarkdownView source={r.answer} spaceId={d.kb.space_id} />
              {r.evidence.length > 0 && (
                <Text size="xs" c="dimmed" mt={4}>Evidence: {r.evidence.map((e) => `${e.path}${e.revision ? '@' + e.revision.slice(0, 8) : ''}${e.kind === 'proposal' ? ' (proposal)' : ''}`).join(' · ')}</Text>
              )}
            </Card>
          )}
          {r.error && <Text size="xs" c="owlRed" mt={4}>{r.error}</Text>}
        </Card>
      ))}
    </Stack>
  )
}

function Sessions({ spaceId, d }: { spaceId: string; d: KBDetail }) {
  if (d.sessions.length === 0) return <Text size="sm" c="dimmed">No sessions yet. A requester's first question starts its session.</Text>
  return (
    <Table>
      <Table.Thead><Table.Tr><Table.Th>Requester</Table.Th><Table.Th>State</Table.Th><Table.Th>Answered</Table.Th><Table.Th>Last active</Table.Th></Table.Tr></Table.Thead>
      <Table.Tbody>
        {d.sessions.map((s) => (
          <Table.Tr key={s.id}>
            <Table.Td><Text size="sm">{s.requester_label || s.requester_role}</Text><Text size="xs" c="dimmed" className="mono">{s.requester_instance}</Text></Table.Td>
            <Table.Td>
              <Badge variant="light" color={s.state === 'active' ? 'owlGreen' : s.state === 'failed' ? 'owlRed' : 'gray'}>{s.state}</Badge>
              {s.run_id && <Text size="xs"><Link to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: s.run_id }}>run</Link></Text>}
              {s.last_error && <Text size="xs" c="owlRed">{s.last_error}</Text>}
            </Table.Td>
            <Table.Td>{s.answered}{s.failures > 0 && <Text size="xs" c="dimmed">{s.failures} failure(s)</Text>}</Table.Td>
            <Table.Td><Text size="xs">{ago(s.last_active_at)}</Text></Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  )
}

function Proposals({ spaceId, d, canWrite }: { spaceId: string; d: KBDetail; canWrite: boolean }) {
  const qc = useQueryClient()
  const act = async (id: string, what: 'accept' | 'reject') => {
    try {
      if (what === 'accept') unwrap(await api.POST('/spaces/{space_id}/kb-proposals/{proposal_id}/accept', { params: { path: { space_id: spaceId, proposal_id: id } } }))
      else unwrap(await api.POST('/spaces/{space_id}/kb-proposals/{proposal_id}/reject', { params: { path: { space_id: spaceId, proposal_id: id } }, body: {} }))
      void qc.invalidateQueries({ queryKey: ['kbs', spaceId] })
      notifyOk(what === 'accept' ? 'Sent to the node for acceptance' : 'Rejected')
    } catch (e) {
      notifyError(e, `Could not ${what} the proposal`)
    }
  }
  if (d.proposals.length === 0) return <Text size="sm" c="dimmed">No proposals. {d.kb.write_policy === 'read_only' ? 'This knowledge base is read-only for sessions.' : 'Sessions propose changes from their own branches.'}</Text>
  return (
    <Stack gap="xs">
      {d.proposals.map((p) => (
        <Card key={p.id} withBorder p="sm">
          <Group justify="space-between">
            <Text size="sm" fw={500}>{p.title}</Text>
            <Badge color={propColor[p.state]} variant="light">{p.state}</Badge>
          </Group>
          {p.summary && <MarkdownView source={p.summary} spaceId={spaceId} />}
          <Text size="xs" c="dimmed"><span className="mono">{p.branch}@{p.commit.slice(0, 8)}</span>{p.merged_commit && <> · merged as <span className="mono">{p.merged_commit.slice(0, 8)}</span></>} · {datetime(p.updated_at)}</Text>
          {p.detail && <Code block mt={4}>{p.detail}</Code>}
          {canWrite && ['proposed', 'conflict', 'failed'].includes(p.state) && d.kb.write_policy === 'review' && (
            <Group justify="flex-end" mt={4}>
              <Button size="xs" variant="default" onClick={() => void act(p.id, 'reject')}>Reject</Button>
              <Button size="xs" onClick={() => void act(p.id, 'accept')}>Accept</Button>
            </Group>
          )}
        </Card>
      ))}
    </Stack>
  )
}

function KBModal({ spaceId, kb, onClose }: { spaceId: string; kb: KnowledgeBase | null; onClose: () => void }) {
  const qc = useQueryClient()
  const roles = useSpaceRoles(spaceId).data ?? []
  const [error, setError] = useState('')
  const form = useAppForm({
    defaultValues: { slug: kb?.slug ?? '', name: kb?.name ?? '', description: kb?.description ?? '', repo: kb?.repo ?? '', base_branch: kb?.base_branch ?? 'main',
      space_role: kb?.space_role ?? '', entry_instructions: kb?.entry_instructions ?? '', validator: kb?.validator ?? [], write_policy: (kb?.write_policy ?? 'review') as 'read_only' | 'review' | 'serialized',
      allowed_roles: kb?.allowed_roles ?? ['*'], max_sessions: kb?.max_sessions ?? 2, idle_minutes: Math.round((kb?.idle_sleep_seconds ?? 900) / 60),
      timeout_minutes: Math.round((kb?.request_timeout_seconds ?? 1800) / 60), enabled: kb?.enabled ?? true },
    onSubmit: async ({ value: v }) => {
      setError('')
      const body = { slug: kb ? undefined : v.slug, name: v.name, description: v.description, repo: v.repo, base_branch: v.base_branch, space_role: v.space_role,
        entry_instructions: v.entry_instructions, validator: v.validator, write_policy: v.write_policy, allowed_roles: v.allowed_roles, max_sessions: v.max_sessions,
        idle_sleep_seconds: v.idle_minutes * 60, request_timeout_seconds: v.timeout_minutes * 60, enabled: v.enabled }
      try {
        if (kb) unwrap(await api.PUT('/spaces/{space_id}/kbs/{kb_id}', { params: { path: { space_id: spaceId, kb_id: kb.id }, header: { 'If-Match': `"${kb.version}"` } }, body }))
        else unwrap(await api.POST('/spaces/{space_id}/kbs', { params: { path: { space_id: spaceId } }, body }))
        void qc.invalidateQueries({ queryKey: ['kbs', spaceId] })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={kb ? `Edit ${kb.name}` : 'Add a knowledge base'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <SimpleGrid cols={2}>
            <form.AppField name="name">{(f) => <f.TextField label="Name" />}</form.AppField>
            <form.AppField name="slug">{(f) => <f.TextField label="Slug" classNames={{ input: 'mono' }} disabled={kb !== null} />}</form.AppField>
            <form.AppField name="repo">{(f) => <f.TextField label="Repository" description="A Git repository folder in the space" placeholder="repos/payments-kb" classNames={{ input: 'mono' }} />}</form.AppField>
            <form.AppField name="base_branch">{(f) => <f.TextField label="Knowledge branch" description="Accepted knowledge; must not be checked out" classNames={{ input: 'mono' }} />}</form.AppField>
          </SimpleGrid>
          <form.AppField name="description">{(f) => <f.TextareaField label="Description" autosize minRows={1} />}</form.AppField>
          <form.AppField name="space_role">{(f) => <f.SelectField label="Sessions run as" description="A space role whose template is of kind kb" data={roles.map((r) => r.name)} />}</form.AppField>
          <form.AppField name="entry_instructions">
            {(f) => <f.TextareaField label="Entry instructions" description="What a session reads first, e.g. 'Follow AGENTS.md; you are a consumer: propose, do not accept.'" autosize minRows={2} />}
          </form.AppField>
          <form.Field name="write_policy">
            {(f) => <Select label="Write policy" allowDeselect={false} value={f.state.value} onChange={(x) => f.handleChange((x ?? 'review') as typeof f.state.value)}
              data={[{ value: 'read_only', label: 'Read-only: sessions answer, never change the KB' }, { value: 'review', label: 'Reviewed: a person accepts proposals' },
                { value: 'serialized', label: 'Serialized: proposals are accepted one at a time after the check' }]} />}
          </form.Field>
          <form.AppField name="validator">{(f) => <f.TagsField label="Check before accepting" description="A command run on the merged result, e.g. python3 tools/kb.py check" />}</form.AppField>
          <form.AppField name="allowed_roles">{(f) => <f.TagsField label="Admitted roles" description="Requester roles that may ask; * admits every role" />}</form.AppField>
          <SimpleGrid cols={3}>
            <form.AppField name="max_sessions">{(f) => <f.NumberField label="Sessions at once" min={1} max={16} fallback={2} />}</form.AppField>
            <form.AppField name="idle_minutes">{(f) => <f.NumberField label="Sleep when idle (min)" min={1} max={1440} fallback={15} />}</form.AppField>
            <form.AppField name="timeout_minutes">{(f) => <f.NumberField label="Question timeout (min)" min={1} max={1440} fallback={30} />}</form.AppField>
          </SimpleGrid>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.AppForm><form.SubmitButton>Save</form.SubmitButton></form.AppForm>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
