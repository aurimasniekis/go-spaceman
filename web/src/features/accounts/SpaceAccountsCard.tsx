import { Badge, Button, Card, Checkbox, Group, Modal, Radio, Stack, Table, Text } from '@mantine/core'
import { IconSettings } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type Account, type AccountBinding } from '@/api/client'
import { keys, useAccounts, useMe, useSpaceAccounts } from '@/api/queries'
import { hasGlobal } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'

const harnessLabel = (h?: string) => (h === 'codex' ? 'Codex' : 'Claude Code')

/** The accounts a space's runs may use, and (for administrators) choosing them and the default per harness. */
export function SpaceAccountsCard({ spaceId }: { spaceId: string }) {
  const bound = useSpaceAccounts(spaceId)
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const [editing, setEditing] = useState(false)
  const items = bound.data ?? []
  return (
    <Card mb="md">
      <Group justify="space-between" mb={items.length ? 'xs' : 0}>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Accounts</Text>
        {admin && <Button size="xs" variant="default" leftSection={<IconSettings size={14} />} onClick={() => setEditing(true)}>Choose accounts</Button>}
      </Group>
      {items.length === 0 ? (
        <Text size="sm" c="dimmed">No account is bound, so runs cannot start. {admin ? 'Choose the accounts this space may use.' : 'Ask an administrator to bind one.'}</Text>
      ) : (
        <Group gap="xs">
          {items.map((b) => (
            <Badge key={b.account_id} variant={b.default ? 'filled' : 'light'} color={b.enabled ? 'owlBlue' : 'gray'}>
              {b.name} · {harnessLabel(b.harness)}{b.default ? ' · default' : ''}{b.enabled ? '' : ' · disabled'}
            </Badge>
          ))}
        </Group>
      )}
      {editing && <BindModal spaceId={spaceId} current={items} onClose={() => setEditing(false)} />}
    </Card>
  )
}

function BindModal({ spaceId, current, onClose }: { spaceId: string; current: AccountBinding[]; onClose: () => void }) {
  const qc = useQueryClient()
  const all = useAccounts().data ?? []
  const [picked, setPicked] = useState<string[]>(current.map((b) => b.account_id))
  const [defaults, setDefaults] = useState<Record<string, string>>(
    Object.fromEntries(current.filter((b) => b.default).map((b) => [b.harness ?? '', b.account_id])),
  )
  const byHarness = (h: string) => all.filter((a) => a.harness === h)
  const toggle = (a: Account, on: boolean) => {
    setPicked(on ? [...picked, a.id] : picked.filter((x) => x !== a.id))
    if (!on && defaults[a.harness] === a.id) setDefaults({ ...defaults, [a.harness]: '' })
  }
  const save = async () => {
    try {
      unwrap(await api.PUT('/spaces/{space_id}/accounts', {
        params: { path: { space_id: spaceId } },
        body: { items: picked.map((id) => ({ account_id: id, default: Object.values(defaults).includes(id) })) },
      }))
      void qc.invalidateQueries({ queryKey: keys.spaceAccounts(spaceId) })
      void qc.invalidateQueries({ queryKey: keys.accounts })
      notifyOk('Accounts saved')
      onClose()
    } catch (e) {
      notifyError(e, 'Could not save the accounts')
    }
  }
  return (
    <Modal opened onClose={onClose} title="Accounts this space may use" size="lg">
      <Stack gap="md">
        <Text size="sm" c="dimmed">A launch uses the account it names, otherwise the default for its harness, otherwise the only bound one.</Text>
        {(['claude-code', 'codex'] as const).map((h) => (
          <Stack key={h} gap={4}>
            <Text fw={600} size="sm">{harnessLabel(h)}</Text>
            {byHarness(h).length === 0 ? <Text size="sm" c="dimmed">No {harnessLabel(h)} accounts yet.</Text> : (
              <Radio.Group value={defaults[h] ?? ''} onChange={(v) => setDefaults({ ...defaults, [h]: v })}>
                <Table>
                  <Table.Thead><Table.Tr><Table.Th>Allowed</Table.Th><Table.Th>Account</Table.Th><Table.Th>Default</Table.Th></Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {byHarness(h).map((a) => (
                      <Table.Tr key={a.id}>
                        <Table.Td><Checkbox aria-label={`Allow ${a.name}`} checked={picked.includes(a.id)} onChange={(e) => toggle(a, e.currentTarget.checked)} /></Table.Td>
                        <Table.Td><Text size="sm">{a.name}</Text>{!a.enabled && <Badge size="xs" color="gray">disabled</Badge>}</Table.Td>
                        <Table.Td><Radio aria-label={`Default ${a.name}`} value={a.id} disabled={!picked.includes(a.id)} /></Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Radio.Group>
            )}
          </Stack>
        ))}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
      </Stack>
    </Modal>
  )
}
