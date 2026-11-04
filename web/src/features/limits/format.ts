import { bytes } from '@/lib/format'

/** A limit's value in its unit: "No limit" for 0 where 0 means none ("Kept" for a retention period, "Never" for minutes), sizes in binary
 * units, cents as dollars, a switch as On or Off. */
export function limitValue(value: number, unit: string, zeroMeansNone = false): string {
  if (zeroMeansNone && value === 0) return unit === 'days' ? 'Kept' : unit === 'minutes' ? 'Never' : 'No limit'
  switch (unit) {
    case 'days':
      return value === 1 ? '1 day' : `${value.toLocaleString('en-US')} days`
    case 'bytes':
      return bytes(value)
    case 'usd_cents':
      return usd(value)
    case 'per_minute':
      return `${value.toLocaleString('en-US')} / min`
    case 'minutes':
      return duration(value)
    case 'percent':
      return `${value} %`
    case 'tokens':
      return `${value.toLocaleString('en-US')} tokens`
    case 'switch':
      return value === 0 ? 'Off' : 'On'
    default:
      return value.toLocaleString('en-US')
  }
}

/** US cents as dollars: 125 → "$1.25". */
export function usd(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

/** Minutes as a short duration: 90 → "1h 30m", 1440 → "1d". */
export function duration(m: number): string {
  if (m <= 0) return '—'
  const d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60), r = m % 60
  return [d && `${d}d`, h && `${h}h`, r && `${r}m`].filter(Boolean).join(' ')
}

/** Seconds as an interval phrase: 3600 → "every 1 h", 90 → "every 90 s", 172800 → "every 2 d". */
export function every(seconds: number): string {
  if (seconds % 86400 === 0) return `every ${seconds / 86400} d`
  if (seconds % 3600 === 0) return `every ${seconds / 3600} h`
  if (seconds % 60 === 0) return `every ${seconds / 60} min`
  return `every ${seconds} s`
}

/** When a schedule runs, in one line. */
export function scheduleWhen(s: { kind: string; cron?: string; interval_seconds?: number; timezone: string }): string {
  return s.kind === 'cron' ? `${s.cron} (${s.timezone})` : every(s.interval_seconds ?? 0)
}

export const MISSED_LABEL: Record<string, string> = { skip: 'Skip missed', run_once: 'Run once', catch_up: 'Catch up' }
