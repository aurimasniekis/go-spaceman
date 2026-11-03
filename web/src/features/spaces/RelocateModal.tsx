import { ActionIcon, Alert, Button, Checkbox, Group, Modal, NumberInput, Radio, Select, Stack, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus, IconTrash } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type Node, type Space } from '@/api/client'
import { keys, useNodes, useRelocation } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { RelocationReport } from './RelocationCard'

type Repo = { path: string; url: string; branch: string }

/**
 * Relocation dialog (SM-INV-003, SM-SPACE-005…007): an explicit request to move a stopped space to another node. A
 * precheck inspects the source first and shows what a clean relocation would lose; unpushed, changed or untracked state is
 * refused unless the operator chooses to export it. When the source is offline, disabled or revoked, the dialog switches to
 * the unavailable-source flow, which needs an acknowledgement that local state may be lost.
 */
export function RelocateModal({ space, opened, onClose }: { space: Space; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const nodes = useNodes()
  const relocation = useRelocation(space.id)
  const source = nodes.data?.find((n) => n.id === space.node_id)
  const sourceDown = !!source && (!source.connected || source.state !== 'active')
  const [dest, setDest] = useState<string | null>(null)
  const [dirty, setDirty] = useState<'refuse' | 'export'>('refuse')
  const [stopRuns, setStopRuns] = useState(space.state === 'active')
  const [recovery, setRecovery] = useState<'retain' | 'delete'>('retain')
  const [retainDays, setRetainDays] = useState<number>(14)
  const [ack, setAck] = useState(false)
  const [ackText, setAckText] = useState('')
  const [repos, setRepos] = useState<Repo[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [key] = useState(idempotencyKey)

  // Only active nodes can be destinations (central refuses the others); the rest are named with the reason.
  const others = (nodes.data ?? []).filter((n: Node) => n.id !== space.node_id)
  const candidates = others.filter((n: Node) => n.state === 'active')
  const why = (n: Node) => (n.state === 'pending' ? 'never connected' : n.state)
  const unavailable = others.filter((n: Node) => n.state !== 'active').map((n: Node) => `${n.name} (${why(n)})`)
  const precheck = relocation.data?.mode === 'precheck' && relocation.data.destination_node_id === dest ? relocation.data : null

  const start = async (dry: boolean) => {
    if (!dest) return
    setError('')
    setBusy(true)
    try {
      const body = dry
        ? { destination_node_id: dest, precheck: true }
        : sourceDown
          ? {
              destination_node_id: dest,
              stop_runs: stopRuns,
              recovery,
              retain_days: recovery === 'retain' ? retainDays : 0,
              source_unavailable: true,
              acknowledge_loss: ack,
              acknowledgement: ackText,
              repositories: repos.filter((r) => r.path && r.url).map((r) => ({ path: r.path, url: r.url, branch: r.branch || undefined })),
            }
          : { destination_node_id: dest, dirty_state: dirty, stop_runs: stopRuns, recovery, retain_days: recovery === 'retain' ? retainDays : 0 }
      const r = unwrap(
        await api.POST('/spaces/{space_id}/relocation', {
          params: { path: { space_id: space.id }, header: { 'Idempotency-Key': dry ? idempotencyKey() : key } },
          body,
        }),
      )
      qc.setQueryData(keys.relocation(space.id), r)
      void qc.invalidateQueries({ queryKey: keys.space(space.id) })
      if (!dry) onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal opened={opened} onClose={onClose} title={`Relocate ${space.name}`} size="xl">
      <Stack gap="sm">
        <Text size="sm">
          The space moves from <b>{source?.name ?? space.node_id}</b> to another node only on this explicit request. It stays paused on the
          destination; resume it there when you are ready.
        </Text>
        <Select
          label="Destination node"
          placeholder="Choose a node"
          description={unavailable.length > 0 ? `Not available as a destination: ${unavailable.join(', ')}` : undefined}
          data={candidates.map((n) => ({ value: n.id, label: `${n.name}${n.connected ? '' : ' (offline)'}` }))}
          value={dest}
          onChange={setDest}
          required
        />
        {sourceDown ? (
          <Alert color="owlRed" title="The source node is unavailable">
            <Stack gap="xs">
              <Text size="sm">
                {source?.name} is {source?.state !== 'active' ? source?.state : 'offline'}. The destination will be prepared only from central's copies: role
                memory as central acknowledged it and the repositories you list below. Unpushed or uncommitted work, files outside repositories, unsynced
                memory and native sessions on the source may be lost. The source's runs are marked lost and fenced if it ever returns.
              </Text>
              {repos.map((r, i) => (
                <Group key={i} gap="xs" align="flex-end" wrap="nowrap">
                  <TextInput label="Folder" placeholder="app" value={r.path} onChange={(e) => setRepos(repos.map((x, j) => (j === i ? { ...x, path: e.currentTarget.value } : x)))} />
                  <TextInput label="Clone URL" placeholder="git@example.com:team/app.git" style={{ flex: 1 }} value={r.url}
                    onChange={(e) => setRepos(repos.map((x, j) => (j === i ? { ...x, url: e.currentTarget.value } : x)))} />
                  <TextInput label="Branch" placeholder="main" value={r.branch} onChange={(e) => setRepos(repos.map((x, j) => (j === i ? { ...x, branch: e.currentTarget.value } : x)))} />
                  <ActionIcon variant="subtle" color="owlRed" aria-label={`Remove repository ${i + 1}`} onClick={() => setRepos(repos.filter((_, j) => j !== i))}>
                    <IconTrash size={14} />
                  </ActionIcon>
                </Group>
              ))}
              <Group>
                <Button size="xs" variant="default" leftSection={<IconPlus size={12} />} onClick={() => setRepos([...repos, { path: '', url: '', branch: '' }])}>
                  Add a repository to clone
                </Button>
              </Group>
              <Checkbox checked={ack} onChange={(e) => setAck(e.currentTarget.checked)} label="I understand that local state on the source node may be lost" />
              <Textarea label="Acknowledgement (recorded in the audit log)" placeholder="Why the source is unavailable and what you accept losing" minRows={2}
                value={ackText} onChange={(e) => setAckText(e.currentTarget.value)} />
            </Stack>
          </Alert>
        ) : (
          <>
            <Group align="flex-end">
              <Button variant="default" disabled={!dest || busy} onClick={() => void start(true)}>Run precheck</Button>
              <Text size="xs" c="dimmed">Inspects the source (memory synced, uploads finished, repositories pushed). Nothing changes.</Text>
            </Group>
            {precheck && <RelocationReport r={precheck} />}
            <Radio.Group label="Unpushed, changed or untracked state" value={dirty} onChange={(v) => setDirty(v as 'refuse' | 'export')}>
              <Stack gap={4} mt={4}>
                <Radio value="refuse" label="Refuse the relocation (default): push or clean up first" />
                <Radio value="export" label="Export it explicitly: bundle branches, changes, stashes and loose files through central to the destination" />
              </Stack>
            </Radio.Group>
          </>
        )}
        <Checkbox checked={stopRuns} onChange={(e) => setStopRuns(e.currentTarget.checked)}
          label="Pause the space and stop its runs" description="Without this, the space must already be paused with no active runs." />
        <Group align="flex-end">
          <Select label="Source folder afterwards" value={recovery} onChange={(v) => setRecovery((v as 'retain' | 'delete') ?? 'retain')} allowDeselect={false}
            data={[{ value: 'retain', label: 'Retain it on the source' }, { value: 'delete', label: 'Delete it' }]} />
          {recovery === 'retain' && (
            <NumberInput label="Keep for (days, 0 until released)" min={0} max={3650} value={retainDays} onChange={(v) => setRetainDays(Number(v) || 0)} w={220} />
          )}
        </Group>
        <Text size="xs" c="dimmed">
          Continuity: repositories, role memory, the inbox, queue, human actions and messages move with the space. Native Claude Code and Codex sessions are
          node-local and are not carried: runs on the destination start a fresh session (project continuity only).
        </Text>
        {error && <Alert color="owlRed">{error}</Alert>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Close</Button>
          <Button color={sourceDown ? 'owlRed' : undefined} disabled={!dest || busy || (sourceDown && (!ack || ackText.trim().length < 10))} onClick={() => void start(false)}>
            {sourceDown ? 'Relocate without the source' : 'Relocate'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
