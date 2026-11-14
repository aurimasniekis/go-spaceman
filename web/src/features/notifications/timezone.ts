/**
 * The user's own time zone (ADR-0083, operator answer Q9): the Telegram digest and quiet hours use it, and are off
 * while it is unset. There is no server-wide fallback, so the UI never fills it in without the user choosing it.
 */

/** The browser's IANA zone, or '' when it can't tell. */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  } catch {
    return ''
  }
}

/** The IANA zones this browser knows ('UTC' at least). */
export function knownZones(): string[] {
  const supported = (Intl as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf
  try {
    const zones = supported ? supported('timeZone') : []
    return zones.includes('UTC') ? zones : ['UTC', ...zones]
  } catch {
    return ['UTC']
  }
}

/**
 * The options of the time zone picker: every known zone plus the stored one and the browser's when the list lacks them
 * (a zone the server accepts but this browser doesn't list must stay selectable), sorted, without duplicates.
 */
export function zoneOptions(known: string[], current: string, browser: string): string[] {
  const all = new Set(known)
  for (const z of [current, browser]) if (z) all.add(z)
  return [...all].sort((a, b) => a.localeCompare(b))
}
