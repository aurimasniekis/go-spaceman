import { Alert, Badge, Button, Card, Drawer, Group, Modal, Select, SimpleGrid, Stack, Table, Text, Textarea, TextInput, Title, Tooltip } from '@mantine/core'
import { IconInfoCircle, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type Repository, type RepositoryInput, type RepositoryStatus } from '@/api/client'
import { keys, useMe, useRepositories, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { fieldError, required } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/repositories')({ component: RepositoriesPage })

const STATE_COLOR: Record<string, string> = { present: 'owlGreen', missing: 'gray', error: 'owlRed' }
const POLICY_LABEL: Record<string, string> = { auto: 'Cloned on the lease', on_demand: 'Cloned when needed', manual: 'Never cloned' }

const short = (c?: string) => (c ? c.slice(0, 10) : '—')

/**
 * Repositories and workspaces of a space (SM-REPO-001/003, SM-UI-002, ADR-0044): the configured repositories, the node's
 * last report of each (remote, default branch, last fetch, changes, unpushed commits) and every worktree with the run
 * that owns it. Report contents come from the node and the repositories: they are shown as text, never as markup.
 */
function RepositoriesPage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const list = useRepositories(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const qc = useQueryClient()
  const [open, setOpen] = useState<string | null>(null)
  const [editing, setEditing] = useState<Repository | 'new' | null>(null)
  const ov = list.data
  const items = ov?.items ?? []
  const sync = async (names?: string[]) => {
    try {
      const r = unwrap(await api.POST('/spaces/{space_id}/repositories/sync', { params: { path: { space_id: spaceId } }, body: { repositories: names } }))
      void qc.invalidateQueries({ queryKey: keys.repositories(spaceId) })
      notifyOk(`Sync requested: ${(r.fetch ?? []).length} to fetch; the node reports when it is done`)
    } catch (e) {
      notifyError(e, 'Could not request a sync')
    }
  }
  return (
    <>
      <PageHeader title={space.data?.name ?? 'Space'}
        description="The space's Git repositories: the node clones them into the space folder with its own Git authentication, and workers get worktrees of them. SpaceMan never merges, pushes, resets or deletes."
        actions={canWrite && (
          <Group gap="xs">
            <Button variant="default" leftSection={<IconRefresh size={14} />} disabled={items.length === 0} onClick={() => void sync()}>Sync now</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add repository</Button>
          </Group>
        )} />
      <SpaceNav spaceId={spaceId} />
      <ErrorAlert error={list.error} />
      {ov && items.length > 0 && (
        <Text size="xs" c="dimmed" mb="xs">
          {ov.reported_at ? <>Node report {ago(ov.reported_at)} ({datetime(ov.reported_at)}).</> : 'The node has not reported yet.'}
          {ov.sync_pending && ' A sync is in progress.'}
        </Text>
      )}
      {ov?.stale && (
        <Alert color="owlYellow" icon={<IconInfoCircle size={16} />} mb="sm" title="Report from the previous node">
          The space moved to another node; this report is from before the move. Sync to get the current node's view.
        </Alert>
      )}
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Repository</Table.Th>
              <Table.Th>Remote</Table.Th>
              <Table.Th>Default branch</Table.Th>
              <Table.Th>State</Table.Th>
              <Table.Th>Changes</Table.Th>
              <Table.Th>Unpushed</Table.Th>
              <Table.Th>Worktrees</Table.Th>
              <Table.Th>Last fetch</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {items.map((r) => {
              const st = r.status
              return (
                <Table.Tr key={r.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(r.id)} onKeyDown={(e) => e.key === 'Enter' && setOpen(r.id)} tabIndex={0}>
                  <Table.Td>
                    <Group gap={6}><Text size="sm" fw={500}>{r.name}</Text>{r.role === 'kb' && <Badge size="xs" variant="light">KB</Badge>}</Group>
                    <Text size="xs" c="dimmed" className="mono">{r.path}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" className="mono" style={{ wordBreak: 'break-all' }}>{r.url || '—'}</Text>
                    {st?.remote && (st.remote.reachable
                      ? <Badge size="xs" color="owlGreen" variant="light">reachable</Badge>
                      : <Tooltip label={st.remote.error ?? ''} multiline w={320}><Badge size="xs" color="owlRed" variant="light">unreachable</Badge></Tooltip>)}
                  </Table.Td>
                  <Table.Td><DefaultBranch r={r} st={st} /></Table.Td>
                  <Table.Td>
                    {st ? <Badge variant="light" color={STATE_COLOR[st.state]}>{st.state}</Badge> : <Text size="sm" c="dimmed">not reported</Text>}
                    <Text size="xs" c="dimmed">{POLICY_LABEL[r.clone_policy]}</Text>
                  </Table.Td>
                  <Table.Td><Text size="sm">{st?.state === 'present' ? `${st.dirty} changed · ${st.untracked} untracked` : '—'}</Text></Table.Td>
                  <Table.Td>{st ? <Text size="sm" c={st.unpushed > 0 ? 'owlYellow' : undefined} fw={st.unpushed > 0 ? 600 : undefined}>{st.unpushed}</Text> : '—'}</Table.Td>
                  <Table.Td><Text size="sm">{st ? (st.worktrees ?? []).length : '—'}</Text></Table.Td>
                  <Table.Td>{st?.last_fetch_at ? <Tooltip label={datetime(st.last_fetch_at)}><Text size="sm">{ago(st.last_fetch_at)}</Text></Tooltip> : <Text size="sm" c="dimmed">never</Text>}</Table.Td>
                </Table.Tr>
              )
            })}
            {list.isSuccess && items.length === 0 && (
              <Table.Tr><Table.Td colSpan={8}><Text size="sm" c="dimmed" ta="center" p="md">No repositories yet. Add one with its clone URL; the node clones it into the space folder.</Text></Table.Td></Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
      {(ov?.unlisted ?? []).length > 0 && (
        <Card mt="sm" withBorder>
          <Text size="sm" fw={600}>Repositories in the space folder that are not listed</Text>
          <Text size="xs" c="dimmed" mb={4}>Agents or operators put them there. Add one with its folder as the path to track it and clone it on another node.</Text>
          <Group gap={6}>{ov!.unlisted.map((p) => <Badge key={p} variant="outline" color="gray" className="mono">{p}</Badge>)}</Group>
        </Card>
      )}
      <RepositoryDrawer spaceId={spaceId} repo={items.find((r) => r.id === open) ?? null} canWrite={canWrite} onClose={() => setOpen(null)}
        onEdit={(r) => { setOpen(null); setEditing(r) }} onSync={(names) => void sync(names)} />
      {editing && <RepositoryModal spaceId={spaceId} repo={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </>
  )
}

function DefaultBranch({ r, st }: { r: Repository; st?: RepositoryStatus }) {
  const behind = st?.tracking_head && st.default_head && st.tracking_head !== st.default_head
  return (
    <>
      <Text size="sm" className="mono">{r.default_branch}</Text>
      {st && (
        <Text size="xs" c="dimmed" className="mono">
          local {short(st.default_head)}{st.remote?.head ? ` · remote ${short(st.remote.head)}` : ''}
          {behind && <Text span size="xs" c="owlYellow"> · differs from the remote-tracking branch</Text>}
        </Text>
      )}
    </>
  )
}

function RepositoryDrawer({ spaceId, repo, canWrite, onClose, onEdit, onSync }: {
  spaceId: string; repo: Repository | null; canWrite: boolean; onClose: () => void; onEdit: (r: Repository) => void; onSync: (names: string[]) => void
}) {
  const qc = useQueryClient()
  const st = repo?.status
  const remove = async () => {
    if (!repo || !window.confirm(`Remove ${repo.name} from the space? Its folder on the node and the upstream repository stay.`)) return
    try {
      check(await api.DELETE('/spaces/{space_id}/repositories/{repository_id}', {
        params: { path: { space_id: spaceId, repository_id: repo.id }, header: { 'If-Match': ifMatch(repo.version) } } }))
      void qc.invalidateQueries({ queryKey: keys.repositories(spaceId) })
      notifyOk('Repository removed; its folder on the node was kept')
      onClose()
    } catch (e) {
      notifyError(e, 'Could not remove the repository')
    }
  }
  return (
    <Drawer opened={!!repo} onClose={onClose} position="right" size="xl" title={repo?.name ?? 'Repository'}>
      {repo && (
        <Stack gap="sm">
          {repo.description && <Text size="sm">{repo.description}</Text>}
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Field label="Folder" value={repo.path} mono />
            <Field label="Clone URL" value={repo.url || 'none (manual)'} mono />
            <Field label="Default branch" value={repo.default_branch} mono />
            <Field label="Clone policy" value={POLICY_LABEL[repo.clone_policy] ?? repo.clone_policy} />
            <Field label="Role" value={repo.role === 'kb' ? 'Knowledge base' : 'Code'} />
            <Field label="Knowledge bases" value={(repo.knowledge_bases ?? []).join(', ') || '—'} />
          </SimpleGrid>
          <Text size="xs" c="dimmed">Git authentication is the node's own (SSH keys, credential helpers, Hallpass Git roles); the URL never holds credentials.</Text>
          <Title order={6}>Node report</Title>
          {!st ? <Text size="sm" c="dimmed">Not reported yet. Sync to ask the node.</Text> : (
            <>
              <Group gap="xs">
                <Badge variant="light" color={STATE_COLOR[st.state]}>{st.state}</Badge>
                {st.stale && <Badge color="owlYellow" variant="outline">previous node</Badge>}
                {st.cloned && <Badge variant="outline">cloned by the last sync</Badge>}
                {st.fetched && <Badge variant="outline">fetched by the last sync</Badge>}
                <Text size="xs" c="dimmed">as of {datetime(st.reported_at)}</Text>
              </Group>
              {st.error && <Alert color="owlRed" title="Problem">{st.error}</Alert>}
              <SimpleGrid cols={{ base: 1, sm: 2 }}>
                <Field label="Remote" value={st.remote ? (st.remote.reachable ? `reachable, ${repo.default_branch} at ${short(st.remote.head)}` : `unreachable: ${st.remote.error ?? ''}`) : '—'} />
                <Field label="Last fetch" value={st.last_fetch_at ? `${ago(st.last_fetch_at)} (${datetime(st.last_fetch_at)})` : 'never'} />
                <Field label={`Local ${repo.default_branch}`} value={short(st.default_head)} mono />
                <Field label={`origin/${repo.default_branch}`} value={short(st.tracking_head)} mono />
                <Field label="Checked out" value={st.branch ? `${st.branch} at ${short(st.head)}` : st.head ? `detached at ${short(st.head)}` : '—'} mono />
                <Field label="Main folder" value={`${st.dirty} changed · ${st.untracked} untracked · ${st.stashes} stashed`} />
              </SimpleGrid>
              <div>
                <Title order={6}>Worktrees</Title>
                <Table>
                  <Table.Thead><Table.Tr><Table.Th>Folder</Table.Th><Table.Th>Branch</Table.Th><Table.Th>Changes</Table.Th><Table.Th>Unpushed</Table.Th><Table.Th>Run</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {(st.worktrees ?? []).map((w) => (
                      <Table.Tr key={w.path}>
                        <Table.Td><Text size="xs" className="mono">{w.path}</Text>{w.error && <Text size="xs" c="owlRed">{w.error}</Text>}</Table.Td>
                        <Table.Td><Text size="xs" className="mono">{w.branch || `detached ${short(w.head)}`}</Text></Table.Td>
                        <Table.Td><Text size="xs">{w.dirty} changed · {w.untracked} untracked</Text></Table.Td>
                        <Table.Td><Text size="xs" c={w.unpushed > 0 ? 'owlYellow' : undefined}>{w.unpushed}</Text></Table.Td>
                        <Table.Td>{w.run_id ? <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: w.run_id }} size="xs" className="mono">{w.run_id}</AnchorLink> : <Text size="xs" c="dimmed">—</Text>}</Table.Td>
                      </Table.Tr>
                    ))}
                    {(st.worktrees ?? []).length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size="xs" c="dimmed">No worktrees.</Text></Table.Td></Table.Tr>}
                  </Table.Tbody>
                </Table>
              </div>
              <div>
                <Title order={6}>Branches</Title>
                <Table>
                  <Table.Thead><Table.Tr><Table.Th>Branch</Table.Th><Table.Th>Commit</Table.Th><Table.Th>Upstream</Table.Th><Table.Th>Unpushed</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {(st.branches ?? []).map((b) => (
                      <Table.Tr key={b.name}>
                        <Table.Td><Text size="xs" className="mono">{b.name}</Text></Table.Td>
                        <Table.Td><Text size="xs" className="mono">{short(b.commit)}</Text></Table.Td>
                        <Table.Td><Text size="xs" className="mono">{b.no_upstream ? 'none' : b.merge?.replace('refs/heads/', `${b.remote ?? ''}/`) ?? ''}</Text></Table.Td>
                        <Table.Td><Text size="xs" c={b.unpushed > 0 ? 'owlYellow' : undefined}>{b.unpushed}</Text></Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </div>
            </>
          )}
          {canWrite && (
            <Group justify="flex-end">
              <Button variant="default" color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => void remove()}>Remove</Button>
              {repo.url && <Button variant="default" leftSection={<IconRefresh size={14} />} onClick={() => onSync([repo.name])}>Fetch</Button>}
              <Button onClick={() => onEdit(repo)}>Edit</Button>
            </Group>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm" className={mono ? 'mono' : undefined} style={{ wordBreak: 'break-all' }}>{value}</Text>
    </div>
  )
}

function RepositoryModal({ spaceId, repo, onClose }: { spaceId: string; repo: Repository | null; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: {
      name: repo?.name ?? '', path: repo?.path ?? '', url: repo?.url ?? '', default_branch: repo?.default_branch ?? 'main',
      role: (repo?.role ?? 'code') as NonNullable<RepositoryInput['role']>, clone_policy: (repo?.clone_policy ?? 'auto') as NonNullable<RepositoryInput['clone_policy']>,
      description: repo?.description ?? '',
    },
    onSubmit: async ({ value }) => {
      setError('')
      const body: RepositoryInput = { url: value.url.trim() || undefined, default_branch: value.default_branch.trim(), role: value.role,
        clone_policy: value.clone_policy, description: value.description }
      try {
        if (repo) {
          unwrap(await api.PUT('/spaces/{space_id}/repositories/{repository_id}', {
            params: { path: { space_id: spaceId, repository_id: repo.id }, header: { 'If-Match': ifMatch(repo.version) } }, body }))
        } else {
          unwrap(await api.POST('/spaces/{space_id}/repositories', { params: { path: { space_id: spaceId } },
            body: { ...body, name: value.name.trim(), path: value.path.trim() || undefined } }))
        }
        void qc.invalidateQueries({ queryKey: keys.repositories(spaceId) })
        notifyOk(repo ? 'Repository saved' : 'Repository added')
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={repo ? `Edit ${repo.name}` : 'Add repository'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <form.Field name="name" validators={{ onBlur: repo ? undefined : required('Name') }}>
              {(f) => <TextInput label="Name" description="Lowercase; fixed after creation" disabled={!!repo} value={f.state.value} onBlur={f.handleBlur}
                onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="path">
              {(f) => <TextInput label="Folder" description="In the space folder (default the name); fixed" placeholder="repos/api" className="mono" disabled={!!repo}
                value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="url">
            {(f) => <TextInput label="Clone URL" description="https://, ssh:// or git@host:org/repo.git, without credentials: the node's own Git authentication is used"
              placeholder="git@github.com:acme/api.git" className="mono" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="default_branch">
              {(f) => <TextInput label="Default branch" className="mono" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <form.Field name="clone_policy">
              {(f) => <Select label="Clone" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as NonNullable<RepositoryInput['clone_policy']>)}
                data={[{ value: 'auto', label: 'On the lease (auto)' }, { value: 'on_demand', label: 'When a run needs it' }, { value: 'manual', label: 'Never (manual)' }]} />}
            </form.Field>
            <form.Field name="role">
              {(f) => <Select label="Role" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as NonNullable<RepositoryInput['role']>)}
                data={[{ value: 'code', label: 'Code' }, { value: 'kb', label: 'Knowledge base' }]} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="description">
            {(f) => <Textarea label="Description" autosize minRows={1} maxRows={4} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button type="submit">{repo ? 'Save' : 'Add'}</Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
