import type { UsageRow } from '@/api/client'

/** The groups drawn as their own series; the rest fold into "Other" (a ninth series is never a generated hue). */
export const MAX_SERIES = 7
export const OTHER = 'Other'

export type Stack = { bucket: string; segments: { group: string; tokens: number }[]; total: number }
export type Chart = { groups: string[]; stacks: Stack[]; max: number }

/** A group's label: an empty group is usage that had none (a run without an account, for example). */
export const groupLabel = (g: string, names?: Record<string, string>) => (g === '' ? 'None' : (names?.[g] ?? g))

/**
 * Pivots a series' rows (one per bucket and group) into one stack per bucket. The MAX_SERIES groups with the most
 * tokens over the range keep their own series, in a fixed order (by group name, so a colour follows its group and never
 * its rank); the rest fold into one Other series, last. Buckets with no usage are kept, so a gap in time shows as a gap.
 */
export function buildChart(rows: UsageRow[], buckets: string[], names?: Record<string, string>): Chart {
  const totals = new Map<string, number>()
  for (const r of rows) totals.set(r.group, (totals.get(r.group) ?? 0) + r.total_tokens)
  const ranked = [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
  const keep = new Set(ranked.slice(0, MAX_SERIES).map(([g]) => g))
  const folded = ranked.length > MAX_SERIES
  const named = [...keep].sort((a, b) => groupLabel(a, names).localeCompare(groupLabel(b, names)))
  const groups = [...named.map((g) => groupLabel(g, names)), ...(folded ? [OTHER] : [])]
  const byBucket = new Map<string, Map<string, number>>()
  for (const r of rows) {
    const key = r.bucket_start ?? ''
    const g = keep.has(r.group) ? groupLabel(r.group, names) : OTHER
    const m = byBucket.get(key) ?? new Map<string, number>()
    m.set(g, (m.get(g) ?? 0) + r.total_tokens)
    byBucket.set(key, m)
  }
  const stacks = buckets.map((bucket) => {
    const m = byBucket.get(bucket)
    const segments = groups.map((group) => ({ group, tokens: m?.get(group) ?? 0 })).filter((s) => s.tokens > 0)
    return { bucket, segments, total: segments.reduce((n, s) => n + s.tokens, 0) }
  })
  return { groups, stacks, max: Math.max(0, ...stacks.map((s) => s.total)) }
}

/** Every bucket start from `from` (inclusive) to `to` (exclusive), UTC, as RFC 3339 strings matching the API's. */
export function bucketStarts(from: string, to: string, bucket: 'hour' | 'day'): string[] {
  const step = bucket === 'hour' ? 3600_000 : 86_400_000
  const out: string[] = []
  const start = Math.floor(Date.parse(from) / step) * step
  for (let t = start; t < Date.parse(to); t += step) out.push(new Date(t).toISOString().replace('.000Z', 'Z'))
  return out
}

/** A round upper bound for the axis and its ticks (1, 2 or 5 times a power of ten). */
export function niceScale(max: number, ticks = 4): { top: number; ticks: number[] } {
  if (max <= 0) return { top: 1, ticks: [0, 1] }
  const raw = max / ticks
  const mag = 10 ** Math.floor(Math.log10(raw))
  const step = ([1, 2, 5, 10].find((m) => m * mag >= raw) ?? 10) * mag
  const n = Math.ceil(max / step)
  return { top: n * step, ticks: Array.from({ length: n + 1 }, (_, i) => i * step) }
}

/** Tokens in a short form for axes and tiles: 1.2K, 3.4M. The exact figure is always in the table. */
export function compact(n: number): string {
  const a = Math.abs(n)
  if (a >= 1e9) return `${(n / 1e9).toFixed(a >= 1e10 ? 0 : 1).replace(/\.0$/, '')}B`
  if (a >= 1e6) return `${(n / 1e6).toFixed(a >= 1e7 ? 0 : 1).replace(/\.0$/, '')}M`
  if (a >= 1e3) return `${(n / 1e3).toFixed(a >= 1e4 ? 0 : 1).replace(/\.0$/, '')}K`
  return String(n)
}

/** The harness's list-price estimate, always shown as an estimate. */
export const estimate = (usd: number) => `≈ $${usd < 1 ? usd.toFixed(3) : usd.toFixed(2)}`
