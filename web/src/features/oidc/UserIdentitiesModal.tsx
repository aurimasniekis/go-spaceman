import { Button, Group, Modal, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { useForm, useSelector } from '@tanstack/react-form'
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
  const form = useForm({ defaultValues: { provider: '', subject: '' } })
  const provider = useSelector(form.store, (st) => st.values.provider)
  const subjectOk = useSelector(form.store, (st) => st.values.subject.trim() !== '')
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
    check(await api.POST('/users/{user_id}/identities', { params: { path: { user_id: user.id } }, body: { provider_id: provider, subject: form.state.values.subject.trim() } }))
    form.setFieldValue('subject', '')
    refresh()
  })
  const issue = () => run(async () => {
    const r = unwrap(await api.POST('/users/{user_id}/link-invite', { params: { path: { user_id: user.id } }, body: { provider_id: provider } }))
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
        <form onSubmit={(e) => { e.preventDefault(); if (provider && subjectOk) void link() }}>
          <Stack gap="sm">
            <form.Field name="provider">
              {(f) => <Select label="Provider" allowDeselect={false} data={providers.map((p) => ({ value: p.id, label: p.display_name }))} value={f.state.value || null} onChange={(x) => f.handleChange(x ?? '')} />}
            </form.Field>
            <Group gap="xs" align="flex-end" wrap="nowrap">
              <form.Field name="subject">
                {(f) => <TextInput label="Subject" description="The provider's stable sub claim (Porch: the u_… user ID), never an email" flex={1} className="mono" value={f.state.value}
                  onChange={(e) => f.handleChange(e.target.value)} />}
              </form.Field>
              <Button type="submit" variant="default" disabled={!provider || !subjectOk}>Link</Button>
            </Group>
          </Stack>
        </form>
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
