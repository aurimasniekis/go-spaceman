import { ActionIcon, Alert, Anchor, Badge, Button, Card, Checkbox, Drawer, Group, Modal, MultiSelect, NumberInput, Select, SimpleGrid, Stack, Table, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { IconExternalLink, IconPlugConnected, IconPlus, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type McpDiscovered, type McpServer } from '@/api/client'
import { keys, useMcpGrants, useMcpServers, useMe, useSpaces } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { AddSpacemanMcpModal } from '@/features/mcp/AddSpacemanMcpModal'
import { SpacemanMcpDetail } from '@/features/mcp/SpacemanMcpDetail'
import { hasGlobal } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/mcp-servers')({ component: McpServersPage })

/** External MCP servers behind SpaceMan's gateway (SM-MCP-001/002): agents see their tools, never their address or credentials. */
function McpServersPage() {
  const servers = useMcpServers()
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [editing, setEditing] = useState<McpServer | 'new' | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [addingSpaceman, setAddingSpaceman] = useState(false)
  const current = servers.data?.find((s) => s.id === open) ?? null
  return (
    <>
      <PageHeader title="MCP servers" description="External MCP servers agents reach through SpaceMan. Each run gets its own upstream session; headers are sealed and never shown again."
        actions={canWrite && (
          <Group gap="xs">
            <Button variant="default" leftSection={<IconPlus size={14} />} onClick={() => setAddingSpaceman(true)}>Add SpaceMan MCP</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add server</Button>
          </Group>
        )} />
      <ErrorAlert error={servers.error} />
      <Card p={0}>
        <Table highlightOnHover>
          <Table.Thead><Table.Tr><Table.Th>Server</Table.Th><Table.Th>Endpoint</Table.Th><Table.Th>Headers</Table.Th><Table.Th>Grants</Table.Th><Table.Th>Limits</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(servers.data ?? []).map((s) => (
              <Table.Tr key={s.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(s.id)}>
                <Table.Td>
                  <Group gap="xs"><Text size="sm" fw={500}>{s.name}</Text>{s.kind === 'spaceman' && <Badge size="xs" color="owlPurple" variant="light">SpaceMan</Badge>}{!s.enabled && <Badge size="xs" color="gray">disabled</Badge>}{s.allow_private && <Badge size="xs" color="owlYellow" variant="light">private network</Badge>}</Group>
                  <Text size="xs" c="dimmed" className="mono">{s.slug}__…</Text>
                </Table.Td>
                <Table.Td>{s.kind === 'spaceman' ? <Text size="xs" c="dimmed">in central{s.spaceman ? ` · ${s.spaceman.preset ?? 'admin'} preset` : ''}</Text> : <Text size="xs" className="mono" style={{ wordBreak: 'break-all' }}>{s.url}</Text>}</Table.Td>
                <Table.Td><Text size="xs" className="mono">{s.header_names.join(', ') || '—'}</Text></Table.Td>
                <Table.Td>{s.grants}</Table.Td>
                <Table.Td><Text size="xs">{s.call_timeout_ms / 1000}s · {s.max_concurrency} at once · {Math.round(s.max_result_bytes / 1024)} KiB</Text></Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {servers.isSuccess && servers.data.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">No MCP servers yet. Add one, check its tools, then grant it to the space roles that need it.</Text>
        )}
      </Card>
      <Text size="xs" c="dimmed" mt="sm">
        Your own MCP client (Claude Code, an IDE) can use SpaceMan’s tools with your API token through the external endpoint{' '}
        <span className="mono">/api/v1/mcp</span>:{' '}
        <Anchor href="https://aurimasniekis.github.io/go-spaceman/guide/mcp-endpoint" target="_blank" rel="noreferrer" size="xs">
          see the guide <IconExternalLink size={10} />
        </Anchor>.
      </Text>
      <AddSpacemanMcpModal opened={addingSpaceman} onClose={() => setAddingSpaceman(false)} onCreated={(srv) => setOpen(srv.id)} />
      {editing && <ServerModal server={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="xl" title={current?.name ?? ''}>
        {current && (current.kind === 'spaceman'
          ? <SpacemanMcpDetail server={current} canWrite={canWrite} onDeleted={() => setOpen(null)} grants={<ServerGrants server={current} canWrite={canWrite} toolOptions={[]} />} />
          : <ServerDetail server={current} canWrite={canWrite} onEdit={() => setEditing(current)} onDeleted={() => setOpen(null)} />)}
      </Drawer>
    </>
  )
}

type HeaderRow = { name: string; value: string }

function ServerModal({ server, onClose }: { server: McpServer | null; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: {
      slug: server?.slug ?? '', name: server?.name ?? '', description: server?.description ?? '', url: server?.url ?? '',
      allow_private: server?.allow_private ?? false, tools: server?.tools ?? [], call_timeout_ms: server?.call_timeout_ms ?? 20000,
      max_concurrency: server?.max_concurrency ?? 4, max_result_bytes: server?.max_result_bytes ?? 262144, enabled: server?.enabled ?? true,
      replace: server === null,
      headers: (server === null ? [{ name: 'Authorization', value: '' }] : []) as HeaderRow[],
    },
    onSubmit: async ({ value }) => {
      setError('')
      const { replace, headers, ...v } = value
      const h = Object.fromEntries(headers.filter((r) => r.name).map((r) => [r.name, r.value]))
      const body = { ...v, slug: server ? undefined : v.slug, set_headers: replace, headers: replace ? h : undefined }
      try {
        if (server) unwrap(await api.PUT('/mcp-servers/{server_id}', { params: { path: { server_id: server.id }, header: { 'If-Match': ifMatch(server.version) } }, body }))
        else unwrap(await api.POST('/mcp-servers', { body }))
        void qc.invalidateQueries({ queryKey: keys.mcpServers })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={server ? `Edit ${server.name}` : 'Add an MCP server'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <SimpleGrid cols={2}>
            <form.Field name="name">{(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            <form.Field name="slug">
              {(f) => <TextInput label="Slug" description="Tools appear as slug__tool" classNames={{ input: 'mono' }} disabled={server !== null} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="url">
            {(f) => <TextInput label="Endpoint" description="Streamable HTTP; https unless private networks are allowed" placeholder="https://mcp.example.com/mcp" classNames={{ input: 'mono' }}
              value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="description">{(f) => <Textarea label="Description" autosize minRows={1} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          <form.Subscribe selector={(st) => st.values.replace}>
            {(replace) => (
              <Stack gap={4}>
                <Group justify="space-between">
                  <Text size="sm" fw={500}>Request headers</Text>
                  {server && <form.Field name="replace">{(f) => <Checkbox label="Replace the stored headers" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}</form.Field>}
                </Group>
                {server && !replace && <Text size="xs" c="dimmed">Stored: {server.header_names.join(', ') || 'none'}. Values are never shown.</Text>}
                {replace && (
                  <form.Field name="headers" mode="array">
                    {(list) => (
                      <>
                        {list.state.value.map((_, i) => (
                          <Group key={i} gap="xs" wrap="nowrap">
                            <form.Field name={`headers[${i}].name`}>
                              {(f) => <TextInput aria-label={`Header ${i + 1} name`} placeholder="Authorization" classNames={{ input: 'mono' }} style={{ flex: 1 }} value={f.state.value}
                                onChange={(e) => f.handleChange(e.target.value)} />}
                            </form.Field>
                            <form.Field name={`headers[${i}].value`}>
                              {(f) => <TextInput aria-label={`Header ${i + 1} value`} type="password" placeholder="Bearer …" style={{ flex: 2 }} value={f.state.value} autoComplete="off"
                                onChange={(e) => f.handleChange(e.target.value)} />}
                            </form.Field>
                            <ActionIcon variant="subtle" aria-label={`Remove header ${i + 1}`} onClick={() => list.removeValue(i)}><IconTrash size={14} /></ActionIcon>
                          </Group>
                        ))}
                        <Button variant="subtle" size="xs" w="fit-content" onClick={() => list.pushValue({ name: '', value: '' })}>Add header</Button>
                      </>
                    )}
                  </form.Field>
                )}
              </Stack>
            )}
          </form.Subscribe>
          <form.Field name="tools">
            {(f) => <TagsInput label="Offered tools" description="Upstream tool names agents may use; empty offers every tool" value={f.state.value} onChange={(tools) => f.handleChange(tools)} />}
          </form.Field>
          <SimpleGrid cols={3}>
            <form.Field name="call_timeout_ms">
              {(f) => <NumberInput label="Call timeout (ms)" min={1000} max={20000} step={1000} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 20000)} />}
            </form.Field>
            <form.Field name="max_concurrency">
              {(f) => <NumberInput label="Concurrent calls" min={1} max={32} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 4)} />}
            </form.Field>
            <form.Field name="max_result_bytes">
              {(f) => <NumberInput label="Max result (bytes)" min={1024} max={4194304} step={65536} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 262144)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="allow_private">
            {(f) => <Checkbox label="Allow private networks" description="Loopback, private and unique-local addresses, and plain http. Link-local (cloud metadata) addresses stay refused."
              checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <form.Field name="enabled">{(f) => <Checkbox label="Enabled" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}</form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

function ServerDetail({ server, canWrite, onEdit, onDeleted }: { server: McpServer; canWrite: boolean; onEdit: () => void; onDeleted: () => void }) {
  const qc = useQueryClient()
  const [found, setFound] = useState<McpDiscovered | null>(null)
  const [checking, setChecking] = useState(false)
  const runCheck = async () => {
    setChecking(true)
    try {
      setFound(unwrap(await api.POST('/mcp-servers/{server_id}/check', { params: { path: { server_id: server.id } } })))
    } catch (e) {
      notifyError(e, 'Could not check the server')
    } finally {
      setChecking(false)
    }
  }
  const remove = async () => {
    try {
      check(await api.DELETE('/mcp-servers/{server_id}', { params: { path: { server_id: server.id } } }))
      void qc.invalidateQueries({ queryKey: keys.mcpServers })
      onDeleted()
    } catch (e) {
      notifyError(e, 'Could not delete the server')
    }
  }
  const toolOptions = found?.tools.filter((t) => t.usable).map((t) => t.name) ?? server.tools
  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">{server.description || 'No description.'}</Text>
      <Group gap="xs">
        <Button variant="default" leftSection={<IconPlugConnected size={14} />} loading={checking} onClick={() => void runCheck()}>Check connection</Button>
        {canWrite && <Button variant="default" onClick={onEdit}>Edit…</Button>}
        {canWrite && <Button variant="subtle" color="owlRed" onClick={() => void remove()}>Delete</Button>}
      </Group>
      {found && (found.ok ? (
        <Card withBorder p="sm">
          <Text size="sm" fw={500} mb={4}>Connected to {found.server || 'the server'} in {found.elapsed_ms} ms · {found.tools.length} tools</Text>
          <Table>
            <Table.Tbody>
              {found.tools.map((t) => (
                <Table.Tr key={t.name}>
                  <Table.Td><Text size="xs" className="mono">{server.slug}__{t.name}</Text></Table.Td>
                  <Table.Td><Text size="xs">{t.description}</Text></Table.Td>
                  <Table.Td>{t.read_only && <Badge size="xs" variant="light">read-only</Badge>}{!t.usable && <Badge size="xs" color="gray">name too long</Badge>}
                    {server.tools.length > 0 && !server.tools.includes(t.name) && <Badge size="xs" color="gray">not offered</Badge>}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      ) : <Alert color="owlRed" title="Connection failed">{found.error}</Alert>)}
      <ServerGrants server={server} canWrite={canWrite} toolOptions={toolOptions} />
    </Stack>
  )
}

/** The spaces and space roles a server is granted to (both server kinds). */
function ServerGrants({ server, canWrite, toolOptions }: { server: McpServer; canWrite: boolean; toolOptions: string[] }) {
  const qc = useQueryClient()
  const grants = useMcpGrants(server.id)
  const spaces = useSpaces()
  const grantForm = useForm({
    defaultValues: { space_id: '', role: '*', tools: [] as string[] },
    onSubmit: async ({ value, formApi }) => {
      try {
        unwrap(await api.POST('/mcp-servers/{server_id}/grants', { params: { path: { server_id: server.id } }, body: value }))
        formApi.reset()
        refresh()
        notifyOk('Granted')
      } catch (e) {
        notifyError(e, 'Could not grant the server')
      }
    },
  })
  const spaceName = (id: string) => spaces.data?.find((s) => s.id === id)?.name ?? id
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.mcpGrants(server.id) })
    void qc.invalidateQueries({ queryKey: keys.mcpServers })
    void qc.invalidateQueries({ queryKey: ['space-mcp'] })
  }
  const revoke = async (id: string) => {
    try {
      check(await api.DELETE('/mcp-servers/{server_id}/grants/{grant_id}', { params: { path: { server_id: server.id, grant_id: id } } }))
      refresh()
    } catch (e) {
      notifyError(e, 'Could not revoke the grant')
    }
  }
  return (
    <Stack gap="xs">
      <Text fw={600}>Grants</Text>
      <Text size="xs" c="dimmed">Runs of a granted space role see the server's tools. Each call checks the grant again, so revoking takes effect on the next call.</Text>
      <Table>
        <Table.Thead><Table.Tr><Table.Th>Space</Table.Th><Table.Th>Role</Table.Th><Table.Th>Tools</Table.Th><Table.Th /></Table.Tr></Table.Thead>
        <Table.Tbody>
          {(grants.data ?? []).map((x) => (
            <Table.Tr key={x.id}>
              <Table.Td>{spaceName(x.space_id)}</Table.Td>
              <Table.Td className="mono">{x.role === '*' ? 'every role' : x.role}</Table.Td>
              <Table.Td><Text size="xs" className="mono">{x.tools.length ? x.tools.join(', ') : 'all offered'}</Text></Table.Td>
              <Table.Td>{canWrite && <ActionIcon variant="subtle" aria-label={`Revoke the grant to ${x.role} in ${spaceName(x.space_id)}`} onClick={() => void revoke(x.id)}><IconTrash size={14} /></ActionIcon>}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {grants.isSuccess && grants.data.length === 0 && <Text size="sm" c="dimmed">Not granted to any space yet.</Text>}
      {canWrite && (
        <form onSubmit={(e) => { e.preventDefault(); void grantForm.handleSubmit() }}>
          <Group align="flex-end" gap="xs">
            <grantForm.Field name="space_id">
              {(f) => <Select label="Space" placeholder="Choose a space" data={(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))} value={f.state.value || null}
                onChange={(x) => f.handleChange(x ?? '')} />}
            </grantForm.Field>
            <grantForm.Field name="role">
              {(f) => <TextInput label="Space role" description="* for every role" classNames={{ input: 'mono' }} w={140} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </grantForm.Field>
            <grantForm.Field name="tools">
              {(f) => <MultiSelect label="Tools" placeholder="All offered" data={toolOptions} value={f.state.value} onChange={(tools) => f.handleChange(tools)} searchable w={220} />}
            </grantForm.Field>
            <grantForm.Subscribe selector={(st) => [st.values.space_id, st.isSubmitting] as const}>
              {([space, busy]) => <Button type="submit" disabled={!space} loading={busy}>Grant</Button>}
            </grantForm.Subscribe>
          </Group>
        </form>
      )}
    </Stack>
  )
}
