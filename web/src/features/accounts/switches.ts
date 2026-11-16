import type { AccountSwitch } from '@/api/client'

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
