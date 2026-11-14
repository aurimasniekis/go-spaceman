import type { Schemas } from '@/api/client'

/** Usage limits (ADR-0079): wording and times shared by the run badge and the Limits page. */

export type UsageLimit = Schemas['FleetUsageLimit']
export type LimitWait = Schemas['FleetLimitWait']
export type LimitKind = UsageLimit['kind']

const KIND_LABEL: Record<LimitKind, string> = {
  five_hour: '5-hour limit',
  seven_day: 'weekly limit',
  seven_day_opus: 'weekly Opus limit',
  seven_day_sonnet: 'weekly Sonnet limit',
  seven_day_overage_included: 'weekly limit (overage included)',
  overage: 'overage limit',
  gateway_quota: 'gateway quota',
  gateway_budget: 'gateway budget',
  unknown: 'usage limit',
}

/** "5-hour limit", "weekly Opus limit"; an unknown kind (Claude Code's wrap-up path names none) is "usage limit". */
export const limitKindLabel = (k: string) => KIND_LABEL[k as LimitKind] ?? 'usage limit'

/** The kinds clear-account-usage-limits accepts (gateway holds are cleared per space). */
export const ACCOUNT_KINDS: readonly string[] = ['five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet', 'seven_day_overage_included', 'overage', 'unknown']

const valid = (iso: string | undefined | null): iso is string => !!iso && !Number.isNaN(Date.parse(iso))

/**
 * A time in the viewer's zone, with the zone named: "Fri 14 Nov, 15:05 EET". A time within the next or last 20 hours
 * leaves out the day: "15:05 EET". timeZone is for tests; the browser's own zone otherwise.
 */
export function localTime(iso: string | undefined | null, now = Date.now(), timeZone?: string): string {
  if (!valid(iso)) return 'unknown'
  const t = Date.parse(iso)
  const near = Math.abs(t - now) < 20 * 3600_000
  return new Intl.DateTimeFormat('en-GB', {
    timeZone, hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZoneName: 'short',
    ...(near ? {} : { weekday: 'short', day: 'numeric', month: 'short' }),
  }).format(t)
}

/** The same time in UTC, for the tooltip: "2026-11-14 13:05 UTC". */
export function utcTime(iso: string | undefined | null): string {
  if (!valid(iso)) return 'unknown'
  return `${new Date(iso).toISOString().slice(0, 16).replace('T', ' ')} UTC`
}

/** A hold that has not been cleared. */
export const isOpen = (u: UsageLimit) => !u.cleared_at

/** Waits that may still resume. */
export const isActiveWait = (w: LimitWait) => w.state === 'waiting' || w.state === 'resuming'

const WAIT_STATE: Record<LimitWait['state'], { label: string; color: string }> = {
  waiting: { label: 'Waiting', color: 'owlYellow' },
  resuming: { label: 'Resuming', color: 'owlBlue' },
  resumed: { label: 'Resumed', color: 'owlGreen' },
  released: { label: 'Released', color: 'gray' },
  cancelled: { label: 'Cancelled', color: 'gray' },
  failed: { label: 'Failed', color: 'owlRed' },
}
export const waitState = (s: LimitWait['state']) => WAIT_STATE[s] ?? { label: s, color: 'gray' }

/** Why a wait is still waiting or ended: the fixed reasons central records (never raw text). */
const WAIT_REASON: Record<string, string> = {
  node_offline: 'the node is not connected',
  space_paused: 'the space is paused',
  space_relocating: 'the space is moving to another node',
  space_full: 'the space is at its concurrent-runs limit',
  account_held: 'the account is still at its limit',
  run_not_ended: 'the parked run has not ended yet',
  launch_failed: 'the launch was refused',
  interrupted: 'a resume was interrupted',
  superseded: 'another run of the instance took over',
  task_moved: 'its task moved on',
  space_archived: 'the space is archived',
  max_wait: 'it waited the maximum time',
  operator: 'an operator released it',
}
export const waitReason = (code: string | undefined) => (code ? (WAIT_REASON[code] ?? code.replaceAll('_', ' ')) : '')

/** Who a hold holds: an account by its binding name, the Harbor Pilot connection, or a project's budget there. */
export function holdTarget(u: UsageLimit, accountName: (id: string) => string | undefined): string {
  if (u.scope === 'account') return accountName(u.account_id ?? '') ?? (u.account_id || 'an account')
  const ref = u.gateway_ref ?? ''
  if (ref.startsWith('harbor-pilot:project:')) return `Harbor Pilot project ${ref.slice('harbor-pilot:project:'.length)}`
  return 'Harbor Pilot gateway'
}

/** Who cleared a hold, in words. */
export function clearedBy(by: string | undefined): string {
  if (!by) return ''
  if (by.startsWith('run:')) return 'a run reported it over'
  if (by === 'evidence') return "another space's run reported it over"
  return by
}
