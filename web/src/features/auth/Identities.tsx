import { Alert, Badge, Button, Card, Group, Modal, Stack, Table, Text, Title } from '@mantine/core'
import { IconLink } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, check, type OidcIdentity } from '@/api/client'
import { keys, useMe, useMyIdentities, useSignInProviders } from '@/api/queries'
import { requestReauth } from '@/api/session'
import { ErrorAlert } from '@/components/ErrorAlert'
import { ago, datetime, isSet } from '@/lib/format'
import { notifyError } from '@/lib/notify'
import { ssoErrorMessage, ssoStartUrl } from '@/lib/sso'

const VIA: Record<OidcIdentity['linked_via'], string> = { admin: 'linked by an administrator', self_link: 'linked by you', jit: 'created at first sign-in' }

/**
 * The account's sign-in identities (SM-AUTH-001): identities at OIDC providers linked to this user, linking another
 * provider (a fresh sign-in at the provider while signed in here, Hallpass HP-OIDC-009) and unlinking.
 */
export function Identities({ ssoError }: { ssoError?: string }) {
  const me = useMe().data
  const identities = useMyIdentities()
  const providers = useSignInProviders().data ?? []
  const qc = useQueryClient()
  const [unlinking, setUnlinking] = useState<OidcIdentity | null>(null)
  const [needsReauth, setNeedsReauth] = useState(ssoError === 'reauth_required')
  const linked = new Set((identities.data ?? []).map((i) => i.provider_slug))
  const available = providers.filter((p) => !linked.has(p.slug))

  const link = (slug: string) => window.location.assign(ssoStartUrl(slug, 'link', '/account'))
  const confirmAndLink = async (slug: string) => {
    if (await requestReauth()) link(slug)
  }
  const unlink = async (i: OidcIdentity) => {
    try {
      check(await api.DELETE('/auth/identities', { params: { query: { issuer: i.issuer, subject: i.subject } } }))
      // Unlinking ends every session of the user, this one included.
      void qc.invalidateQueries({ queryKey: keys.me })
      window.location.assign('/login')
    } catch (e) {
      notifyError(e, 'Could not unlink')
    } finally {
      setUnlinking(null)
    }
  }

  return (
    <div>
      <Title order={5} mb="xs">Sign-in identities</Title>
      <Stack gap="xs" maw={820}>
        {ssoError && !needsReauth && <Alert color="owlRed" title="Single sign-on failed" role="alert">{ssoErrorMessage(ssoError)}</Alert>}
        {needsReauth && (
          <Alert color="owlYellow" title="Confirm it's you first">
            <Stack gap="xs" align="flex-start">
              <Text size="sm">{ssoErrorMessage('reauth_required')}</Text>
              <Group gap="xs">
                {available.map((p) => (
                  <Button key={p.slug} size="xs" variant="default" onClick={() => void confirmAndLink(p.slug).then(() => setNeedsReauth(false))}>Confirm and link {p.display_name}</Button>
                ))}
              </Group>
            </Stack>
          </Alert>
        )}
        <ErrorAlert error={identities.error} />
        <Card p={0}>
          <Table>
            <Table.Thead>
              <Table.Tr><Table.Th>Provider</Table.Th><Table.Th>Subject</Table.Th><Table.Th>Linked</Table.Th><Table.Th>Last sign-in</Table.Th><Table.Th w={90} /></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {(identities.data ?? []).map((i) => (
                <Table.Tr key={`${i.issuer}|${i.subject}`}>
                  <Table.Td>
                    <Group gap="xs"><Text size="sm" fw={500}>{i.provider_name}</Text>{me?.method === 'oidc' && me.provider === i.provider_slug && <Badge color="owlGreen">this session</Badge>}</Group>
                  </Table.Td>
                  <Table.Td><Text size="xs" className="mono">{i.subject}</Text></Table.Td>
                  <Table.Td><Text size="xs">{datetime(i.created_at)}</Text><Text size="xs" c="dimmed">{VIA[i.linked_via]}</Text></Table.Td>
                  <Table.Td><Text size="xs">{isSet(i.last_login_at) ? ago(i.last_login_at) : 'never'}</Text></Table.Td>
                  <Table.Td><Button size="compact-xs" variant="light" color="owlRed" onClick={() => setUnlinking(i)}>Unlink</Button></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {identities.isSuccess && identities.data.length === 0 && (
            <Text size="sm" c="dimmed" ta="center" p="md">No identity provider is linked. {available.length > 0 ? 'Link one to sign in with it.' : ''}</Text>
          )}
        </Card>
        {available.length > 0 && (
          <Group gap="xs">
            {available.map((p) => (
              <Button key={p.slug} variant="default" leftSection={<IconLink size={14} />} onClick={() => link(p.slug)}>Link {p.display_name}</Button>
            ))}
          </Group>
        )}
        <Text size="xs" c="dimmed">
          Linking signs you in at the provider again and binds that identity to this account. Grants from the provider's groups apply only when you sign in
          with it.
        </Text>
      </Stack>
      <Modal opened={unlinking !== null} onClose={() => setUnlinking(null)} title={`Unlink ${unlinking?.provider_name ?? ''}?`}>
        <Stack gap="sm">
          <Text size="sm">You can no longer sign in with this identity, its group grants are removed, and every session of yours ends, this one included.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setUnlinking(null)}>Cancel</Button>
            <Button color="owlRed" onClick={() => unlinking && void unlink(unlinking)}>Unlink and sign out</Button>
          </Group>
        </Stack>
      </Modal>
    </div>
  )
}
