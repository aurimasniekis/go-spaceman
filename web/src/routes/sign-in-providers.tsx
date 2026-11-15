import { ActionIcon, Alert, Badge, Button, Card, Code, CopyButton, Group, Menu, Modal, Stack, Table, Text, Title } from '@mantine/core'
import { IconDots, IconPlus } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, unwrap, type OidcProvider } from '@/api/client'
import { keys, useOidcProviders, useSpaces, useUsers } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { Mappings, roleColor } from '@/features/oidc/Mappings'
import { ProviderModal } from '@/features/oidc/ProviderModal'
import { SignInSettingsCard, signInSettingsKey } from '@/features/oidc/SignInSettings'
import { ago, isSet } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { ssoStartUrl } from '@/lib/sso'

export const Route = createFileRoute('/sign-in-providers')({
  validateSearch: (s: Record<string, unknown>): { test_login?: string } => (typeof s.test_login === 'string' ? { test_login: s.test_login } : {}),
  component: SignInProvidersPage,
})

/**
 * OIDC sign-in providers (SM-AUTH-001/002, ADR-0029): Porch or any other OpenID Connect provider, with write-only client
 * secrets, group mappings to SpaceMan grants and a test login that previews the mapping without signing in; and the
 * sign-in settings (SSO only, direct provider; ADR-0045).
 */
function SignInProvidersPage() {
  const qc = useQueryClient()
  const providers = useOidcProviders()
  const { test_login } = Route.useSearch()
  const [editing, setEditing] = useState<OidcProvider | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<OidcProvider | null>(null)
  const redirectUri = providers.data?.redirect_uri ?? ''
  const remove = async (p: OidcProvider) => {
    try {
      check(await api.DELETE('/oidc/providers/{provider_id}', { params: { path: { provider_id: p.id } } }))
      void qc.invalidateQueries({ queryKey: keys.oidcProviders })
      void qc.invalidateQueries({ queryKey: keys.signInProviders })
      void qc.invalidateQueries({ queryKey: signInSettingsKey })
      notifyOk(`${p.display_name} deleted`)
      setDeleting(null)
    } catch (e) {
      notifyError(e, 'Could not delete the provider')
    }
  }
  return (
    <>
      <PageHeader title="Sign-in providers" description="OpenID Connect providers people sign in with, such as Porch, and the grants their groups map to."
        actions={<Button leftSection={<IconPlus size={14} />} onClick={() => setAdding(true)}>Add provider</Button>} />
      <ErrorAlert error={providers.error} />
      <Stack gap="md">
        {redirectUri && (
          <Alert variant="light" color="owlBlue" title="Redirect URI">
            <Group gap="xs">
              <Code className="mono">{redirectUri}</Code>
              <CopyButton value={redirectUri}>{({ copied, copy }) => <Button size="compact-xs" variant="default" onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}</CopyButton>
            </Group>
            <Text size="xs" mt={4}>Register exactly this URI at every provider. SpaceMan always uses PKCE with S256.</Text>
          </Alert>
        )}
        {providers.data && <SignInSettingsCard providers={providers.data.items} />}
        {(providers.data?.items ?? []).map((p) => (
          <Card key={p.id}>
            <Stack gap="sm">
              <Group justify="space-between" align="flex-start" wrap="nowrap">
                <Stack gap={2}>
                  <Group gap="xs">
                    <Title order={5}>{p.display_name}</Title>
                    <Badge color={p.enabled ? 'owlGreen' : 'gray'}>{p.enabled ? 'enabled' : 'disabled'}</Badge>
                    <Badge variant="light">{p.provisioning === 'jit' ? 'JIT provisioning' : 'linked users only'}</Badge>
                  </Group>
                  <Text size="xs" c="dimmed" className="mono">{p.issuer}</Text>
                  <Text size="xs" c="dimmed">
                    client <span className="mono">{p.client_id}</span> · secret {p.has_client_secret ? 'stored (write-only)' : 'none'} · groups claim{' '}
                    <span className="mono">{p.groups_claim || '—'}</span> · test login {isSet(p.test_login_ok_at) ? ago(p.test_login_ok_at) : 'never'}
                  </Text>
                </Stack>
                <Group gap="xs" wrap="nowrap">
                  <Button size="xs" variant="default" component="a" href={ssoStartUrl(p.slug, 'test', '/sign-in-providers')}>Test login</Button>
                  <Menu position="bottom-end">
                    <Menu.Target><ActionIcon variant="subtle" aria-label={`Actions for ${p.display_name}`}><IconDots size={16} /></ActionIcon></Menu.Target>
                    <Menu.Dropdown>
                      <Menu.Item onClick={() => setEditing(p)}>Edit…</Menu.Item>
                      <Menu.Item color="owlRed" onClick={() => setDeleting(p)}>Delete…</Menu.Item>
                    </Menu.Dropdown>
                  </Menu>
                </Group>
              </Group>
              <Mappings providerId={p.id} providerName={p.display_name} />
            </Stack>
          </Card>
        ))}
        {providers.isSuccess && providers.data.items.length === 0 && (
          <Card><Text size="sm" c="dimmed" ta="center">No sign-in providers. Add Porch (or another OpenID Connect provider) so people can sign in with it.</Text></Card>
        )}
      </Stack>
      {adding && <ProviderModal redirectUri={redirectUri} onClose={() => setAdding(false)} />}
      {editing && <ProviderModal provider={editing} redirectUri={redirectUri} onClose={() => setEditing(null)} />}
      {test_login && <TestLoginModal providerId={test_login} />}
      <Modal opened={deleting !== null} onClose={() => setDeleting(null)} title={`Delete ${deleting?.display_name ?? ''}?`}>
        <Stack gap="sm">
          <Text size="sm">Its mappings and linked identities are removed. Linked people lose the grants its groups gave them and can no longer sign in with it.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button color="owlRed" onClick={() => deleting && void remove(deleting)}>Delete</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}

/** The claim-mapping preview of the caller's latest test login (no session was created). */
function TestLoginModal({ providerId }: { providerId: string }) {
  const navigate = useNavigate()
  const users = useUsers()
  const spaces = useSpaces()
  const res = useQuery({
    queryKey: ['oidc-test-login', providerId],
    queryFn: async ({ signal }) => unwrap(await api.GET('/oidc/providers/{provider_id}/test-login', { params: { path: { provider_id: providerId } }, signal })),
    retry: false,
  })
  const close = () => void navigate({ to: '/sign-in-providers', search: {} })
  const spaceName = (d: string) => (d === '*' ? 'all spaces' : (spaces.data?.find((s) => s.id === d)?.name ?? d))
  const linked = users.data?.find((u) => u.id === res.data?.linked_user_id)
  const c = res.data?.claims
  return (
    <Modal opened onClose={close} title="Test login result" size="lg">
      <Stack gap="sm">
        <ErrorAlert error={res.error} />
        {c && (
          <>
            <Alert color="owlGreen" variant="light">The provider signed you in and SpaceMan validated the ID token. No session was created.</Alert>
            <Table>
              <Table.Tbody>
                <Table.Tr><Table.Th w={160}>Subject</Table.Th><Table.Td className="mono">{c.sub}</Table.Td></Table.Tr>
                <Table.Tr><Table.Th>Issuer</Table.Th><Table.Td className="mono">{c.iss}</Table.Td></Table.Tr>
                <Table.Tr><Table.Th>Email</Table.Th><Table.Td>{c.email || '—'} {c.email && (c.email_verified ? '(verified)' : '(not verified)')}</Table.Td></Table.Tr>
                <Table.Tr><Table.Th>Groups</Table.Th><Table.Td>{(c.groups ?? []).length ? (c.groups ?? []).join(', ') : 'none'}</Table.Td></Table.Tr>
                <Table.Tr><Table.Th>Linked user</Table.Th><Table.Td>{res.data?.linked_user_id ? (linked?.display_name ?? res.data.linked_user_id) : res.data?.would_provision ? 'none: would be created at sign-in' : `none (${res.data?.provision_reason ?? 'not provisioned'})`}</Table.Td></Table.Tr>
                <Table.Tr>
                  <Table.Th>Mapped grants</Table.Th>
                  <Table.Td>
                    <Group gap={4}>
                      {(res.data?.mapped_grants ?? []).map((g) => <Badge key={`${g.role}@${g.domain}`} color={roleColor(g.role)} variant={g.domain === '*' ? 'filled' : 'light'}>{g.role} · {spaceName(g.domain)}</Badge>)}
                      {(res.data?.mapped_grants ?? []).length === 0 && <Text size="sm" c="dimmed">none</Text>}
                    </Group>
                  </Table.Td>
                </Table.Tr>
              </Table.Tbody>
            </Table>
            <Text size="xs" c="dimmed">Mapped grants apply only to SSO sessions; the user's own grants apply to every session.</Text>
          </>
        )}
        <Group justify="flex-end"><Button onClick={close}>Done</Button></Group>
      </Stack>
    </Modal>
  )
}
