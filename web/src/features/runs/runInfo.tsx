import { Box, Tooltip } from '@mantine/core'
import type { Run } from '@/api/client'
import { isSet } from '@/lib/format'

/** How an agent run is shown everywhere: one status dot, a state in words and a title (UX review §4). */

export const ENDED = ['exited', 'failed', 'lost'] as const

export const isEnded = (r: Run) => (ENDED as readonly string[]).includes(r.observed)

const KIND_LABEL: Record<Run['kind'], string> = { control: 'Control', worker: 'Worker', kb: 'Knowledge base', dreamer: 'Dreamer', diagnostic: 'Diagnostic' }
const HARNESS_LABEL: Record<Run['harness'], string> = { 'claude-code': 'Claude Code', codex: 'Codex' }

export const kindLabel = (k: Run['kind']) => KIND_LABEL[k] ?? k
export const harnessLabel = (h: Run['harness'] | string) => HARNESS_LABEL[h as Run['harness']] ?? h

/** "control #4": the role and the turn (once it has one). */
export const runTitle = (r: Run) => (r.turn > 0 ? `${r.role} #${r.turn}` : r.role)

export type Tone = 'working' | 'idle' | 'attention' | 'starting' | 'ended' | 'failed' | 'offline'

/** The run's tone: what the status dot shows. */
export function runTone(r: Run): Tone {
  if (r.observed === 'unknown') return 'offline'
  if (r.observed === 'failed' || r.observed === 'lost') return 'failed'
  if (r.observed === 'exited') return 'ended'
  if (r.observed === 'pending' || r.observed === 'starting') return 'starting'
  if (r.status === 'blocked' || r.status_kind === 'permission' || r.status_kind === 'question' || r.status_kind === 'auth') return 'attention'
  if (r.status === 'error') return 'failed'
  if (r.status === 'working') return 'working'
  return 'idle'
}

const TONE_COLOR: Record<Tone, string> = {
  working: 'var(--mantine-color-owlGreen-5)', idle: 'var(--mantine-color-owlBlue-5)', attention: 'var(--mantine-color-owlYellow-5)',
  starting: 'var(--mantine-color-dimmed)', ended: 'var(--mantine-color-dimmed)', failed: 'var(--mantine-color-owlRed-5)',
  offline: 'var(--mantine-color-owlPurple-5)',
}
const TONE_LABEL: Record<Tone, string> = {
  working: 'Working', idle: 'Idle', attention: 'Needs attention', starting: 'Starting', ended: 'Ended', failed: 'Failed', offline: 'Node offline',
}

/** A coloured status dot with the tone as its accessible name. */
export function RunDot({ run, size = 8 }: { run: Run; size?: number }) {
  const tone = runTone(run)
  return (
    <Tooltip label={TONE_LABEL[tone]}>
      <Box component="span" role="img" aria-label={TONE_LABEL[tone]} style={{ display: 'inline-block', width: size, height: size, borderRadius: '50%',
        background: TONE_COLOR[tone], flexShrink: 0, boxShadow: tone === 'working' ? `0 0 0 3px color-mix(in srgb, ${TONE_COLOR[tone]} 25%, transparent)` : undefined }} />
    </Tooltip>
  )
}

/** A duration in its two largest units: "45s", "12m 30s", "3h 5m", "2d 4h". */
export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60
  if (d > 0) return h ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m ? `${h}h ${m}m` : `${h}h`
  if (m > 0) return sec && m < 10 ? `${m}m ${sec}s` : `${m}m`
  return `${sec}s`
}

/** How long the run has been running (or ran). */
export function runDuration(r: Run, now = Date.now()): string | null {
  if (!isSet(r.started_at)) return null
  const end = isSet(r.ended_at) ? Date.parse(r.ended_at!) : now
  return formatDuration(end - Date.parse(r.started_at!))
}

/** The state in words, for tabs and lists: "working · 12m", "idle", "waiting for a person", "exited 3m ago". */
export function runStateText(r: Run, now = Date.now()): string {
  const tone = runTone(r)
  switch (tone) {
    case 'offline':
      return 'node offline'
    case 'starting':
      return r.observed === 'pending' ? 'queued' : 'starting…'
    case 'ended':
    case 'failed': {
      const at = isSet(r.ended_at) ? ` ${formatDuration(now - Date.parse(r.ended_at!))} ago` : ''
      return `${r.observed === 'exited' ? 'exited' : r.observed}${at}${r.end_reason ? ` · ${r.end_reason}` : ''}`
    }
    case 'attention':
      return r.status_kind === 'permission' ? 'waiting for permission' : r.status_kind === 'auth' ? 'needs sign-in' : 'waiting for input'
    default: {
      if (r.takeover) return 'taken over'
      if (r.desired === 'stopped') return 'stopping…'
      const d = runDuration(r, now)
      return tone === 'working' ? `working${d ? ` · ${d}` : ''}` : 'idle'
    }
  }
}

/** Live first (control, then workers, then the rest, each in the order they started), then ended runs, newest first. */
export function sortRuns(runs: Run[]): Run[] {
  const rank = (r: Run) => (isEnded(r) ? 10 : r.kind === 'control' ? 0 : r.kind === 'worker' ? 1 : 2)
  const at = (r: Run) => Date.parse(r.started_at ?? r.created_at)
  // Ended runs newest first; live ones in the order they started.
  return [...runs].sort((a, b) => rank(a) - rank(b) || (isEnded(a) ? at(b) - at(a) : at(a) - at(b)))
}

/** Titles that tell runs apart: agents with the same title are numbered in the given order ("kb-support (2)"). */
export function runLabels(runs: Run[]): Map<string, string> {
  const total = new Map<string, number>()
  for (const r of runs) total.set(runTitle(r), (total.get(runTitle(r)) ?? 0) + 1)
  const seen = new Map<string, number>()
  const out = new Map<string, string>()
  for (const r of runs) {
    const t = runTitle(r)
    if ((total.get(t) ?? 0) < 2) {
      out.set(r.id, t)
      continue
    }
    const n = (seen.get(t) ?? 0) + 1
    seen.set(t, n)
    out.set(r.id, `${t} (${n})`)
  }
  return out
}
