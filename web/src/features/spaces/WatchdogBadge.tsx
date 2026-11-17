import { Badge, Tooltip } from '@mantine/core'
import { useWatchdog } from '@/api/queries'
import { ago, isSet } from '@/lib/format'
import type { components } from '@/api/schema.gen'

type Watchdog = components['schemas']['SpaceWatchdog']

/** What waits for control, as a short phrase ("2 messages, 1 review"). Counts only, as the API gives them. */
export function backlogText(b: Watchdog['backlog']): string {
  const part = (n: number, one: string, many = `${one}s`) => (n > 0 ? [`${n} ${n === 1 ? one : many}`] : [])
  const out = [...part(b.messages, 'message'), ...part(b.reviews, 'review'), ...part(b.failed, 'failed run'), ...part(b.exited, 'ended run'), ...part(b.startable, 'task to start', 'tasks to start')]
  return out.join(', ')
}

/** The label and tooltip of the control's watchdog badge, or null when the watchdog is not acting on it. */
export function watchdogBadge(w: Watchdog | undefined): { label: string; color: string; tip: string } | null {
  if (!w || (w.status !== 'nudging' && w.status !== 'escalated')) return null
  const waiting = backlogText(w.backlog)
  const nudges = `${w.nudges} reminder${w.nudges === 1 ? '' : 's'} sent`
  const last = w.last_nudge && isSet(w.last_nudge) ? `, last ${ago(w.last_nudge)}` : ''
  if (w.status === 'escalated') {
    return { label: 'not responding', color: 'owlRed', tip: `Control left waiting work alone after ${nudges}${last}; the space's people were told.${waiting ? ` Waiting: ${waiting}.` : ''}` }
  }
  return { label: 'being nudged', color: 'owlYellow', tip: `The watchdog reminded control of waiting work: ${nudges}${last}.${waiting ? ` Waiting: ${waiting}.` : ''}` }
}

/** On the control's row: shown while the control watchdog is nudging it or has escalated (ADR-0099). */
export function WatchdogBadge({ spaceId }: { spaceId: string }) {
  const w = useWatchdog(spaceId)
  const b = watchdogBadge(w.data)
  if (!b) return null
  return (
    <Tooltip label={b.tip} multiline maw={320} withArrow>
      <Badge color={b.color} variant="light" data-testid="watchdog-badge" style={{ flexShrink: 0, textTransform: 'none' }}>{b.label}</Badge>
    </Tooltip>
  )
}
