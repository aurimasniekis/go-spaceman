import { Badge, Button, Card, Group, Menu, Modal, PasswordInput, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { IconDots, IconPlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, ifMatch, unwrap, type GrantInput, type User } from '@/api/client'
import { keys, useMe, useSpaces, useUsers } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { GrantsEditor } from '@/components/GrantsEditor'
import { PageHeader } from '@/components/PageHeader'
import { SecretModal } from '@/components/SecretModal'
import { hasGlobal } from '@/lib/access'
import { ago, isSet } from '@/lib/format'
import { fieldError, required } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/users')({ component: UsersPage })

function UsersPage() {
  const users = useUsers()
  const spaces = useSpaces()
  const me = useMe().data
  const qc = useQueryClient()
  const admin = hasGlobal(me, 'admin')
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [reset, setReset] = useState<{ user: string; token: string } | null>(null)
  const spaceName = (d: string) => (d === '*' ? 'all spaces' : (spaces.data?.find((s) => s.id === d)?.name ?? d))

  const toggle = async (u: User) => {
    try {
      const path = u.enabled ? '/users/{user_id}/disable' : '/users/{user_id}/enable'
      unwrap(await api.POST(path, { params: { path: { user_id: u.id }, header: { 'If-Match': ifMatch(u.version) } } }))
      void qc.invalidateQueries({ queryKey: keys.users })
      notifyOk(`${u.display_name} ${u.enabled ? 'disabled' : 'enabled'}`)
    } catch (e) {
      notifyError(e)
    }
  }
  const issueReset = async (u: User) => {
    try {
      const r = unwrap(await api.POST('/users/{user_id}/reset-token', { params: { path: { user_id: u.id } } }))
      setReset({ user: u.display_name, token: r.token })
    } catch (e) {
      notifyError(e)
    }
  }

  return (
    <>
      <PageHeader title="Users" description="People who sign in to SpaceMan, with their role grants."
        actions={admin && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New user</Button>} />
      <ErrorAlert error={users.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>User</Table.Th>
              <Table.Th>Grants</Table.Th>
              <Table.Th>MFA</Table.Th>
              <Table.Th>Last sign-in</Table.Th>
              {admin && <Table.Th w={40} />}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(users.data ?? []).map((u) => (
              <Table.Tr key={u.id} opacity={u.enabled ? 1 : 0.55}>
                <Table.Td>
                  <Group gap="xs"><Text size="sm" fw={500}>{u.display_name}</Text>{!u.enabled && <Badge color="gray">disabled</Badge>}</Group>
                  <Text size="xs" c="dimmed">{u.email || u.provisioned_via}</Text>
                </Table.Td>
                <Table.Td>
                  <Group gap={4}>
                    {u.grants.map((g) => <Badge key={`${g.role}@${g.domain}@${g.source}`} variant={g.domain === '*' ? 'filled' : 'light'} color={g.role === 'admin' ? 'owlPurple' : g.role === 'operator' ? 'owlBlue' : 'gray'}>{g.role} · {spaceName(g.domain)}</Badge>)}
                    {u.grants.length === 0 && <Text size="xs" c="dimmed">none</Text>}
                  </Group>
                </Table.Td>
                <Table.Td>{u.mfa_enrolled ? <Badge color="owlGreen">enrolled</Badge> : u.mfa_required ? <Badge color="owlYellow">required</Badge> : <Text size="xs" c="dimmed">off</Text>}</Table.Td>
                <Table.Td><Text size="xs" c="dimmed">{isSet(u.last_login_at) ? ago(u.last_login_at) : 'never'}</Text></Table.Td>
                {admin && (
                  <Table.Td>
                    <Menu position="bottom-end">
                      <Menu.Target><Button variant="subtle" size="compact-xs" aria-label={`Actions for ${u.display_name}`}><IconDots size={14} /></Button></Menu.Target>
                      <Menu.Dropdown>
                        <Menu.Item onClick={() => setEditing(u)}>Edit grants</Menu.Item>
                        <Menu.Item onClick={() => void issueReset(u)}>Password reset token</Menu.Item>
                        <Menu.Item color={u.enabled ? 'owlRed' : undefined} onClick={() => void toggle(u)} disabled={u.id === me?.user?.id}>{u.enabled ? 'Disable' : 'Enable'}</Menu.Item>
                      </Menu.Dropdown>
                    </Menu>
                  </Table.Td>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Card>
      <CreateUserModal opened={creating} onClose={() => setCreating(false)} />
      {editing && <EditGrantsModal user={editing} onClose={() => setEditing(null)} />}
      <SecretModal title={`Password reset token for ${reset?.user ?? ''}`} secret={reset?.token ?? null} onClose={() => setReset(null)}>
        <Text size="sm">Give it to the user over a trusted channel. They open <b>{window.location.origin}/reset</b>, paste it and choose a new password. It works once.</Text>
      </SecretModal>
    </>
  )
}

const manual = (u: User): GrantInput[] => u.grants.filter((g) => g.source === 'manual').map((g) => ({ role: g.role as GrantInput['role'], domain: g.domain }))

function EditGrantsModal({ user, onClose }: { user: User; onClose: () => void }) {
  const qc = useQueryClient()
  const [grants, setGrants] = useState<GrantInput[]>(manual(user))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setBusy(true)
    setError('')
    try {
      unwrap(await api.PUT('/users/{user_id}/grants', { params: { path: { user_id: user.id }, header: { 'If-Match': ifMatch(user.version) } }, body: { grants } }))
      void qc.invalidateQueries({ queryKey: keys.users })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened onClose={onClose} title={`Grants for ${user.display_name}`} size="lg">
      <Stack gap="sm">
        <GrantsEditor value={grants} onChange={setGrants} />
        {user.grants.some((g) => g.source !== 'manual') && <Text size="xs" c="dimmed">Grants from single sign-on are managed by the identity provider and are not shown here.</Text>}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>Save grants</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function CreateUserModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const form = useForm({
    defaultValues: { display_name: '', email: '', password: '', mfa_required: true, grants: [{ role: 'viewer', domain: '*' }] as GrantInput[] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.POST('/users', { params: { header: { 'Idempotency-Key': key } }, body: { ...value, password: value.password || undefined, email: value.email || undefined } }))
        void qc.invalidateQueries({ queryKey: keys.users })
        setKey(idempotencyKey())
        form.reset()
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title="New user" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="display_name" validators={{ onBlur: required('Name') }}>
            {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="email">
            {(f) => <TextInput label="Email" type="email" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="password">
            {(f) => <PasswordInput label="Initial password" description="Leave empty to issue a reset token instead" autoComplete="new-password" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="mfa_required">
            {(f) => <Switch label="Require multi-factor authentication" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <form.Field name="grants">{(f) => <GrantsEditor value={f.state.value} onChange={f.handleChange} />}</form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Create user</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
