import { Alert, Badge, Button, Checkbox, Card, Group, Modal, NumberInput, SegmentedControl, Stack, TagsInput, Text, TextInput } from '@mantine/core'
import { IconAlertTriangle, IconSettings } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { ApiError, api, ifMatch, unwrap, type SpaceHarborPilot } from '@/api/client'
import { keys, useMe, useSpaceHarborPilot } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { atLeast, roleIn } from '@/lib/access'
import { notifyOk } from '@/lib/notify'
import { durationLabel, harnessLabel, ttlError } from './format'

/** A space's Harbor Pilot opt-in (ADR-0060): which harnesses route their runs through the gateway, and how. */
export function SpaceHarborPilotCard({ spaceId }: { spaceId: string }) {
  const settings = useSpaceHarborPilot(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [editing, setEditing] = useState(false)
  const s = settings.data
  if (!s) return null
  return (
    <Card mb="md" data-testid="space-harbor-pilot">
      <Group justify="space-between" mb="xs">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Harbor Pilot</Text>
        {canWrite && <Button size="xs" variant="default" leftSection={<IconSettings size={14} />} onClick={() => setEditing(true)}>Settings</Button>}
      </Group>
      {s.enabled ? (
        <Stack gap={4}>
          <Group gap="xs">
            <Badge color="owlBlue" variant="light">Enabled</Badge>
            {s.harnesses.map((h) => <Badge key={h} color="gray" variant="light">{harnessLabel(h)}</Badge>)}
          </Group>
          <Text size="sm">
            Project <span className="mono">{s.project_id}</span> · run tokens last {durationLabel(s.ttl_seconds)} ·{' '}
            {s.on_budget_status_unavailable === 'refuse' ? 'launches are refused' : 'launches proceed'} when budget status is unavailable
            {s.allowed_model_aliases.length > 0 && <> · models {s.allowed_model_aliases.join(', ')}</>}
          </Text>
          <Text size="xs" c="dimmed">Runs of these harnesses use Harbor Pilot instead of the space&apos;s accounts; there is no fallback to an account.</Text>
        </Stack>
      ) : (
        <Text size="sm" c="dimmed">
          Not enabled: runs use the space&apos;s accounts.{' '}
          {!s.connected && 'Harbor Pilot is not connected yet (an administrator connects it under Integrations).'}
        </Text>
      )}
      {s.warnings.map((w) => (
        <Alert key={w} color="owlYellow" icon={<IconAlertTriangle size={16} />} p="xs" mt="xs"><Text size="sm">{w}</Text></Alert>
      ))}
      {editing && <SettingsModal spaceId={spaceId} current={s} onClose={() => setEditing(false)} />}
    </Card>
  )
}

const KNOWN_HARNESSES = ['claude-code', 'codex']

function SettingsModal({ spaceId, current, onClose }: { spaceId: string; current: SpaceHarborPilot; onClose: () => void }) {
  const qc = useQueryClient()
  // The project decides who pays: only a global administrator (account:write) sets or changes it (ADR-0060 decision 2).
  const canSetProject = roleIn(useMe().data, '*') === 'admin'
  const [enabled, setEnabled] = useState(current.enabled)
  const [project, setProject] = useState(current.project_id)
  const [harnesses, setHarnesses] = useState<string[]>(current.harnesses)
  const [ttl, setTTL] = useState<number>(current.ttl_seconds)
  const [policy, setPolicy] = useState(current.on_budget_status_unavailable)
  const [aliases, setAliases] = useState<string[]>(current.allowed_model_aliases)
  const [error, setError] = useState('')
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setError('')
    setFields({})
    setBusy(true)
    try {
      unwrap(await api.PUT('/spaces/{space_id}/harbor-pilot', {
        params: { path: { space_id: spaceId }, header: current.stored ? { 'If-Match': ifMatch(current.version) } : {} },
        body: { enabled, project_id: project.trim() || undefined, harnesses, ttl_seconds: ttl, on_budget_status_unavailable: policy, allowed_model_aliases: aliases },
      }))
      void qc.invalidateQueries({ queryKey: keys.spaceHarborPilot(spaceId) })
      notifyOk('Harbor Pilot settings saved')
      onClose()
    } catch (e) {
      if (e instanceof ApiError) setFields(e.fields)
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  const other = harnesses.filter((h) => !KNOWN_HARNESSES.includes(h))
  return (
    <Modal opened onClose={onClose} title="Harbor Pilot for this space" size="lg">
      <Stack gap="sm">
        <Checkbox label="Route runs through Harbor Pilot" checked={enabled} onChange={(e) => setEnabled(e.currentTarget.checked)}
          description="Each run gets its own Harbor Pilot token instead of an account; a run is refused when Harbor Pilot can't issue one" />
        <TextInput label="Harbor Pilot project id" placeholder={canSetProject ? 'prj_…' : 'Not set'} value={project} onChange={(e) => setProject(e.currentTarget.value)}
          error={fields.project_id} readOnly={!canSetProject} classNames={{ input: 'mono' }} data-testid="hp-project"
          description={canSetProject
            ? "Runs are charged to it; it must be the connection's binding project or one of its allowed projects"
            : 'Runs are charged to it. Only an administrator sets or changes the project, since it decides who pays'} />
        <Stack gap={4}>
          <Text size="sm" fw={500}>Harnesses</Text>
          <Group gap="md">
            {KNOWN_HARNESSES.map((h) => (
              <Checkbox key={h} label={harnessLabel(h)} checked={harnesses.includes(h)}
                onChange={(e) => setHarnesses(e.currentTarget.checked ? [...harnesses, h] : harnesses.filter((x) => x !== h))} />
            ))}
          </Group>
          <TagsInput label="Other harnesses" description="Any other harness kind Harbor Pilot serves" value={other} error={fields.harnesses}
            onChange={(v) => setHarnesses([...harnesses.filter((h) => KNOWN_HARNESSES.includes(h)), ...v])} />
          <Text size="xs" c="dimmed">Harnesses not listed keep the space&apos;s accounts. None checked means Claude Code and Codex.</Text>
        </Stack>
        <NumberInput label="Run token lifetime (seconds)" min={900} max={604800} step={3600} value={ttl} onChange={(v) => setTTL(Number(v) || 0)}
          description={`${durationLabel(ttl)}: 15 minutes to 7 days. A run that outlives its token loses model access.`} error={fields.ttl_seconds ?? ttlError(ttl)} />
        <Stack gap={4}>
          <Text size="sm" fw={500}>When Harbor Pilot budget status can&apos;t be read</Text>
          <SegmentedControl value={policy} onChange={(v) => setPolicy(v === 'refuse' ? 'refuse' : 'proceed')} data={[{ value: 'proceed', label: 'Launch anyway' }, { value: 'refuse', label: 'Refuse the launch' }]} />
          <Text size="xs" c="dimmed">Harbor Pilot still enforces its budgets on every request.</Text>
        </Stack>
        <TagsInput label="Allowed models" description="Model aliases runs may use; empty allows any model the binding's plan routes" value={aliases} onChange={setAliases}
          error={fields.allowed_model_aliases} placeholder="coding/default" />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>Save</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
