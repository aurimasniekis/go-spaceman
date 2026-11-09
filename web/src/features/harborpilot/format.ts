/** Display helpers for the Harbor Pilot settings (ADR-0060). */

const HARNESS_LABELS: Record<string, string> = { 'claude-code': 'Claude Code', codex: 'Codex' }

/** A harness kind's display name (unknown kinds as they are). */
export const harnessLabel = (h: string) => HARNESS_LABELS[h] ?? h

/** A token lifetime in seconds, e.g. "24 h", "1 h 30 min", "7 days". */
export function durationLabel(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  if (seconds % 86400 === 0) {
    const d = seconds / 86400
    return d === 1 ? '24 h' : `${d} days`
  }
  const h = Math.floor(seconds / 3600)
  const m = Math.round((seconds % 3600) / 60)
  if (h === 0) return `${m} min`
  return m === 0 ? `${h} h` : `${h} h ${m} min`
}

/** The client-side check of a token lifetime (the server checks it too). */
export function ttlError(seconds: number): string | undefined {
  return seconds < 900 || seconds > 604800 ? 'Between 900 seconds (15 minutes) and 604800 (7 days)' : undefined
}

const origin = (u: string) => u.trim().replace(/\/+$/, '')

/**
 * Whether a connection edit moves the gateway: another gateway URL, another URL runs use, or plain HTTP newly allowed.
 * The server then needs the tokens entered again (it never sends stored tokens to a new address); this mirrors its
 * check so the form can say so before saving; the server's check is the one that counts.
 */
export function gatewayAddressChanged(
  current: { base_url: string; run_base_url: string; allow_insecure_http: boolean } | null,
  baseURL: string, runURL: string, insecure: boolean,
): boolean {
  if (!current) return false
  const base = origin(baseURL)
  const run = origin(runURL) || base
  const curRun = origin(current.run_base_url) || origin(current.base_url)
  return base !== origin(current.base_url) || run !== curRun || (insecure && !current.allow_insecure_http)
}

/** The overall Harbor Pilot status of a space (HP-T5): its badge label and colour. */
export const STATUS_BADGE: Record<string, { label: string; color: string }> = {
  ok: { label: 'Within budgets', color: 'owlGreen' },
  warning: { label: 'Warning', color: 'owlYellow' },
  exceeded: { label: 'Budget used up: launches refused', color: 'owlRed' },
  unavailable: { label: 'Status unavailable', color: 'gray' },
  off: { label: 'Not enabled', color: 'gray' },
}

/** One budget's state: its badge label and colour. A used-up warn-only budget only warns. */
export function budgetBadge(state: string, enforcement: string): { label: string; color: string } {
  switch (state) {
    case 'exceeded':
      return enforcement === 'block' ? { label: 'Used up', color: 'owlRed' } : { label: 'Over (warn only)', color: 'owlYellow' }
    case 'warning':
      return { label: 'Warning', color: 'owlYellow' }
    case 'ok':
      return { label: 'OK', color: 'owlGreen' }
    case 'disabled':
      return { label: 'Disabled', color: 'gray' }
    case 'scheduled':
      return { label: 'Scheduled', color: 'owlBlue' }
    case 'expired':
      return { label: 'Ended', color: 'gray' }
  }
  return { label: 'Unknown state', color: 'gray' }
}

/** How a budget repeats: "per day", or "one time" for a one-time budget (Harbor Pilot 0.1.4, period once). */
export const periodLabel = (period: string) => (period === 'once' ? 'one time' : `per ${period}`)

/**
 * When a budget resets or ends: a recurring budget resets at its period end; a one-time budget never resets, and its
 * window ends at period_end, or never when it has none (Harbor Pilot 0.1.4 sends null).
 */
export function resetLabel(b: { period: string; period_end?: string | null; state: string }): string {
  if (b.period !== 'once') return b.period_end ? new Date(b.period_end).toLocaleString() : '—'
  if (b.state === 'expired') return b.period_end ? `Ended ${new Date(b.period_end).toLocaleString()}` : 'Ended'
  return b.period_end ? `Never; ends ${new Date(b.period_end).toLocaleString()}` : 'Never'
}

/** What the connected gateway's version source means, for the connection page. */
export function gatewayVersionLabel(g: { source: string; version?: string }): string {
  switch (g.source) {
    case 'system':
      return g.version || 'unknown build'
    case 'legacy':
      return 'older than 0.1.4'
  }
  return 'unknown'
}

/** What a budget covers, e.g. "Project prj_…", "Client binding cbn_…", "Every request". */
export function scopeLabel(scope: string, scopeID: string): string {
  switch (scope) {
    case 'project':
      return `Project ${scopeID}`
    case 'workspace':
      return `Workspace ${scopeID}`
    case 'binding':
      return `Client binding ${scopeID}`
    case 'global':
      return 'Every request'
  }
  return `${scope} ${scopeID}`.trim()
}

/** How far a budget is used, as a percentage for a progress bar (0-100). */
export const usedPercent = (fraction: number) => Math.max(0, Math.min(100, Math.round(fraction * 100)))

const COST_KIND: Record<string, string> = {
  actual_api_charge: 'charged',
  estimated_api_charge: 'estimated',
  list_equivalent: 'at list prices',
  subscription_fee: 'subscription',
  allocation: 'allocated',
}

/** A Harbor Pilot cost entry, e.g. "0.0123 USD estimated". Categories are never summed. */
export const costLabel = (c: { kind: string; currency: string; amount: string }) => `${c.amount} ${c.currency} ${COST_KIND[c.kind] ?? c.kind}`

/** What a project usage window covers: "this day", "this month", or "the last 30 days". */
export const windowLabel = (w: string) => (w === 'last_30_days' ? 'the last 30 days' : w === 'once' ? 'since the one-time budget started counting' : `this ${w}`)
