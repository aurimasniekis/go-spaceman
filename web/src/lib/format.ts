/** Formats an RFC 3339 time (or unix microseconds) as a short relative time: "now", "5m ago", "in 3h". */
export function ago(v: string | number | undefined | null, now = Date.now()): string {
  if (v === undefined || v === null || v === '') return '—'
  const t = typeof v === 'number' ? v / 1000 : Date.parse(v)
  if (Number.isNaN(t) || t <= 0) return '—'
  const s = Math.round((now - t) / 1000)
  const abs = Math.abs(s)
  const unit = abs < 5 ? '' : abs < 60 ? `${abs}s` : abs < 3600 ? `${Math.round(abs / 60)}m` : abs < 86400 ? `${Math.round(abs / 3600)}h` : `${Math.round(abs / 86400)}d`
  if (!unit) return 'now'
  return s >= 0 ? `${unit} ago` : `in ${unit}`
}

/** Formats an RFC 3339 time (or unix microseconds) as a local date-time. */
export function datetime(v: string | number | undefined | null): string {
  if (v === undefined || v === null || v === '') return '—'
  const t = typeof v === 'number' ? new Date(v / 1000) : new Date(v)
  return Number.isNaN(t.getTime()) ? '—' : t.toLocaleString()
}

/** Formats a duration in milliseconds as m:ss or h:mm:ss. */
export function clock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`
}

/** A Go zero time ("0001-01-01T00:00:00Z") or empty value means "never". */
export function isSet(v: string | undefined | null): v is string {
  return !!v && !v.startsWith('0001-01-01')
}
