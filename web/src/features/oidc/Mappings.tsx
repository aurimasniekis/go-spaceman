import { ActionIcon, Badge, Button, Group, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { IconPlus, IconTrash } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, check, unwrap, type OidcMapping, type Role } from '@/api/client'
import { keys, useOidcMappings, useSpaces } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { notifyError } from '@/lib/notify'

const ROLES: { value: Role; label: string }[] = [
  { value: 'viewer', label: 'Viewer' },
  { value: 'operator', label: 'Operator' },
  { value: 'admin', label: 'Admin' },
]

export const roleColor = (r: string) => (r === 'admin' ? 'owlPurple' : r === 'operator' ? 'owlBlue' : 'gray')

/**
 * A provider's group mappings (SM-AUTH-002): a value of the groups claim grants a role in every space ("*") or in one
 * space. They are applied at every SSO sign-in and only to SSO sessions.
 */
export function Mappings({ providerId, providerName }: { providerId: string; providerName: string }) {
  const qc = useQueryClient()
  const mappings = useOidcMappings(providerId)
  const spaces = useSpaces()
  const [group, setGroup] = useState('')
  const [role, setRole] = useState<Role>('viewer')
  const [domain, setDomain] = useState('*')
  const [error, setError] = useState('')
  const domains = [{ value: '*', label: 'All spaces (global)' }, ...(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))]
  const spaceName = (d: string) => (d === '*' ? 'all spaces' : (spaces.data?.find((s) => s.id === d)?.name ?? d))
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.oidcMappings(providerId) })

  const add = async () => {
    setError('')
    try {
      unwrap(await api.POST('/oidc/providers/{provider_id}/mappings', { params: { path: { provider_id: providerId } }, body: { group_value: group.trim(), role, domain } }))
      setGroup('')
      refresh()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  const remove = async (m: OidcMapping) => {
    try {
      check(await api.DELETE('/oidc/providers/{provider_id}/mappings/{mapping_id}', { params: { path: { provider_id: providerId, mapping_id: m.id } } }))
      refresh()
    } catch (e) {
      notifyError(e, 'Could not remove the mapping')
    }
  }
  return (
    <Stack gap="xs">
      <ErrorAlert error={mappings.error} />
      <Table>
        <Table.Thead>
          <Table.Tr><Table.Th>Group in the claim</Table.Th><Table.Th>Grants</Table.Th><Table.Th w={40} /></Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {(mappings.data ?? []).map((m) => (
            <Table.Tr key={m.id}>
              <Table.Td><Text size="sm" className="mono">{m.group_value}</Text></Table.Td>
              <Table.Td><Badge variant={m.domain === '*' ? 'filled' : 'light'} color={roleColor(m.role)}>{m.role} · {spaceName(m.domain)}</Badge></Table.Td>
              <Table.Td><ActionIcon variant="subtle" color="owlRed" aria-label={`Remove the mapping of ${m.group_value} in ${providerName}`} onClick={() => void remove(m)}><IconTrash size={14} /></ActionIcon></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {mappings.isSuccess && mappings.data.length === 0 && <Text size="sm" c="dimmed">No mappings: people who sign in here get only the grants given to them on the Users page.</Text>}
      <Group gap="xs" align="flex-end" wrap="nowrap">
        <TextInput label="Group" aria-label={`New group for ${providerName}`} placeholder="spaceman-operators" flex={1} value={group} onChange={(e) => setGroup(e.target.value)} />
        <Select label="Role" aria-label={`Role for ${providerName}`} w={130} allowDeselect={false} data={ROLES} value={role} onChange={(v) => v && setRole(v as Role)} />
        <Select label="Scope" aria-label={`Scope for ${providerName}`} w={220} allowDeselect={false} searchable data={domains} value={domain} onChange={(v) => v && setDomain(v)} />
        <Button variant="default" leftSection={<IconPlus size={14} />} disabled={!group.trim()} onClick={() => void add()}>Add mapping</Button>
      </Group>
      {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
    </Stack>
  )
}
