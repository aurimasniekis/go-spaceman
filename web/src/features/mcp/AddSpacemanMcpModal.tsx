import { Accordion, Alert, Badge, Button, Checkbox, Group, Modal, NumberInput, Radio, ScrollArea, SegmentedControl, Select, SimpleGrid, Stack, Switch, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconShieldLock } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { api, idempotencyKey, unwrap, type GrantInput, type McpServer } from '@/api/client'
import { keys, useMe, useSpaces, useSpacemanTools } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { GrantsEditor } from '@/components/GrantsEditor'
import { useNarrow } from '@/lib/media'
import { notifyOk } from '@/lib/notify'
import {
  classColor, covers, DEFAULT_NAME, DEFAULT_ROLE, DEFAULT_SLUG, expiryDays, grantsParam, groupByTag, initialScope, PRESETS, SCOPES, scopeBlocked,
  scopeGrants, slugError, type Preset, type ScopeId,
} from './spacemanMcp'

/**
 * "Add SpaceMan MCP" (ADR-0063): a SpaceMan MCP server with its internal token, granted to a space role. Creating needs
 * mcp:write and a step-up; the API prompts for it (ReauthModal) and the request is retried once.
 */
export function AddSpacemanMcpModal({ opened, onClose, spaceId, onCreated }: { opened: boolean; onClose: () => void; spaceId?: string; onCreated?: (s: McpServer) => void }) {
  const narrow = useNarrow()
  return (
    <Modal opened={opened} onClose={onClose} title="Add SpaceMan MCP" size="xl" fullScreen={narrow}>
      {opened && <AddSpacemanMcpForm spaceId={spaceId ?? ''} fixedSpace={spaceId !== undefined} onClose={onClose} onCreated={onCreated} />}
    </Modal>
  )
}

function AddSpacemanMcpForm({ spaceId: initialSpace, fixedSpace, onClose, onCreated }: { spaceId: string; fixedSpace: boolean; onClose: () => void; onCreated?: (s: McpServer) => void }) {
  const me = useMe().data
  const narrow = useNarrow()
  const spaces = useSpaces()
  const qc = useQueryClient()
  const [name, setName] = useState(DEFAULT_NAME)
  const [slug, setSlug] = useState(DEFAULT_SLUG)
  const [space, setSpace] = useState(initialSpace)
  const [role, setRole] = useState(DEFAULT_ROLE)
  const [scope, setScopeId] = useState<ScopeId>(() => initialScope(me, initialSpace))
  const [customGrants, setCustomGrants] = useState<GrantInput[]>(initialSpace ? [{ role: 'viewer', domain: initialSpace }] : [{ role: 'viewer', domain: '*' }])
  const [preset, setPreset] = useState<Preset>(() => SCOPES.find((s) => s.id === scope)?.preset ?? 'admin')
  const [picked, setPicked] = useState<string[]>([])
  const [gateWrites, setGateWrites] = useState(false)
  const [never, setNever] = useState(true)
  const [days, setDays] = useState(90)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [key] = useState(idempotencyKey)

  const grants = scope === 'custom' ? customGrants : scopeGrants(scope, space)
  const grantsOK = grants.length > 0 && covers(me, grants)
  const tools = useSpacemanTools(preset, preset === 'custom' ? picked : [], grantsParam(grants), grants.length > 0)
  const groups = useMemo(() => groupByTag(tools.data?.items ?? []), [tools.data])
  const count = tools.data?.count ?? 0
  const slugProblem = slugError(slug)
  const chooseScope = (id: ScopeId) => {
    setScopeId(id)
    const p = SCOPES.find((s) => s.id === id)?.preset
    if (p && preset !== 'custom') setPreset(p)
  }
  const chooseSpace = (id: string) => {
    setSpace(id)
    const cur = SCOPES.find((s) => s.id === scope)
    if (cur && scopeBlocked(me, cur, id)) setScopeId(initialScope(me, id))
  }
  const toggle = (tool: string, on: boolean) => setPicked((p) => (on ? [...p, tool].sort() : p.filter((x) => x !== tool)))

  const submit = async () => {
    setError('')
    setBusy(true)
    try {
      const srv = unwrap(await api.POST('/mcp-servers/spaceman', {
        params: { header: { 'Idempotency-Key': key } },
        body: { slug, name, preset, tools: preset === 'custom' ? picked : [], approval: gateWrites ? 'writes' : 'none', grants, expires_in_days: expiryDays(never, days) },
      }))
      if (space && role) {
        try {
          unwrap(await api.POST('/mcp-servers/{server_id}/grants', { params: { path: { server_id: srv.id } }, body: { space_id: space, role, tools: [] } }))
        } catch (e) {
          setError(`The server was created, but granting it failed: ${errorMessage(e)}. Grant it from its details.`)
        }
      }
      void qc.invalidateQueries({ queryKey: keys.mcpServers })
      void qc.invalidateQueries({ queryKey: keys.tokens })
      if (space) void qc.invalidateQueries({ queryKey: keys.spaceMcp(space) })
      notifyOk(`${srv.name} added`)
      onCreated?.(srv)
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  const spaceName = spaces.data?.find((s) => s.id === space)?.name
  return (
    <form onSubmit={(e) => { e.preventDefault(); void submit() }}>
      <Stack gap="md">
        <Text size="sm" c="dimmed">
          SpaceMan’s own API as tools for a space’s control and worker runs. The server acts through an internal token you own, with no secret;
          revoking it, disabling you or deleting the server cuts access on the next call.
        </Text>

        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <TextInput label="Name" required value={name} onChange={(e) => setName(e.currentTarget.value)} />
          <TextInput label="Slug" description="Tools appear as slug__tool; “spaceman” is reserved" classNames={{ input: 'mono' }} required value={slug}
            error={slug && slugProblem} onChange={(e) => setSlug(e.currentTarget.value.trim())} />
        </SimpleGrid>

        <Stack gap={4}>
          <Text size="sm" fw={600}>Grant</Text>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Select label="Space" placeholder="Grant later" clearable={!fixedSpace} disabled={fixedSpace} searchable
              data={(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))} value={space || null} onChange={(v) => chooseSpace(v ?? '')} />
            <TextInput label="Space role" description="Runs of this role see the tools; * for every role" classNames={{ input: 'mono' }} value={role}
              disabled={!space} onChange={(e) => setRole(e.currentTarget.value.trim())} />
          </SimpleGrid>
        </Stack>

        <Radio.Group label="Scope" description="What the token may do. Options beyond your own grants are disabled." value={scope} onChange={(v) => chooseScope(v as ScopeId)}>
          <Stack gap={6} mt={6}>
            {SCOPES.map((s) => {
              const blocked = scopeBlocked(me, s, space)
              return (
                <Radio key={s.id} value={s.id} disabled={blocked !== null} data-testid={`scope-${s.id}`}
                  label={<Group gap={6}>{s.label}{s.space && spaceName && <Text span size="xs" c="dimmed">({spaceName})</Text>}{blocked && <Badge size="xs" color="gray" variant="light">{blocked}</Badge>}</Group>}
                  description={s.description} color={s.danger ? 'owlRed' : undefined} />
              )
            })}
          </Stack>
        </Radio.Group>
        {scope === 'custom' && <GrantsEditor value={customGrants} onChange={setCustomGrants} />}
        {scope === 'admin-all' && (
          <Alert color="owlRed" variant="light" icon={<IconAlertTriangle size={16} />} title="Full administration">
            Agents with these tools can do what you can through the API, except managing users, tokens, approvals and SpaceMan MCP servers.
            Dangerous calls (deletes, secrets, profiles, node changes) always wait for a person’s approval.
          </Alert>
        )}
        {!grantsOK && grants.length > 0 && <Text size="sm" c="owlRed" role="alert">These grants reach beyond your own; the server can’t be created with them.</Text>}

        <Stack gap={6}>
          <Group justify="space-between" wrap="nowrap">
            <Text size="sm" fw={600}>Tools</Text>
            <Badge variant="light" data-testid="tool-count">{tools.isFetching && !tools.data ? '…' : `${count} ${count === 1 ? 'tool' : 'tools'}`}</Badge>
          </Group>
          <SegmentedControl fullWidth={narrow} orientation={narrow ? 'vertical' : 'horizontal'} data={PRESETS.map((p) => ({ value: p.value, label: p.label }))} value={preset}
            onChange={(v) => setPreset(v as Preset)} aria-label="Tool preset" />
          <Text size="xs" c="dimmed">{PRESETS.find((p) => p.value === preset)?.description}. Tools the grants can never reach are left out of the count.</Text>
          {preset === 'custom' && (
            <ScrollArea.Autosize mah={narrow ? undefined : 320} type="auto">
              <Accordion multiple variant="contained" chevronPosition="left">
                {groups.map((g) => {
                  const on = g.tools.filter((t) => picked.includes(t.name)).length
                  return (
                    <Accordion.Item key={g.tag} value={g.tag}>
                      <Accordion.Control><Group gap={6}>{g.tag}<Text span size="xs" c="dimmed">{on}/{g.tools.length}</Text></Group></Accordion.Control>
                      <Accordion.Panel>
                        <Stack gap={4}>
                          {g.tools.map((t) => (
                            <Checkbox key={t.name} checked={picked.includes(t.name)} onChange={(e) => toggle(t.name, e.currentTarget.checked)} disabled={!t.reachable && !picked.includes(t.name)}
                              aria-label={t.name}
                              label={
                                <Group gap={6} wrap="wrap">
                                  <Text span size="xs" className="mono">{t.name}</Text>
                                  {t.class !== 'read' && <Badge size="xs" variant="light" color={classColor[t.class]}>{t.class === 'dangerous' ? 'dangerous · approval' : t.class}</Badge>}
                                  {!t.reachable && <Badge size="xs" variant="outline" color="gray">beyond grants</Badge>}
                                </Group>
                              }
                              description={t.summary} />
                          ))}
                        </Stack>
                      </Accordion.Panel>
                    </Accordion.Item>
                  )
                })}
              </Accordion>
            </ScrollArea.Autosize>
          )}
        </Stack>

        <Stack gap={6}>
          <Text size="sm" fw={600}>Approvals</Text>
          <Checkbox checked disabled label="Dangerous tools: always" description="Every dangerous call waits for a person’s approval. This can’t be turned off." />
          <Switch checked={gateWrites} onChange={(e) => setGateWrites(e.currentTarget.checked)} label="Writes need approval too" description="Off: write tools run without asking." />
        </Stack>

        <Group align="flex-end">
          <Checkbox label="Never expires" checked={never} onChange={(e) => setNever(e.currentTarget.checked)} mb={6} />
          <NumberInput label="Expires in (days)" min={1} max={366} w={160} disabled={never} value={days} onChange={(v) => setDays(Number(v) || 1)} />
        </Group>

        <Group gap={6} wrap="nowrap" align="flex-start">
          <IconShieldLock size={16} style={{ flexShrink: 0, marginTop: 2 }} />
          <Text size="xs" c="dimmed">Adding the server needs a recent sign-in: you may be asked for your password or second factor first.</Text>
        </Group>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button type="submit" loading={busy} disabled={!name || slugProblem !== null || !grantsOK || (preset === 'custom' && picked.length === 0)}>Add server</Button>
        </Group>
      </Stack>
    </form>
  )
}
