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
