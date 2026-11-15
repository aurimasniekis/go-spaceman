import { Alert, Badge, Button, Checkbox, Group, Modal, NumberInput, SimpleGrid, Stack, Text } from '@mantine/core'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo, useState, type ReactNode } from 'react'
import { api, check, ifMatch, unwrap, type McpServer } from '@/api/client'
import { keys, useSpaces, useSpacemanTools, useUsers } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage } from '@/components/ErrorAlert'
import { datetime } from '@/lib/format'
import { notifyOk } from '@/lib/notify'
import { classColor, effectiveTools, expiryDays, grantsParam, groupByTag } from './spacemanMcp'

const stateText: Record<string, string> = { active: 'active', revoked: 'token revoked', expired: 'token expired', owner_disabled: 'owner disabled' }

type Action = 'reissue' | 'toggle' | 'delete'

/**
 * A SpaceMan MCP server's details (ADR-0063): owner, internal token, effective tools, and Re-issue, Disable and Delete,
 * each behind a confirmation. `grants` is the space-grant section the page renders below.
 */
export function SpacemanMcpDetail({ server, canWrite, onDeleted, grants }: { server: McpServer; canWrite: boolean; onDeleted: () => void; grants?: ReactNode }) {
  const sm = server.spaceman
  const users = useUsers()
  const spaces = useSpaces()
  const tokenGrants = sm?.grants ?? []
  const tools = useSpacemanTools(sm?.preset ?? 'admin', sm?.preset === 'custom' ? (sm?.tools ?? []) : [], grantsParam(tokenGrants), sm !== undefined)
  const effective = useMemo(() => groupByTag(effectiveTools(tools.data?.items ?? [])), [tools.data])
  const [action, setAction] = useState<Action | null>(null)
  if (!sm) return null
  const owner = users.data?.find((u) => u.id === sm.owner_user_id)
  const domain = (d: string) => (d === '*' ? 'every space' : (spaces.data?.find((s) => s.id === d)?.name ?? d))
  const count = tools.data?.count ?? 0
  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">{server.description || 'SpaceMan’s own API as tools for the granted space roles’ control and worker runs.'}</Text>
      {sm.token_state !== 'active' && (
        <Alert color="owlRed" variant="light" title={`Tools refused: ${stateText[sm.token_state]}`}>Runs see no tools and calls are refused. Re-issue the server to give it a new token.</Alert>
      )}
      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="xs">
        <Field label="Owner">{owner ? `${owner.display_name}${owner.email ? ` (${owner.email})` : ''}` : <span className="mono">{sm.owner_user_id}</span>}</Field>
        <Field label="Token">
          <Group gap={6} wrap="wrap">
            <AnchorLink to="/tokens" size="sm" className="mono">{sm.token_id}</AnchorLink>
            <Badge size="xs" color={sm.token_state === 'active' ? 'owlGreen' : 'owlRed'} variant="light">{stateText[sm.token_state]}</Badge>
          </Group>
          <Text size="xs" c="dimmed">Internal: it has no secret and can’t be copied or used as a bearer token.</Text>
        </Field>
        <Field label="Grants">
          <Group gap={4}>{tokenGrants.map((g) => <Badge key={`${g.role}@${g.domain}`} color={g.role === 'admin' && g.domain === '*' ? 'owlRed' : 'gray'} variant="light">{g.role} · {domain(g.domain)}</Badge>)}</Group>
        </Field>
        <Field label="Expires">{sm.never_expires ? 'never' : datetime(sm.expires_at)}</Field>
        <Field label="Tools">{sm.preset ?? 'admin'} preset{sm.tools?.length ? ` + ${sm.tools.length} named` : ''}</Field>
        <Field label="Approvals">Dangerous: always{sm.approval === 'writes' ? ' · writes: always' : ' · writes: no'}</Field>
      </SimpleGrid>
      {canWrite && (
        <Group gap="xs">
          <Button variant="default" onClick={() => setAction('reissue')}>Re-issue…</Button>
          <Button variant="default" onClick={() => setAction('toggle')}>{server.enabled ? 'Disable…' : 'Enable…'}</Button>
          <Button variant="subtle" color="owlRed" onClick={() => setAction('delete')}>Delete…</Button>
        </Group>
      )}
      <Stack gap={4}>
        <Group justify="space-between">
          <Text fw={600}>Effective tools</Text>
          <Badge variant="light" data-testid="effective-count">{count} {count === 1 ? 'tool' : 'tools'}</Badge>
        </Group>
        <Text size="xs" c="dimmed">What runs of a granted role see, as <span className="mono">{server.slug}__tool</span>: the preset and named tools the token’s grants can reach.</Text>
        {effective.map((g) => (
          <Stack key={g.tag} gap={2}>
            <Text size="xs" fw={600} c="dimmed" mt={4}>{g.tag}</Text>
            <Group gap={4}>
              {g.tools.map((t) => (
                <Badge key={t.name} size="sm" variant={t.class === 'read' ? 'outline' : 'light'} color={classColor[t.class]} tt="none" className="mono" title={t.summary}>
                  {t.name}{t.class === 'dangerous' ? ' · approval' : ''}
                </Badge>
              ))}
            </Group>
          </Stack>
        ))}
      </Stack>
      {grants}
      {action && <ConfirmAction server={server} action={action} onClose={() => setAction(null)} onDeleted={onDeleted} />}
    </Stack>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm" component="div">{children}</Text>
    </Stack>
  )
}

const titles: Record<Action, string> = { reissue: 'Re-issue the token?', toggle: '', delete: 'Delete this SpaceMan MCP server?' }

function ConfirmAction({ server, action, onClose, onDeleted }: { server: McpServer; action: Action; onClose: () => void; onDeleted: () => void }) {
  const qc = useQueryClient()
  const sm = server.spaceman
  const [never, setNever] = useState(sm?.never_expires ?? true)
  const [days, setDays] = useState(90)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (!sm) return null
  const run = async () => {
    setBusy(true)
    setError('')
    try {
      const path = { params: { path: { server_id: server.id }, header: { 'If-Match': ifMatch(server.version) } } }
      if (action === 'reissue') {
        unwrap(await api.POST('/mcp-servers/{server_id}/reissue', { ...path, body: { grants: sm.grants, expires_in_days: expiryDays(never, days) } }))
        notifyOk(`${server.name} re-issued`)
      } else if (action === 'toggle') {
        unwrap(await api.PUT('/mcp-servers/{server_id}/spaceman', {
          ...path,
          body: { name: server.name, description: server.description, preset: sm.preset, tools: sm.tools, approval: sm.approval, call_timeout_ms: server.call_timeout_ms,
            max_concurrency: server.max_concurrency, max_result_bytes: server.max_result_bytes, enabled: !server.enabled },
        }))
        notifyOk(`${server.name} ${server.enabled ? 'disabled' : 'enabled'}`)
      } else {
        check(await api.DELETE('/mcp-servers/{server_id}', { params: { path: { server_id: server.id } } }))
        notifyOk(`${server.name} deleted`)
      }
      void qc.invalidateQueries({ queryKey: keys.mcpServers })
      void qc.invalidateQueries({ queryKey: keys.tokens })
      void qc.invalidateQueries({ queryKey: ['space-mcp'] })
      onClose()
      if (action === 'delete') onDeleted()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  const title = action === 'toggle' ? (server.enabled ? 'Disable this SpaceMan MCP server?' : 'Enable this SpaceMan MCP server?') : titles[action]
  return (
    <Modal opened onClose={onClose} title={title}>
      <Stack gap="sm">
        {action === 'reissue' && (
          <>
            <Text size="sm">The current token is revoked and a new one, owned by you, takes its place with the same grants. Runs keep their tools; a step-up may be asked first.</Text>
            <Group align="flex-end">
              <Checkbox label="Never expires" checked={never} onChange={(e) => setNever(e.currentTarget.checked)} mb={6} />
              <NumberInput label="Expires in (days)" min={1} max={366} w={160} disabled={never} value={days} onChange={(v) => setDays(Number(v) || 1)} />
            </Group>
          </>
        )}
        {action === 'toggle' && (
          <Text size="sm">{server.enabled ? 'Every call is refused and new runs don’t see the tools until it is enabled again. The token stays.' : 'Granted runs see the tools again on their next launch.'}</Text>
        )}
        {action === 'delete' && <Text size="sm">The server and its grants are removed and its internal token is revoked at once: later calls are refused.</Text>}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={action === 'reissue' ? undefined : 'owlRed'} loading={busy} onClick={() => void run()}>
            {action === 'reissue' ? 'Re-issue' : action === 'delete' ? 'Delete' : server.enabled ? 'Disable' : 'Enable'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
