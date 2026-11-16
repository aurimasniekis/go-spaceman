export const WINDOW_LABEL: Record<string, string> = { five_hour: '5-hour window', seven_day: '7-day window' }

/** A window older than this may be out of date: a run reported it, and nothing has since (the API's own wording). */
export const STALE_AFTER_MS = 60 * 60_000

/** Whether an observation is old enough that the percentage may no longer hold. A reset in the past is stale too. */
export function windowStale(w: { observed_at: string; resets_at?: string }, now = Date.now()): boolean {
  if (w.resets_at && Date.parse(w.resets_at) <= now) return true
  return now - Date.parse(w.observed_at) > STALE_AFTER_MS
}

/** The percentage clamped to 0–100 and rounded for display. */
export const pct = (v: number) => Math.max(0, Math.min(100, Math.round(v)))
