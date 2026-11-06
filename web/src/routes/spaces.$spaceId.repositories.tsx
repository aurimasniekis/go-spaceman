import { Alert, Badge, Button, Card, Code, CopyButton, Drawer, Group, Modal, Select, SimpleGrid, Stack, Table, Text, Textarea, TextInput, Title, Tooltip } from '@mantine/core'
import { IconInfoCircle, IconKey, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type Repository, type RepositoryInput, type RepositoryStatus } from '@/api/client'
import { keys, useMe, useRepositories, useSpaceGitKeys } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
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
      <SectionHeader title="Repositories" description="Git repositories the node clones for this space; workers get worktrees of them"
        actions={canWrite && (
          <Group gap="xs">
            <Button variant="default" leftSection={<IconRefresh size={14} />} disabled={items.length === 0} onClick={() => void sync()}>Sync now</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add repository</Button>
          </Group>
        )} />
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
          <SSHKeySection spaceId={spaceId} repo={repo} canWrite={canWrite} />
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

/** SSH key (ADR-0057): SpaceMan keeps the private key; only the public key is shown, to add as a read-only deploy key. */
function SSHKeySection({ spaceId, repo, canWrite }: { spaceId: string; repo: Repository; canWrite: boolean }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const key = repo.ssh_key
  const shared = repo.git_key
  const call = async (method: 'POST' | 'DELETE', done: string) => {
    if (method === 'POST' && key && !window.confirm('Replace the SSH key? The current key stops being used at once: add the new public key to the Git host and remove the old one.')) return
    if (method === 'DELETE' && !window.confirm('Remove the SSH key? The node clones and fetches with its own Git authentication again.')) return
    setBusy(true)
    try {
      const opts = { params: { path: { space_id: spaceId, repository_id: repo.id }, header: { 'If-Match': ifMatch(repo.version) } } }
      unwrap(method === 'POST'
        ? await api.POST('/spaces/{space_id}/repositories/{repository_id}/ssh-key', opts)
        : await api.DELETE('/spaces/{space_id}/repositories/{repository_id}/ssh-key', opts))
      void qc.invalidateQueries({ queryKey: keys.repositories(spaceId) })
      notifyOk(done)
    } catch (e) {
      notifyError(e, 'Could not change the SSH key')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Card withBorder padding="sm">
      <Stack gap={6}>
        <Group justify="space-between">
          <Title order={6}>SSH key</Title>
          {canWrite && !shared && (
            <Group gap="xs">
              {key && <Button size="xs" variant="default" color="owlRed" loading={busy} onClick={() => void call('DELETE', 'SSH key removed')}>Remove</Button>}
              <Button size="xs" variant={key ? 'default' : 'filled'} leftSection={<IconKey size={14} />} loading={busy}
                onClick={() => void call('POST', key ? 'New SSH key generated: add it to the Git host' : 'SSH key generated: add it to the Git host')}>
                {key ? 'Rotate' : 'Generate SSH key'}
              </Button>
            </Group>
          )}
        </Group>
        {shared ? (
          <>
            <Text size="sm">Uses the shared key <b>{shared.name}</b> <Text span size="xs" c="dimmed" className="mono">{shared.fingerprint}</Text></Text>
            <Text size="xs" c="dimmed">An administrator manages it under Git SSH keys; its public key is on a Git account that can read this repository. Choose another key with Edit.</Text>
            {shared.note && <Alert color="owlYellow" icon={<IconInfoCircle size={16} />}>{shared.note}</Alert>}
          </>
        ) : key ? (
          <>
            <Text size="xs" c="dimmed">Add this public key to the Git host as a <b>read-only deploy key</b>. SpaceMan keeps the private key; the node uses it only to clone and fetch, and agents never receive it.</Text>
            <Group gap="xs" wrap="nowrap" align="flex-start">
              <Code block style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}>{key.public_key}</Code>
              <CopyButton value={key.public_key}>{({ copied, copy }) => <Button size="xs" variant="default" onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}</CopyButton>
            </Group>
            <Text size="xs" c="dimmed" className="mono">{key.fingerprint} · created {datetime(key.created_at)}</Text>
            <Text size="xs" c="dimmed">Host keys: {repo.known_hosts ? 'the repository\'s known_hosts' : 'built-in pins (github.com); set known_hosts for other hosts'}.</Text>
            {key.note && <Alert color="owlYellow" icon={<IconInfoCircle size={16} />}>{key.note}</Alert>}
          </>
        ) : (
          <Text size="xs" c="dimmed">None: the node clones and fetches with its own Git authentication. Generate a deploy key for this repository, or choose a shared key with Edit, so SpaceMan holds the credential instead (SSH URLs only).</Text>
        )}
      </Stack>
    </Card>
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
  const sharedKeys = useSpaceGitKeys(spaceId)
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: {
      name: repo?.name ?? '', path: repo?.path ?? '', url: repo?.url ?? '', default_branch: repo?.default_branch ?? 'main',
      role: (repo?.role ?? 'code') as NonNullable<RepositoryInput['role']>, clone_policy: (repo?.clone_policy ?? 'auto') as NonNullable<RepositoryInput['clone_policy']>,
      description: repo?.description ?? '', known_hosts: repo?.known_hosts ?? '', git_key_id: repo?.git_key?.id ?? '',
    },
    onSubmit: async ({ value }) => {
      setError('')
      const body: RepositoryInput = { url: value.url.trim() || undefined, default_branch: value.default_branch.trim(), role: value.role,
        clone_policy: value.clone_policy, description: value.description, known_hosts: value.known_hosts.trim() || undefined, git_key_id: value.git_key_id || undefined }
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
            {(f) => <TextInput label="Clone URL" description="https://, ssh:// or git@host:org/repo.git, without credentials. The node's own Git authentication is used unless the repository has an SSH key"
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
          {(sharedKeys.data?.length ?? 0) > 0 && !repo?.ssh_key && (
            <form.Field name="git_key_id">
              {(f) => <Select label="Shared Git SSH key" description="Clone with a key an administrator shared with this space (SSH URLs only)" allowDeselect={false}
                value={f.state.value} onChange={(v) => f.handleChange(v ?? '')}
                data={[{ value: '', label: "None (the repository's own key, or the node's Git)" }, ...(sharedKeys.data ?? []).map((k) => ({ value: k.id, label: `${k.name} · ${k.fingerprint.slice(0, 18)}…` }))]} />}
            </form.Field>
          )}
          <form.Field name="known_hosts">
            {(f) => <Textarea label="Known hosts" description="Pins the Git host's SSH keys for the repository's SSH key (known_hosts lines). Empty: the built-in pins (github.com)."
              className="mono" autosize minRows={1} maxRows={6} placeholder="gitlab.example.com ssh-ed25519 AAAA…" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
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
