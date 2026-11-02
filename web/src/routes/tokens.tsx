import { Badge, Button, Card, Checkbox, Group, Modal, NumberInput, Stack, Table, Tabs, Text, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type APIToken, type GrantInput } from '@/api/client'
import { useAllTokens, useMe, useMyTokens } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { GrantsEditor } from '@/components/GrantsEditor'
import { PageHeader } from '@/components/PageHeader'
import { SecretModal } from '@/components/SecretModal'
import { hasGlobal } from '@/lib/access'
import { ago, datetime, isSet } from '@/lib/format'
import { required, fieldError } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/tokens')({ component: TokensPage })

function TokensPage() {
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const mine = useMyTokens()
  const all = useAllTokens(admin)
  const [creating, setCreating] = useState(false)
  const [secret, setSecret] = useState<string | null>(null)
  return (
    <>
      <PageHeader title="API tokens" description="Tokens act for you, limited to the grants you give them (each within your own)."
        actions={me?.user && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New token</Button>} />
      <Tabs defaultValue="mine">
        <Tabs.List>
          <Tabs.Tab value="mine">Your tokens</Tabs.Tab>
          {admin && <Tabs.Tab value="all">All tokens</Tabs.Tab>}
        </Tabs.List>
        <Tabs.Panel value="mine" pt="sm">
          <ErrorAlert error={mine.error} />
          <TokenTable tokens={mine.data ?? []} revoke="mine" />
        </Tabs.Panel>
        {admin && (
          <Tabs.Panel value="all" pt="sm">
            <ErrorAlert error={all.error} />
            <TokenTable tokens={all.data ?? []} revoke="any" />
          </Tabs.Panel>
        )}
      </Tabs>
      <CreateTokenModal opened={creating} onClose={() => setCreating(false)} onCreated={setSecret} />
      <SecretModal title="New API token" secret={secret} onClose={() => setSecret(null)}>
        <Text size="sm">Send it as <code>Authorization: Bearer …</code>.</Text>
      </SecretModal>
    </>
  )
}

function TokenTable({ tokens, revoke }: { tokens: APIToken[]; revoke: 'mine' | 'any' }) {
  const qc = useQueryClient()
  const doRevoke = async (t: APIToken) => {
    try {
      const params = { params: { path: { token_id: t.id } } }
      unwrap(revoke === 'mine' ? await api.DELETE('/tokens/{token_id}', params) : await api.DELETE('/admin/tokens/{token_id}', params))
      void qc.invalidateQueries({ queryKey: ['tokens'] })
      notifyOk(`Token ${t.name} revoked`)
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <Card p={0}>
      <Table>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Name</Table.Th>
            <Table.Th>Grants</Table.Th>
            <Table.Th>Expires</Table.Th>
            <Table.Th>Last used</Table.Th>
            <Table.Th w={90} />
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {tokens.map((t) => (
            <Table.Tr key={t.id} opacity={isSet(t.revoked_at) ? 0.5 : 1}>
              <Table.Td>
                <Text size="sm" fw={500}>{t.name}</Text>
                <Text size="xs" c="dimmed" className="mono">…{t.hint}</Text>
              </Table.Td>
              <Table.Td><Group gap={4}>{t.grants.map((g) => <Badge key={`${g.role}@${g.domain}`} color="gray">{g.role} · {g.domain === '*' ? 'all' : g.domain}</Badge>)}</Group></Table.Td>
              <Table.Td><Text size="xs">{t.never_expires ? 'never' : datetime(t.expires_at)}</Text></Table.Td>
              <Table.Td><Text size="xs" c="dimmed">{isSet(t.last_used_at) ? `${ago(t.last_used_at)}${t.last_used_ip ? ` from ${t.last_used_ip}` : ''}` : 'never'}</Text></Table.Td>
              <Table.Td>{isSet(t.revoked_at) ? <Badge color="gray">revoked</Badge> : <Button size="compact-xs" color="owlRed" variant="light" onClick={() => void doRevoke(t)}>Revoke</Button>}</Table.Td>
            </Table.Tr>
          ))}
          {tokens.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed" ta="center" p="md">No tokens.</Text></Table.Td></Table.Tr>}
        </Table.Tbody>
      </Table>
    </Card>
  )
}

function CreateTokenModal({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (s: string) => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const form = useForm({
    defaultValues: { name: '', expires_in_days: 30, never_expires: false, grants: [{ role: 'viewer', domain: '*' }] as GrantInput[] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const res = unwrap(await api.POST('/tokens', { params: { header: { 'Idempotency-Key': key } }, body: { ...value, expires_in_days: value.never_expires ? undefined : value.expires_in_days } }))
        void qc.invalidateQueries({ queryKey: ['tokens'] })
        setKey(idempotencyKey())
        form.reset()
        onClose()
        onCreated(res.secret)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title="New API token" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="name" validators={{ onBlur: required('Name') }}>
            {(f) => <TextInput label="Name" description="What uses it, e.g. terraform-ci" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <Group align="flex-end">
            <form.Subscribe selector={(s) => s.values.never_expires}>
              {(never) => (
                <form.Field name="expires_in_days">
                  {(f) => <NumberInput label="Expires in (days)" min={1} max={365} w={160} disabled={never} value={f.state.value} onChange={(v) => f.handleChange(Number(v) || 1)} />}
                </form.Field>
              )}
            </form.Subscribe>
            <form.Field name="never_expires">
              {(f) => <Checkbox label="Never expires" mb={6} checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          </Group>
          <form.Field name="grants">{(f) => <GrantsEditor value={f.state.value} onChange={f.handleChange} />}</form.Field>
          {error && <Text c="red" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Create token</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
