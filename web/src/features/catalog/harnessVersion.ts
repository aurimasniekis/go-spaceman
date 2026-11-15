// A harness profile's Claude Code version pin (ADR-0077): empty follows the node's channel; latest, stable or an exact
// X.Y.Z overrides it. The pattern is the server's (proto.CheckHarnessVersion).
const EXACT = /^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$/

export const HARNESS_CHANNELS = ['latest', 'stable'] as const

export const HARNESS_VERSION_HINT =
  'Empty follows the node’s channel (latest by default). latest or stable follow that channel; an exact version such as 2.1.322 is downloaded if missing and never falls back to another one. A node that doesn’t manage Claude Code runs a pinned launch only when its installed version matches.'

/** The pin as sent to the API (trimmed), or an error message. */
export function harnessVersionValue(raw: string): { value: string } | { error: string } {
  const v = raw.trim()
  if (v === '' || (HARNESS_CHANNELS as readonly string[]).includes(v) || EXACT.test(v)) return { value: v }
  return { error: 'Use latest, stable or an exact version such as 2.1.322, or leave it empty' }
}

/** A short label for the profile list: the pin, or that the node decides. */
export function harnessVersionLabel(v: string | undefined): string {
  return v ? `Claude Code ${v}` : ''
}
