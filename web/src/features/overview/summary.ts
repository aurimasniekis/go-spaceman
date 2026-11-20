import type { UsageRow } from '@/api/client'

const DAY = 86_400_000

/** The usage window of the Overview: 14 whole UTC days ending at the end of today (exclusive), as two periods of 7. */
export function overviewRange(now = Date.now()): { from: string; to: string } {
  const end = Math.floor(now / DAY) * DAY + DAY
  const iso = (t: number) => new Date(t).toISOString().replace('.000Z', 'Z')
  return { from: iso(end - 14 * DAY), to: iso(end) }
}

export type Summary = {
  today: number
  week: number
  /** The 7 days before the last 7, for the delta (0 when that period has no usage; `delta` then returns undefined). */
  previous: number
  /** 14 daily totals, oldest first. */
  days: number[]
  top: { group: string; tokens: number }[]
  /** Tokens of every group over the last 7 days. */
  weekByGroup: Map<string, number>
}

/** Sums daily rows into today, the last 7 days, the 7 before them and a 14-day sparkline; the top groups are over the last 7 days. */
export function summarize(rows: UsageRow[], from: string, top = 5): Summary {
  const start = Date.parse(from)
  const days = Array.from({ length: 14 }, () => 0)
  const groups = new Map<string, number>()
  for (const r of rows) {
    const i = Math.floor((Date.parse(r.bucket_start ?? '') - start) / DAY)
    if (!(i >= 0 && i < 14)) continue
    days[i] += r.total_tokens
    if (i >= 7) groups.set(r.group, (groups.get(r.group) ?? 0) + r.total_tokens)
  }
  const sum = (a: number[]) => a.reduce((n, v) => n + v, 0)
  return {
    today: days[13], week: sum(days.slice(7)), previous: sum(days.slice(0, 7)), days,
    weekByGroup: groups,
    top: [...groups.entries()].filter(([, t]) => t > 0).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, top).map(([group, tokens]) => ({ group, tokens })),
  }
}

/** The change against the previous period as a signed percentage, or undefined when there is nothing to compare with. */
export function delta(now: number, previous: number): { pct: number; up: boolean } | undefined {
  if (previous <= 0) return undefined
  const pct = Math.round(((now - previous) / previous) * 100)
  return { pct: Math.abs(pct), up: pct > 0 }
}
