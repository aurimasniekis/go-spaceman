import type { AccountSwitch } from '@/api/client'

/** What a switch is, by its cause (ADR-0090, ADR-0105): a switch-back is the move home after the original account reset. */
export const isSwitchBack = (s: Pick<AccountSwitch, 'cause'>) => s.cause === 'switch_back'

export const SWITCH_CAUSE: Record<AccountSwitch['cause'], string> = {
  usage_limit: 'Usage limit',
  operator: 'Operator',
  switch_back: 'Switched back',
}

/** The state badge of a switch: a switch-back says "Switched back" where a usage-limit switch says "Moved". */
export function switchStateLabel(s: Pick<AccountSwitch, 'cause' | 'state'>): { label: string; color: string } {
  if (!isSwitchBack(s)) return SWITCH_STATE[s.state]
  switch (s.state) {
    case 'pending': case 'parked': return { label: 'Switching back to the original account', color: 'owlBlue' }
    case 'resumed': return { label: 'Switched back, waiting for its first turn', color: 'owlBlue' }
    case 'succeeded': return { label: 'Switched back', color: 'owlGreen' }
    case 'fell_back': return { label: 'Stayed on its account', color: 'owlYellow' }
    default: return SWITCH_STATE[s.state]
  }
}

export const SWITCH_STATE: Record<AccountSwitch['state'], { label: string; color: string }> = {
  pending: { label: 'Armed, waiting for the run to be idle', color: 'owlBlue' },
  parked: { label: 'Parked, resuming on the new account', color: 'owlBlue' },
  resumed: { label: 'Resumed, waiting for its first turn', color: 'owlBlue' },
  succeeded: { label: 'Moved', color: 'owlGreen' },
  failed: { label: 'Failed', color: 'owlRed' },
  fell_back: { label: 'Waited for the reset instead', color: 'owlYellow' },
  cancelled: { label: 'Cancelled', color: 'gray' },
}

/** The fixed reasons a switch records (never raw text), in words. */
export const SWITCH_REASON: Record<string, string> = {
  no_eligible_account: 'no other account of the pool was free',
  budget_spent: "the instance's daily switch budget was spent",
  failover_off: 'usage-limit failover was turned off',
  reset_near: 'the limit was about to reset',
  resume_failed: 'the resumed run could not start',
  limited_at_once: 'the new account was at its limit too',
  limit_cleared: 'the limit cleared',
  run_stopped: 'the run was stopped',
  switch_back_off: 'switching back was turned off',
}

export const reasonLabel = (r?: string) => (r ? (SWITCH_REASON[r] ?? r.replaceAll('_', ' ')) : '')

/** The switch that moved a run away (as its source) and the one that moved it here (as its target), newest first. */
export function switchesOfRun(all: AccountSwitch[], runId: string): { from?: AccountSwitch; into?: AccountSwitch } {
  return {
    from: all.find((s) => s.from_run_id === runId),
    // Only a switch that actually started a run links to it.
    into: all.find((s) => s.new_run_id === runId),
  }
}
