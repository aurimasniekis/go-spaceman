import { Badge, Button, Card, Group, PasswordInput, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, unwrap } from '@/api/client'
import { keys, useMe, useSessions } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SecretModal } from '@/components/SecretModal'
import { Identities } from '@/features/auth/Identities'
import { NotificationPrefsCard } from '@/features/notifications/NotificationPrefs'
import { hasGlobal } from '@/lib/access'
import { ago, datetime, isSet } from '@/lib/format'
import { fieldError } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/account')({
  validateSearch: (s: Record<string, unknown>): { sso_error?: string } => (typeof s.sso_error === 'string' ? { sso_error: s.sso_error } : {}),
  component: AccountPage,
})

function AccountPage() {
  const me = useMe().data
  const { sso_error } = Route.useSearch()
  return (
    <>
      <PageHeader title="Account" description={me?.user ? `${me.user.display_name}${me.user.email ? ` · ${me.user.email}` : ''}` : 'Signed in with the root token'} />
      <Stack gap="lg">
        {me?.user && <Sessions />}
        {me?.user && <Identities ssoError={sso_error} />}
        {me?.user && <NotificationPrefsCard />}
        {me?.user?.has_password && <ChangePassword />}
        {me?.user?.mfa_enrolled && <RecoveryCodes />}
        {hasGlobal(me, 'admin') && <RootToken />}
      </Stack>
    </>
  )
}

function Sessions() {
  const sessions = useSessions()
  const qc = useQueryClient()
  const revoke = async (id: string) => {
    try {
      check(await api.DELETE('/auth/sessions/{session_id}', { params: { path: { session_id: id } } }))
      void qc.invalidateQueries({ queryKey: keys.sessions })
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <div>
      <Title order={5} mb="xs">Sessions</Title>
      <ErrorAlert error={sessions.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr><Table.Th>Device</Table.Th><Table.Th>Method</Table.Th><Table.Th>IP</Table.Th><Table.Th>Last seen</Table.Th><Table.Th>Expires</Table.Th><Table.Th w={90} /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(sessions.data ?? []).map((s) => (
              <Table.Tr key={s.id}>
                <Table.Td><Text size="xs" lineClamp={1} maw={320}>{s.user_agent || '—'}</Text></Table.Td>
                <Table.Td>{s.auth_method}</Table.Td>
                <Table.Td className="mono">{s.ip}</Table.Td>
                <Table.Td><Text size="xs">{ago(s.last_seen_at)}</Text></Table.Td>
                <Table.Td><Text size="xs">{datetime(s.expires_at)}</Text></Table.Td>
                <Table.Td>{s.current ? <Badge color="owlGreen">this one</Badge> : <Button size="compact-xs" variant="light" color="owlRed" onClick={() => void revoke(s.id)}>Revoke</Button>}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Card>
    </div>
  )
}

function ChangePassword() {
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { current: '', next: '', confirm: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        check(await api.POST('/auth/password', { body: { current: value.current, new: value.next } }))
        form.reset()
        notifyOk('Password changed; your other sessions were signed out')
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <div>
      <Title order={5} mb="xs">Change password</Title>
      <Card maw={520}>
        <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
          <Stack gap="sm">
            <form.Field name="current">{(f) => <PasswordInput label="Current password" autoComplete="current-password" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            <SimpleGrid cols={2}>
              <form.Field name="next" validators={{ onBlur: ({ value }) => (value.length < 12 ? 'Use at least 12 characters' : undefined) }}>
                {(f) => <PasswordInput label="New password" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
              </form.Field>
              <form.Field name="confirm" validators={{ onChangeListenTo: ['next'], onBlur: ({ value, fieldApi }) => (value !== fieldApi.form.getFieldValue('next') ? 'Passwords do not match' : undefined) }}>
                {(f) => <PasswordInput label="Confirm" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
              </form.Field>
            </SimpleGrid>
            {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
            <Group><form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Change password</Button>}</form.Subscribe></Group>
          </Stack>
        </form>
      </Card>
    </div>
  )
}

/** Replaces the recovery codes (the old ones stop working); needs a recent re-authentication with TOTP. */
function RecoveryCodes() {
  const [codes, setCodes] = useState<string[] | null>(null)
  const regenerate = async () => {
    try {
      setCodes(unwrap(await api.POST('/auth/mfa/recovery-codes')).recovery_codes)
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <div>
      <Title order={5} mb="xs">Recovery codes</Title>
      <Card maw={520}>
        <Stack gap="xs">
          <Text size="sm">Each recovery code signs you in once if you lose your authenticator. Replacing them invalidates every earlier code.</Text>
          <Group><Button variant="default" onClick={() => void regenerate()}>Replace recovery codes</Button></Group>
        </Stack>
      </Card>
      <SecretModal title="New recovery codes" secret={codes ? codes.join('\n') : null} onClose={() => setCodes(null)} />
    </div>
  )
}

/** The bootstrap root token (SM-AUTH-010): metadata, rotation and revocation; never stored in the browser. */
function RootToken() {
  const qc = useQueryClient()
  const rt = useQuery({ queryKey: keys.rootToken, queryFn: async () => {
    const res = await api.GET('/admin/root-token')
    return res.response.status === 404 ? null : unwrap(res)
  } })
  const [secret, setSecret] = useState<string | null>(null)
  const rotate = async () => {
    try {
      setSecret(unwrap(await api.POST('/admin/root-token/rotate')).secret)
      void qc.invalidateQueries({ queryKey: keys.rootToken })
    } catch (e) {
      notifyError(e)
    }
  }
  const revoke = async () => {
    try {
      check(await api.DELETE('/admin/root-token'))
      void qc.invalidateQueries({ queryKey: keys.rootToken })
      notifyOk('Root token revoked; `spaceman admin root-token rotate` on the server issues a new one')
    } catch (e) {
      notifyError(e)
    }
  }
  const active = rt.data && !isSet(rt.data.revoked_at)
  return (
    <div>
      <Title order={5} mb="xs">Root token</Title>
      <Card maw={520}>
        <Stack gap="xs">
          <ErrorAlert error={rt.error} />
          {active ? (
            <>
              <Text size="sm">Active · <span className="mono">…{rt.data?.hint}</span> · created {datetime(rt.data?.created_at)}</Text>
              <Text size="xs" c="dimmed">Last used {isSet(rt.data?.last_used_at) ? `${ago(rt.data?.last_used_at)}${rt.data?.last_used_ip ? ` from ${rt.data.last_used_ip}` : ''}` : 'never'}</Text>
            </>
          ) : (
            <Text size="sm" c="dimmed">No active root token.</Text>
          )}
          <Group gap="xs">
            <Button variant="default" onClick={() => void rotate()}>{active ? 'Rotate' : 'Issue new'}</Button>
            {active && <Button variant="light" color="owlRed" onClick={() => void revoke()}>Revoke</Button>}
          </Group>
        </Stack>
      </Card>
      <SecretModal title="New root token" secret={secret} onClose={() => setSecret(null)}>
        <Text size="sm">Store it offline. It can create administrators and is never given to nodes or agents.</Text>
      </SecretModal>
    </div>
  )
}
