import { Button, Group, Modal, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, check, unwrap, type OidcIdentity, type User } from '@/api/client'
import { keys, useOidcProviders, useUserIdentities } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { SecretModal } from '@/components/SecretModal'
import { ago, isSet } from '@/lib/format'

/**
 * An administrator's view of a user's sign-in identities (Hallpass HP-OIDC-009): link an expected identity by its
 * subject, issue a single-use link invitation, or unlink (which ends the user's sessions).
 */
export function UserIdentitiesModal({ user, onClose }: { user: User; onClose: () => void }) {
  const qc = useQueryClient()
  const identities = useUserIdentities(user.id)
  const providers = useOidcProviders().data?.items ?? []
  const [provider, setProvider] = useState<string | null>(null)
  const [subject, setSubject] = useState('')
  const [error, setError] = useState('')
  const [invite, setInvite] = useState<string | null>(null)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.userIdentities(user.id) })
    void qc.invalidateQueries({ queryKey: keys.users })
  }
  const run = async (fn: () => Promise<void>) => {
    setError('')
    try {
      await fn()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  const link = () => run(async () => {
    check(await api.POST('/users/{user_id}/identities', { params: { path: { user_id: user.id } }, body: { provider_id: provider ?? '', subject: subject.trim() } }))
    setSubject('')
    refresh()
  })
  const issue = () => run(async () => {
    const r = unwrap(await api.POST('/users/{user_id}/link-invite', { params: { path: { user_id: user.id } }, body: { provider_id: provider ?? '' } }))
    setInvite(`${window.location.origin}${r.url}`)
  })
  const unlink = (i: OidcIdentity) => run(async () => {
    check(await api.DELETE('/users/{user_id}/identities', { params: { path: { user_id: user.id }, query: { issuer: i.issuer, subject: i.subject } } }))
    refresh()
  })
  return (
    <Modal opened onClose={onClose} title={`Sign-in identities of ${user.display_name}`} size="lg">
      <Stack gap="sm">
        <ErrorAlert error={identities.error} />
        <Table>
          <Table.Thead><Table.Tr><Table.Th>Provider</Table.Th><Table.Th>Subject</Table.Th><Table.Th>Last sign-in</Table.Th><Table.Th w={80} /></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(identities.data ?? []).map((i) => (
              <Table.Tr key={`${i.issuer}|${i.subject}`}>
                <Table.Td>{i.provider_name}</Table.Td>
                <Table.Td><Text size="xs" className="mono">{i.subject}</Text></Table.Td>
                <Table.Td><Text size="xs">{isSet(i.last_login_at) ? ago(i.last_login_at) : 'never'}</Text></Table.Td>
                <Table.Td><Button size="compact-xs" variant="light" color="owlRed" onClick={() => void unlink(i)}>Unlink</Button></Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {identities.isSuccess && identities.data.length === 0 && <Text size="sm" c="dimmed">No linked identity.</Text>}
        <Select label="Provider" allowDeselect={false} data={providers.map((p) => ({ value: p.id, label: p.display_name }))} value={provider} onChange={setProvider} />
        <Group gap="xs" align="flex-end" wrap="nowrap">
          <TextInput label="Subject" description="The provider's stable sub claim (Porch: the u_… user ID), never an email" flex={1} className="mono" value={subject} onChange={(e) => setSubject(e.target.value)} />
          <Button variant="default" disabled={!provider || !subject.trim()} onClick={() => void link()}>Link</Button>
        </Group>
        <Group justify="space-between">
          <Button variant="subtle" disabled={!provider} onClick={() => void issue()}>Issue a link invitation instead</Button>
          <Button onClick={onClose}>Done</Button>
        </Group>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
      </Stack>
      <SecretModal title="SSO link invitation" secret={invite} onClose={() => setInvite(null)}>
        <Text size="sm">Send it to {user.display_name} over a trusted channel. Opening it signs them in at the provider and links that identity to this user. It works once, within 72 hours.</Text>
      </SecretModal>
    </Modal>
  )
}
