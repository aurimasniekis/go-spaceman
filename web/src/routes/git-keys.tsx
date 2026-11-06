import { Alert, Badge, Button, Card, Code, CopyButton, Drawer, Group, Menu, Modal, MultiSelect, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { IconDots, IconInfoCircle, IconKey, IconPlus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type GitKey } from '@/api/client'
import { keys, useGitKeys, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/git-keys')({ component: GitKeysPage })

/**
 * Shared Git SSH keys (ADR-0058): one key, added to a Git account, that several spaces' repositories clone with. Only the
 * public half is ever shown; agents never receive the key.
 */
function GitKeysPage() {
  const list = useGitKeys()
  const spaces = useSpaces().data ?? []
  const [open, setOpen] = useState<string | null>(null)
  const [editing, setEditing] = useState<GitKey | 'new' | null>(null)
  const spaceName = (id: string) => spaces.find((s) => s.id === id)?.name ?? id
  const current = (list.data ?? []).find((k) => k.id === open) ?? null
  return (
    <>
      <PageHeader title="Git SSH keys" description="Keys SpaceMan clones private repositories with. Add a key's public half to a Git account; spaces you allow can use it."
        actions={<Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Generate key</Button>} />
      <Alert color="owlBlue" icon={<IconInfoCircle size={16} />} mb="md" p="xs">
        <Text size="sm">
          A key on a person&apos;s Git account can also push to every repository that person can. Prefer a read-only machine user, or a
          repository&apos;s own deploy key (its SSH key card). SpaceMan only clones and fetches, and agents never receive the key.
        </Text>
      </Alert>
      <ErrorAlert error={list.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr><Table.Th>Name</Table.Th><Table.Th>Fingerprint</Table.Th><Table.Th>Spaces</Table.Th><Table.Th>Used by</Table.Th><Table.Th>Updated</Table.Th></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(list.data ?? []).map((k) => (
              <Table.Tr key={k.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(k.id)} tabIndex={0} onKeyDown={(e) => { if (e.key === 'Enter') setOpen(k.id) }}>
                <Table.Td><Text size="sm" fw={500} c="var(--mantine-primary-color-filled)">{k.name}</Text>{k.description && <Text size="xs" c="dimmed">{k.description}</Text>}</Table.Td>
                <Table.Td><Text size="xs" className="mono">{k.fingerprint}</Text></Table.Td>
                <Table.Td><Text size="sm">{k.all_spaces ? 'All spaces' : k.spaces.length === 0 ? 'None yet' : k.spaces.map(spaceName).join(', ')}</Text></Table.Td>
                <Table.Td><Text size="sm">{k.used_by.length === 0 ? '—' : `${k.used_by.length} ${k.used_by.length === 1 ? 'repository' : 'repositories'}`}</Text></Table.Td>
                <Table.Td><Text size="sm" c="dimmed">{ago(k.updated_at)}</Text></Table.Td>
              </Table.Tr>
            ))}
            {list.isSuccess && list.data.length === 0 && (
              <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed" ta="center" p="md">No shared keys. Generate one to clone private repositories from several spaces with one Git account.</Text></Table.Td></Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
      <Drawer opened={!!current} onClose={() => setOpen(null)} position="right" size="lg" title={current?.name ?? 'Key'}>
        {current && <KeyDetail k={current} spaceName={spaceName} onEdit={() => setEditing(current)} onClose={() => setOpen(null)} />}
      </Drawer>
      {editing && <KeyModal k={editing === 'new' ? null : editing} onClose={() => setEditing(null)} onCreated={(id) => setOpen(id)} />}
    </>
  )
}

function KeyDetail({ k, spaceName, onEdit, onClose }: { k: GitKey; spaceName: (id: string) => string; onEdit: () => void; onClose: () => void }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const rotate = async () => {
    if (!window.confirm(`Replace ${k.name}? The current key stops working at once: add the new public key where the old one was, and remove the old one.`)) return
    setBusy(true)
    try {
      unwrap(await api.POST('/git-ssh-keys/{key_id}/rotate', { params: { path: { key_id: k.id }, header: { 'If-Match': ifMatch(k.version) } } }))
      void qc.invalidateQueries({ queryKey: keys.gitKeys })
      notifyOk('Key replaced: add the new public key to the Git account')
    } catch (e) {
      notifyError(e, 'Could not replace the key')
    } finally {
      setBusy(false)
    }
  }
  const remove = async () => {
    if (!window.confirm(`Delete ${k.name}? Remove its public key from the Git account too.`)) return
    try {
      check(await api.DELETE('/git-ssh-keys/{key_id}', { params: { path: { key_id: k.id } } }))
      void qc.invalidateQueries({ queryKey: keys.gitKeys })
      notifyOk('Key deleted')
      onClose()
    } catch (e) {
      notifyError(e, 'Could not delete the key')
    }
  }
  return (
    <Stack gap="sm">
      {k.description && <Text size="sm">{k.description}</Text>}
      <Text size="sm" fw={600}>Public key</Text>
      <Group gap="xs" wrap="nowrap" align="flex-start">
        <Code block style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}>{k.public_key}</Code>
        <CopyButton value={k.public_key}>{({ copied, copy }) => <Button size="xs" variant="default" onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}</CopyButton>
      </Group>
      <Text size="xs" c="dimmed">
        Add it to the Git account (GitHub: the account&apos;s Settings → SSH and GPG keys) or as a repository&apos;s deploy key.{' '}
        <span className="mono">{k.fingerprint}</span>
        {k.rotated_at ? ` · replaced ${datetime(k.rotated_at)}` : ` · created ${datetime(k.created_at)}`}
      </Text>
      <Text size="sm" fw={600}>Spaces that may use it</Text>
      <Group gap={4}>{k.all_spaces ? <Badge variant="light">All spaces</Badge> : k.spaces.length === 0 ? <Text size="sm" c="dimmed">None yet</Text>
        : k.spaces.map((s) => <Badge key={s} variant="light" color="gray">{spaceName(s)}</Badge>)}</Group>
      <Text size="sm" fw={600}>Repositories that clone with it</Text>
      {k.used_by.length === 0 ? <Text size="sm" c="dimmed">None. Choose the key on a space&apos;s repository (its SSH key card).</Text> : (
        <Stack gap={2}>
          {k.used_by.map((r) => (
            <AnchorLink key={r.repository_id} to="/spaces/$spaceId/repositories" params={{ spaceId: r.space_id }} size="sm">{spaceName(r.space_id)} / {r.name}</AnchorLink>
          ))}
        </Stack>
      )}
      <Group justify="flex-end" mt="sm">
        <Menu position="bottom-end">
          <Menu.Target><Button variant="default" px="xs" aria-label={`More actions for ${k.name}`}><IconDots size={16} /></Button></Menu.Target>
          <Menu.Dropdown>
            <Menu.Item leftSection={<IconKey size={14} />} onClick={() => void rotate()} disabled={busy}>Replace key…</Menu.Item>
            <Menu.Item color="owlRed" onClick={() => void remove()} disabled={k.used_by.length > 0}>Delete…</Menu.Item>
          </Menu.Dropdown>
        </Menu>
        <Button onClick={onEdit}>Edit</Button>
      </Group>
    </Stack>
  )
}

function KeyModal({ k, onClose, onCreated }: { k: GitKey | null; onClose: () => void; onCreated: (id: string) => void }) {
  const qc = useQueryClient()
  const spaces = useSpaces().data ?? []
  const [name, setName] = useState(k?.name ?? '')
  const [description, setDescription] = useState(k?.description ?? '')
  const [all, setAll] = useState(k?.all_spaces ?? false)
  const [chosen, setChosen] = useState<string[]>(k?.spaces ?? [])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setError('')
    setBusy(true)
    const body = { name: name.trim(), description: description.trim() || undefined, all_spaces: all, spaces: all ? [] : chosen }
    try {
      if (k) {
        unwrap(await api.PUT('/git-ssh-keys/{key_id}', { params: { path: { key_id: k.id }, header: { 'If-Match': ifMatch(k.version) } }, body }))
        notifyOk('Key saved')
      } else {
        const made = unwrap(await api.POST('/git-ssh-keys', { body }))
        notifyOk('Key generated: add its public key to the Git account')
        onCreated(made.id)
      }
      void qc.invalidateQueries({ queryKey: keys.gitKeys })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened onClose={onClose} title={k ? `Edit ${k.name}` : 'Generate a Git SSH key'}>
      <Stack gap="sm">
        <TextInput label="Name" description="Lowercase; for example github-bot" value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus />
        <TextInput label="Description" placeholder="Read-only machine user for the payments repositories" value={description} onChange={(e) => setDescription(e.currentTarget.value)} />
        <Switch label="Every space may use it" checked={all} onChange={(e) => setAll(e.currentTarget.checked)} />
        {!all && (
          <MultiSelect label="Spaces that may use it" searchable value={chosen} onChange={setChosen}
            data={spaces.filter((s) => s.state !== 'archived').map((s) => ({ value: s.id, label: s.name }))} placeholder="Choose spaces" />
        )}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>{k ? 'Save' : 'Generate key'}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
