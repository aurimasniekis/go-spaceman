import { Alert, Badge, Button, Card, Checkbox, Code, Group, Modal, PasswordInput, SimpleGrid, Stack, Switch, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconInfoCircle, IconPlugConnected } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { ApiError, api, check, ifMatch, unwrap, type HarborPilotConnection } from '@/api/client'
import { keys, useHarborPilot, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/harbor-pilot')({ component: HarborPilotPage })

/**
 * The Harbor Pilot gateway connection (ADR-0060): one per installation. Its tokens are write-only: the page shows only
 * whether each is set and how many times it was replaced.
 */
function HarborPilotPage() {
  const conn = useHarborPilot()
  const [editing, setEditing] = useState(false)
  const c = conn.data
  return (
    <>
      <PageHeader title="Harbor Pilot" description="The LLM gateway runs of opted-in spaces use instead of direct accounts: one token per run, revoked when it ends."
        actions={conn.isSuccess && <Button leftSection={<IconPlugConnected size={14} />} onClick={() => setEditing(true)}>{c ? 'Replace connection' : 'Connect'}</Button>} />
      <Alert color="owlBlue" icon={<IconInfoCircle size={16} />} mb="md" p="xs">
        <Text size="sm">
          The parent token mints a short-lived token for each run of a space that opts in (on its overview). It needs gateway:invoke,
          gateway:models and tokens:mint on one Harbor Pilot client binding. SpaceMan keeps it sealed, never shows it again and never sends it
          to a node. Harbor Pilot 0.1.3 or later is required.
        </Text>
      </Alert>
      <ErrorAlert error={conn.error} />
      {conn.isSuccess && !c && (
        <Card><Text size="sm" c="dimmed" ta="center" p="md">Not connected. Connect Harbor Pilot to let spaces route their runs through it.</Text></Card>
      )}
      {c && <ConnectionCard c={c} />}
      {editing && <ConnectionModal current={c ?? null} onClose={() => setEditing(false)} />}
    </>
  )
}

function ConnectionCard({ c }: { c: HarborPilotConnection }) {
  const qc = useQueryClient()
  const spaces = useSpaces().data ?? []
  const spaceName = (id: string) => spaces.find((s) => s.id === id)?.name ?? id
  const remove = async () => {
    if (!window.confirm('Remove the Harbor Pilot connection? Its tokens are deleted from SpaceMan; revoke the parent credential in Harbor Pilot too.')) return
    try {
      check(await api.DELETE('/harbor-pilot/connection', { params: { header: { 'If-Match': ifMatch(c.version) } } }))
      void qc.invalidateQueries({ queryKey: keys.harborPilot })
      notifyOk('Connection removed')
    } catch (e) {
      notifyError(e, 'Could not remove the connection')
    }
  }
  return (
    <Stack gap="md">
      {c.warnings.map((w) => (
        <Alert key={w} color="owlYellow" icon={<IconAlertTriangle size={16} />} p="xs"><Text size="sm">{w}</Text></Alert>
      ))}
      <Card>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
          <Field label="Gateway URL"><Code>{c.base_url}</Code></Field>
          <Field label="URL runs use"><Code>{c.run_base_url || c.base_url}</Code>{!c.run_base_url && <Text size="xs" c="dimmed">same as the gateway URL</Text>}</Field>
          <Field label="Parent token">
            <Badge color="owlGreen" variant="light">set</Badge> <Text span size="xs" c="dimmed">version {c.parent_token_version}{c.parent_expires_at ? ` · expires ${datetime(c.parent_expires_at)}` : ' · no expiry'}</Text>
          </Field>
          <Field label="Service-principal token">
            {c.has_service_token ? <Badge color="owlGreen" variant="light">set</Badge> : <Badge color="gray" variant="light">not set</Badge>}{' '}
            <Text span size="xs" c="dimmed">version {c.service_token_version}</Text>
          </Field>
          <Field label="Client binding"><Text size="sm" className="mono">{c.binding_id || '—'}</Text></Field>
          <Field label="Validated"><Text size="sm">{ago(c.validated_at)}</Text>{c.allow_insecure_http && <Badge color="owlRed" variant="light" mt={4}>insecure HTTP allowed</Badge>}</Field>
        </SimpleGrid>
      </Card>
      <Card>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb="xs">Spaces using it</Text>
        {c.enabled_spaces.length === 0 ? <Text size="sm" c="dimmed">None yet. A space opts in on its overview.</Text> : (
          <Group gap="xs">
            {c.enabled_spaces.map((id) => (
              <AnchorLink key={id} to="/spaces/$spaceId" params={{ spaceId: id }} size="sm">{spaceName(id)}</AnchorLink>
            ))}
          </Group>
        )}
      </Card>
      <Group justify="flex-end">
        <Button variant="default" color="owlRed" onClick={() => void remove()} disabled={c.enabled_spaces.length > 0}
          title={c.enabled_spaces.length > 0 ? 'Disable Harbor Pilot in every space first' : undefined}>Remove connection…</Button>
      </Group>
    </Stack>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" fw={600}>{label}</Text>
      <div>{children}</div>
    </Stack>
  )
}

function ConnectionModal({ current, onClose }: { current: HarborPilotConnection | null; onClose: () => void }) {
  const qc = useQueryClient()
  const [baseURL, setBaseURL] = useState(current?.base_url ?? '')
  const [runURL, setRunURL] = useState(current?.run_base_url ?? '')
  const [insecure, setInsecure] = useState(current?.allow_insecure_http ?? false)
  const [parent, setParent] = useState('')
  const [service, setService] = useState('')
  const [removeService, setRemoveService] = useState(false)
  const [error, setError] = useState('')
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setError('')
    setFields({})
    setBusy(true)
    const body = { base_url: baseURL.trim(), run_base_url: runURL.trim() || undefined, allow_insecure_http: insecure || undefined,
      parent_token: parent || undefined, service_token: service || undefined, remove_service_token: removeService || undefined }
    try {
      unwrap(await api.PUT('/harbor-pilot/connection', { params: { header: current ? { 'If-Match': ifMatch(current.version) } : {} }, body }))
      void qc.invalidateQueries({ queryKey: keys.harborPilot })
      notifyOk(current ? 'Connection saved' : 'Harbor Pilot connected')
      onClose()
    } catch (e) {
      if (e instanceof ApiError) setFields(e.fields)
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened onClose={onClose} title={current ? 'Replace the Harbor Pilot connection' : 'Connect Harbor Pilot'} size="lg">
      <Stack gap="sm">
        <Text size="sm" c="dimmed">Saving checks the connection with Harbor Pilot: the parent token mints a probe token, which is revoked at once.</Text>
        <TextInput label="Gateway URL" placeholder="https://harbor-pilot.example.com" value={baseURL} onChange={(e) => setBaseURL(e.currentTarget.value)}
          error={fields.base_url} data-autofocus />
        <TextInput label="URL runs use" description="Only when runs reach the gateway on another address than SpaceMan does; it needs https"
          placeholder="Same as the gateway URL" value={runURL} onChange={(e) => setRunURL(e.currentTarget.value)} error={fields.run_base_url} />
        <PasswordInput label="Parent token" autoComplete="off" classNames={{ input: 'mono' }}
          description={current ? 'Leave empty to keep the current token' : 'An hpk_… gateway credential with tokens:mint'}
          value={parent} onChange={(e) => setParent(e.currentTarget.value)} error={fields.parent_token} />
        <PasswordInput label="Service-principal token" autoComplete="off" classNames={{ input: 'mono' }}
          description={current?.has_service_token ? 'Leave empty to keep the current token' : 'Optional: an hpa_… token with usage.read and config.read, for budget status and project checks'}
          value={service} onChange={(e) => setService(e.currentTarget.value)} error={fields.service_token} disabled={removeService} />
        {current?.has_service_token && (
          <Checkbox label="Remove the service-principal token" checked={removeService} onChange={(e) => setRemoveService(e.currentTarget.checked)} />
        )}
        <Switch label="Allow plain HTTP" description="Tests and trusted local networks only: every token then crosses the network in clear text"
          checked={insecure} onChange={(e) => setInsecure(e.currentTarget.checked)} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>{current ? 'Save' : 'Connect'}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
