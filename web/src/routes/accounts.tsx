import { ActionIcon, Badge, Button, Card, Group, Menu, Modal, Stack, Table, Text } from '@mantine/core'
import { IconDots, IconPlus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, type Account } from '@/api/client'
import { keys, useAccountKinds, useAccounts } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { AccountModal } from '@/features/accounts/AccountModal'
import { ago } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/accounts')({ component: AccountsPage })

/** Harness accounts (SM-ACCT-001): credentials are write-only; this page never shows them. */
function AccountsPage() {
  const qc = useQueryClient()
  const accounts = useAccounts()
  const kinds = useAccountKinds().data ?? []
  const [editing, setEditing] = useState<Account | null>(null)
  const [adding, setAdding] = useState(false)
  const label = (k: string) => kinds.find((x) => x.name === k)?.label ?? k
  const [deleting, setDeleting] = useState<Account | null>(null)
  const remove = async (a: Account) => {
    try {
      check(await api.DELETE('/accounts/{account_id}', { params: { path: { account_id: a.id } } }))
      void qc.invalidateQueries({ queryKey: keys.accounts })
      notifyOk('Account deleted')
      setDeleting(null)
    } catch (e) {
      notifyError(e, 'Could not delete the account')
    }
  }
  return (
    <>
      <PageHeader title="Accounts" description="Credentials Claude Code and Codex runs use. They are sealed when saved and never shown again."
        actions={<Button leftSection={<IconPlus size={14} />} onClick={() => setAdding(true)}>Add account</Button>} />
      <ErrorAlert error={accounts.error} />
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr><Table.Th>Name</Table.Th><Table.Th>Credential</Table.Th><Table.Th>Harness</Table.Th><Table.Th>Spaces</Table.Th><Table.Th>Credential set</Table.Th><Table.Th /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(accounts.data ?? []).map((a) => (
              <Table.Tr key={a.id}>
                <Table.Td>
                  <Group gap="xs"><Text size="sm" fw={500}>{a.name}</Text>{!a.enabled && <Badge size="xs" color="gray">disabled</Badge>}</Group>
                  {a.description && <Text size="xs" c="dimmed">{a.description}</Text>}
                </Table.Td>
                <Table.Td><Text size="sm">{label(a.kind)}</Text></Table.Td>
                <Table.Td><Badge variant="light">{a.harness === 'codex' ? 'Codex' : 'Claude Code'}</Badge></Table.Td>
                <Table.Td>{a.spaces}</Table.Td>
                <Table.Td><Text size="sm">{ago(a.secret_rotated_at)}</Text><Text size="xs" c="dimmed">version {a.secret_version}</Text></Table.Td>
                <Table.Td>
                  <Menu position="bottom-end">
                    <Menu.Target><ActionIcon variant="subtle" aria-label={`Actions for ${a.name}`}><IconDots size={16} /></ActionIcon></Menu.Target>
                    <Menu.Dropdown>
                      <Menu.Item onClick={() => setEditing(a)}>Edit or rotate…</Menu.Item>
                      <Menu.Item color="owlRed" onClick={() => setDeleting(a)}>Delete…</Menu.Item>
                    </Menu.Dropdown>
                  </Menu>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {accounts.isSuccess && accounts.data.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">No accounts yet. Add a Claude subscription token (<code>claude setup-token</code>) or an API key, then bind it to spaces.</Text>
        )}
      </Card>
      {adding && <AccountModal opened onClose={() => setAdding(false)} />}
      {editing && <AccountModal account={editing} opened onClose={() => setEditing(null)} />}
      <Modal opened={deleting !== null} onClose={() => setDeleting(null)} title={`Delete ${deleting?.name ?? ''}?`}>
        <Stack gap="sm">
          <Text size="sm">Its bindings are removed. Runs already started keep their credential; new launches in {deleting?.spaces ?? 0} space(s) need another account.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button color="owlRed" onClick={() => deleting && void remove(deleting)}>Delete</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}
