import { Alert, Button, Card, Group, NumberInput, Select, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Limit } from '@/api/client'
import { keys, useLimits, useMe } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { limitValue } from '@/features/limits/format'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'

/** The limits that steer account switching (ADR-0090), in the order the card shows them. */
export const FAILOVER_KEYS = ['usage_limit_failover', 'usage_limit_wait_if_reset_within', 'usage_limit_switch_max_per_day', 'usage_limit_min_headroom_percent'] as const

type Draft = Record<string, number | ''>

/** The overrides to send: every override the space already has, with the edited fields replaced (empty = default). */
export function overridesWith(all: Limit[], draft: Draft): Record<string, number> {
  const out: Record<string, number> = {}
  for (const l of all) if (l.override != null) out[l.key] = l.override
  for (const [k, v] of Object.entries(draft)) {
    if (v === '') delete out[k]
    else out[k] = v
  }
  return out
}

/**
 * Failover and the two thresholds around it: how close a reset must be for a run to wait instead of moving (Q4), and
 * the launch headroom gate (Q5). They are space limits, so this is the same override the Limits page edits.
 */
export function FailoverSettingsCard({ spaceId }: { spaceId: string }) {
  const qc = useQueryClient()
  const limits = useLimits(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const [draft, setDraft] = useState<Draft>({})
  const [busy, setBusy] = useState(false)
  const l = limits.data
  const shown = FAILOVER_KEYS.map((k) => l?.limits.find((x) => x.key === k)).filter((x): x is Limit => !!x)
  const current = (x: Limit): number | '' => (x.key in draft ? draft[x.key] : (x.override ?? ''))
  const dirty = Object.keys(draft).length > 0
  async function save() {
    if (!l) return
    setBusy(true)
    try {
      const body = { values: overridesWith(l.limits, draft), on_lower: l.on_lower }
      const header = l.version > 0 ? { 'If-Match': ifMatch(l.version) } : {}
      const next = unwrap(await api.PUT('/spaces/{space_id}/limits', { params: { path: { space_id: spaceId }, header }, body }))
      qc.setQueryData(keys.limits(spaceId), next)
      setDraft({})
      notifyOk('Account switching settings saved')
    } catch (e) {
      notifyError(e, 'Could not save the settings')
    } finally {
      setBusy(false)
    }
  }
  const set = (k: string, v: number | '') => setDraft((d) => ({ ...d, [k]: v }))
  const field = (x: Limit) => {
    const label = x.key === 'usage_limit_wait_if_reset_within' ? 'Wait instead of switching when the reset is within (minutes)'
      : x.key === 'usage_limit_min_headroom_percent' ? 'Launch headroom: skip an account with less than this much free (%)' : x.label
    if (x.unit === 'switch') {
      return (
        <Select key={x.key} label={x.label} description={x.scope} allowDeselect={false} disabled={!canWrite} value={String(current(x))}
          data={[{ value: '', label: `Default (${limitValue(x.default, x.unit)})` }, { value: '1', label: 'On' }, { value: '0', label: 'Off' }]}
          onChange={(v) => set(x.key, v === '1' ? 1 : v === '0' ? 0 : '')} />
      )
    }
    return (
      <NumberInput key={x.key} label={label} description={x.scope} disabled={!canWrite} min={x.min} max={x.max} allowDecimal={false} value={current(x)}
        placeholder={`Default: ${limitValue(x.default, x.unit, x.zero_means_none)}`} onChange={(v) => set(x.key, typeof v === 'number' ? v : '')} />
    )
  }
  return (
    <Card data-testid="failover-settings">
      <Title order={5} mb={4}>Account switching</Title>
      <Text size="sm" c="dimmed" mb="sm">Leave a field empty to use the installation default. Changes apply to limits reached and launches made from then on; running agents are untouched.</Text>
      <ErrorAlert error={limits.error} />
      <Stack gap="sm">
        <SimpleGrid cols={{ base: 1, md: 2 }}>{shown.map(field)}</SimpleGrid>
        <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />}>
          <Text size="sm">The headroom gate only looks at windows whose usage is known. An account with no report is never refused, and a running agent is never moved because of headroom.</Text>
        </Alert>
        {canWrite && (
          <Group justify="flex-end">
            <Button variant="default" disabled={!dirty} onClick={() => setDraft({})}>Reset</Button>
            <Button disabled={!dirty} loading={busy} onClick={() => void save()}>Save</Button>
          </Group>
        )}
      </Stack>
    </Card>
  )
}
