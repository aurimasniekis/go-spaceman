export type RangeKey = '24h' | '7d' | '30d' | '90d'
export const RANGES: { value: RangeKey; label: string; bucket: 'hour' | 'day' }[] = [
  { value: '24h', label: 'Last 24 hours', bucket: 'hour' },
  { value: '7d', label: 'Last 7 days', bucket: 'day' },
  { value: '30d', label: 'Last 30 days', bucket: 'day' },
  { value: '90d', label: 'Last 90 days', bucket: 'day' },
]

const iso = (ms: number) => new Date(ms).toISOString().replace('.000Z', 'Z')

/**
 * The range of a choice, aligned to whole UTC buckets and ending after the current one, so the query key is stable
 * within a bucket and the newest bucket is included. Hours: the last 24 including this one; days: the last N UTC days
 * including today.
 */
export function rangeOf(key: RangeKey, now = Date.now()): { from: string; to: string; bucket: 'hour' | 'day' } {
  const hour = 3600_000
  const day = 24 * hour
  if (key === '24h') {
    const to = Math.floor(now / hour) * hour + hour
    return { from: iso(to - 24 * hour), to: iso(to), bucket: 'hour' }
  }
  const days = key === '7d' ? 7 : key === '30d' ? 30 : 90
  const to = Math.floor(now / day) * day + day
  return { from: iso(to - days * day), to: iso(to), bucket: 'day' }
}
