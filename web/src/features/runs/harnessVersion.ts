// The harness version a run launched, as its node reported it (protocol 1.11, ADR-0077).
export function runHarnessVersion(r: { harness_version?: string; harness_requested?: string; harness_fallback?: string }): string {
  if (!r.harness_version) return ''
  const notes: string[] = []
  if (r.harness_requested && r.harness_requested !== r.harness_version) notes.push(`requested ${r.harness_requested}`)
  if (r.harness_fallback) notes.push(`fallback: ${r.harness_fallback.replaceAll('_', ' ')}`)
  return notes.length > 0 ? `${r.harness_version} (${notes.join('; ')})` : r.harness_version
}

// A manifest dependency: a version, or (protocol 1.11 harness entries) what was requested and the node's default then.
export type Dependency = string | number | { requested?: string; node_default?: string }

export function dependencyText(name: string, v: Dependency): string {
  if (typeof v !== 'object' || v === null) return `${name} ${v}`
  const parts = [v.requested && `requested ${v.requested}`, v.node_default && `node default ${v.node_default}`].filter(Boolean)
  return parts.length > 0 ? `${name} ${parts.join(', ')}` : name
}
