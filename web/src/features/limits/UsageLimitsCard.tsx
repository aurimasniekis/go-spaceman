import { Badge, Button, Card, Group, Modal, Paper, Stack, Text, Title, Tooltip } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type Schemas } from '@/api/client'
import { keys, useLimitWaits, useMe, useSpaceAccounts, useUsageLimits } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { kindLabel } from '@/features/runs/runInfo'
import { atLeast, roleIn } from '@/lib/access'
import { closeButtonProps, notifyOk } from '@/lib/notify'
import { LimitTime } from './LimitTime'
import { ACCOUNT_KINDS, clearedBy, holdTarget, isActiveWait, isOpen, limitKindLabel, waitReason, waitState, type LimitWait, type UsageLimit } from './usageLimit'

type AccountKind = NonNullable<Schemas['Clear-account-usage-limitsRequest']['kind']>

/** How many ended holds and waits are kept on the page, newest first. */
const RECENT = 5

const SOURCE: Record<string, string> = { transcript: 'the run transcript', quota_board: 'the gateway quota board', budget_status: 'the gateway budget', operator: 'an operator' }

type Pending =
  | { what: 'clear'; hold: UsageLimit; label: string }
  | { what: 'clear-account'; hold: UsageLimit; label: string }
  | { what: 'release'; wait: LimitWait }
  | null

/**
 * Usage limits on the Limits page (ADR-0079): the accounts and gateway references held at a usage limit, and the runs
 * parked until a reset, each with until when (the viewer's zone, UTC in the tooltip). Operators resume a wait now,
 * release it (its task goes back to the queue) or clear a hold; admins of every account clear an account's holds in
 * every space. Other spaces' holds on a shared account show without their details (shared).
 */
export function UsageLimitsCard({ spaceId }: { spaceId: string }) {
  const qc = useQueryClient()
  const holds = useUsageLimits(spaceId)
  const waits = useLimitWaits(spaceId)
  const accounts = useSpaceAccounts(spaceId)
  const me = useMe().data
  const canControl = atLeast(roleIn(me, spaceId), 'operator') // run:control
  const canAccounts = atLeast(roleIn(me, '*'), 'admin') // account:write is global
  const [pending, setPending] = useState<Pending>(null)
  const [busy, setBusy] = useState<string>('')
  const [error, setError] = useState('')

  const accountName = (id: string) => accounts.data?.find((a) => a.account_id === id)?.name
  const all = holds.data ?? []
  const open = all.filter(isOpen)
  const cleared = all.filter((u) => !isOpen(u)).slice(0, RECENT)
  const ws = waits.data ?? []
  const active = ws.filter(isActiveWait)
  const ended = ws.filter((w) => !isActiveWait(w)).sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at)).slice(0, RECENT)

  // A changed wait or hold replaces its row; the change feed refreshes the rest.
  const putWait = (w: LimitWait) => qc.setQueryData<LimitWait[]>(keys.limitWaits(spaceId), (old) => old?.map((x) => (x.id === w.id ? w : x)))
  const putHolds = (cs: UsageLimit[]) =>
    qc.setQueryData<UsageLimit[]>(keys.usageLimits(spaceId), (old) => old?.map((x) => cs.find((c) => c.id && c.id === x.id) ?? x))

  async function act(key: string, fn: () => Promise<void>) {
    setBusy(key)
    setError('')
    try {
      await fn()
      setPending(null)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy('')
    }
  }

  const resume = (w: LimitWait) =>
    act(`resume:${w.id}`, async () => {
      const next = unwrap(await api.POST('/spaces/{space_id}/limit-waits/{wait_id}/resume', { params: { path: { space_id: spaceId, wait_id: w.id } } }))
      putWait(next)
      notifyOk(next.new_run_id ? `Resumed ${w.role} as a new run` : `Resume of ${w.role} requested`)
    })
  const release = (w: LimitWait) =>
    act(`release:${w.id}`, async () => {
      putWait(unwrap(await api.POST('/spaces/{space_id}/limit-waits/{wait_id}/release', { params: { path: { space_id: spaceId, wait_id: w.id } } })))
      notifyOk(w.task_id ? `Released ${w.role}: its task is back in the queue` : `Released ${w.role}`)
    })
  const clearHeld = (res: { cleared: UsageLimit[]; held_elsewhere: boolean; note?: string }) => {
    putHolds(res.cleared)
    if (res.held_elsewhere) {
      notifications.show({ color: 'owlYellow', title: 'Cleared here; still held elsewhere', closeButtonProps,
        message: res.note || 'Other spaces still hold this account, so launches here may still be refused.' })
    } else notifyOk(res.cleared.length === 1 ? 'Hold cleared' : `${res.cleared.length} holds cleared`)
  }
  const clear = (u: UsageLimit) =>
    act(`clear:${u.id}`, async () => {
      clearHeld(unwrap(await api.POST('/spaces/{space_id}/usage-limits/{limit_id}/clear', { params: { path: { space_id: spaceId, limit_id: u.id! } } })))
    })
  const clearAccount = (u: UsageLimit) =>
    act(`clear-account:${u.account_id}`, async () => {
      clearHeld(unwrap(await api.POST('/accounts/{account_id}/usage-limits/clear', {
        params: { path: { account_id: u.account_id! } },
        body: ACCOUNT_KINDS.includes(u.kind) ? { kind: u.kind as AccountKind } : {},
      })))
    })

  return (
    <Card data-testid="usage-limits">
      <Title order={5}>Usage limits</Title>
      <Text size="sm" c="dimmed" mb="sm">
        An account at its usage limit is held: launches on it are refused until it resets. Runs that reach a limit wait for the reset, kept
        alive or parked and resumed afterwards. Times are in your time zone; hover or focus one for UTC.
      </Text>
      <ErrorAlert error={holds.error ?? waits.error} />

      <Stack gap="lg">
        <section aria-labelledby="usage-holds-title">
          <Title order={6} id="usage-holds-title" mb={6}>Held accounts</Title>
          {holds.isSuccess && open.length === 0 && <Text size="sm" c="dimmed" data-testid="usage-holds-empty">No account or gateway is at a usage limit.</Text>}
          <Stack gap="xs">
            {open.map((u, i) => {
              const target = holdTarget(u, accountName)
              return (
                <Paper key={u.id || `shared-${i}`} withBorder p="sm" data-testid="usage-hold">
                  <Group justify="space-between" align="flex-start" gap="xs" wrap="wrap">
                    <Stack gap={2} style={{ minWidth: 0, flex: '1 1 16rem' }}>
                      <Group gap={6} wrap="wrap">
                        <Text size="sm" fw={600} style={{ overflowWrap: 'anywhere' }}>{target}</Text>
                        <Badge color="owlYellow" variant="light" style={{ textTransform: 'none' }}>{limitKindLabel(u.kind)}</Badge>
                        {u.shared && (
                          <Tooltip multiline maw={280} events={{ hover: true, focus: true, touch: true }}
                            label="Another space's run reached this limit on an account or gateway this space uses, so it holds launches here too. Its details stay with that space.">
                            <Badge color="gray" variant="outline" tabIndex={0} style={{ textTransform: 'none' }}>shared · another space</Badge>
                          </Tooltip>
                        )}
                      </Group>
                      <Text size="sm">
                        {u.resets_at ? <>Held until <LimitTime at={u.resets_at} /></> : (
                          <>Reset time unknown{u.check_at && <>; checked again at <LimitTime at={u.check_at} /></>}</>
                        )}
                      </Text>
                      <Text size="xs" c="dimmed">
                        Seen at <LimitTime at={u.seen_at} />
                        {!u.shared && u.source && ` · from ${SOURCE[u.source] ?? u.source}`}
                      </Text>
                    </Stack>
                    {(canControl && !u.shared && u.id) || (canAccounts && u.scope === 'account' && u.account_id) ? (
                      <Group gap="xs" wrap="wrap">
                        {canControl && !u.shared && u.id && (
                          <Button size="xs" variant="default" loading={busy === `clear:${u.id}`} onClick={() => setPending({ what: 'clear', hold: u, label: target })}>
                            Clear
                          </Button>
                        )}
                        {canAccounts && u.scope === 'account' && u.account_id && (
                          <Button size="xs" variant="default" color="owlRed" loading={busy === `clear-account:${u.account_id}`}
                            onClick={() => setPending({ what: 'clear-account', hold: u, label: target })}>
                            Clear in every space
                          </Button>
                        )}
                      </Group>
                    ) : null}
                  </Group>
                </Paper>
              )
            })}
          </Stack>
          {cleared.length > 0 && (
            <Stack gap={2} mt="xs" data-testid="usage-holds-cleared">
              <Text size="xs" fw={600} c="dimmed">Recently cleared</Text>
              {cleared.map((u, i) => (
                <Text key={u.id || i} size="xs" c="dimmed">
                  {holdTarget(u, accountName)} · {limitKindLabel(u.kind)} · cleared at <LimitTime at={u.cleared_at} />
                  {u.cleared_by && ` (${clearedBy(u.cleared_by)})`}
                </Text>
              ))}
            </Stack>
          )}
        </section>

        <section aria-labelledby="limit-waits-title">
          <Title order={6} id="limit-waits-title" mb={6}>Waiting runs</Title>
          {waits.isSuccess && active.length === 0 && <Text size="sm" c="dimmed" data-testid="limit-waits-empty">No run is parked at a usage limit.</Text>}
          <Stack gap="xs">
            {active.map((w) => {
              const st = waitState(w.state)
              return (
                <Paper key={w.id} withBorder p="sm" data-testid="limit-wait">
                  <Group justify="space-between" align="flex-start" gap="xs" wrap="wrap">
                    <Stack gap={2} style={{ minWidth: 0, flex: '1 1 16rem' }}>
                      <Group gap={6} wrap="wrap">
                        <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: w.run_id }} size="sm" fw={600}>{w.role}</AnchorLink>
                        <Text size="xs" c="dimmed">{kindLabel(w.run_kind)}</Text>
                        <Badge color={st.color} variant="light">{st.label}</Badge>
                      </Group>
                      <Text size="sm">Resumes at <LimitTime at={w.resume_at} /></Text>
                      {w.release_at && <Text size="xs" c="dimmed">{w.task_id ? 'Its task goes back to the queue' : 'Released'} at <LimitTime at={w.release_at} /></Text>}
                      {w.task_id && <Text size="xs" c="dimmed">Task <span className="mono">{w.task_id}</span></Text>}
                      {(w.attempts > 0 || w.last_error) && (
                        <Text size="xs" c="dimmed">
                          {w.attempts > 0 && `${w.attempts} attempt${w.attempts === 1 ? '' : 's'}`}
                          {w.attempts > 0 && w.last_error && ' · '}
                          {w.last_error && `waiting because ${waitReason(w.last_error)}`}
                        </Text>
                      )}
                    </Stack>
                    {canControl && w.state === 'waiting' && (
                      <Group gap="xs" wrap="wrap">
                        <Button size="xs" loading={busy === `resume:${w.id}`} onClick={() => void resume(w)}>Resume now</Button>
                        <Button size="xs" variant="default" color="owlRed" loading={busy === `release:${w.id}`} onClick={() => setPending({ what: 'release', wait: w })}>Release</Button>
                      </Group>
                    )}
                  </Group>
                </Paper>
              )
            })}
          </Stack>
          {ended.length > 0 && (
            <Stack gap={2} mt="xs" data-testid="limit-waits-ended">
              <Text size="xs" fw={600} c="dimmed">Recently ended</Text>
              {ended.map((w) => (
                <Text key={w.id} size="xs" c="dimmed">
                  {w.role} · {waitState(w.state).label.toLowerCase()} at <LimitTime at={w.updated_at} />
                  {w.new_run_id && <> as <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: w.new_run_id }} size="xs">a new run</AnchorLink></>}
                  {w.state !== 'resumed' && w.last_error && ` (${waitReason(w.last_error)})`}
                </Text>
              ))}
            </Stack>
          )}
        </section>
        {error && !pending && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
      </Stack>

      {pending && (
        <Modal opened onClose={() => { setPending(null); setError('') }} title={pending.what === 'release' ? 'Release this run?' : 'Clear this hold?'}>
          <Stack gap="sm">
            {pending.what === 'release' && (
              <Text size="sm">
                {pending.wait.role} will not be resumed. {pending.wait.task_id ? 'Its task goes back to the queue, ready and unassigned, and control is told.' : 'Control is told.'}
              </Text>
            )}
            {pending.what === 'clear' && (
              <Text size="sm">
                Launches on {pending.label} are allowed again in this space before its {limitKindLabel(pending.hold.kind)} resets. If the account is still
                exhausted, runs stop at once and hold it again. Other spaces' holds on it stay.
              </Text>
            )}
            {pending.what === 'clear-account' && (
              <Text size="sm">
                Clears every space's {ACCOUNT_KINDS.includes(pending.hold.kind) ? limitKindLabel(pending.hold.kind) : 'usage-limit'} hold on {pending.label}. Each space's control is told.
                If the account is still exhausted, runs stop at once and hold it again.
              </Text>
            )}
            {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
            <Group justify="flex-end">
              <Button variant="default" onClick={() => { setPending(null); setError('') }}>Cancel</Button>
              <Button color="owlRed" loading={busy !== ''}
                onClick={() => void (pending.what === 'release' ? release(pending.wait) : pending.what === 'clear' ? clear(pending.hold) : clearAccount(pending.hold))}>
                {pending.what === 'release' ? 'Release' : 'Clear'}
              </Button>
            </Group>
          </Stack>
        </Modal>
      )}
    </Card>
  )
}
