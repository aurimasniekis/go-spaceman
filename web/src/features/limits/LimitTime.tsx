import { Badge, Tooltip } from '@mantine/core'
import type { Run } from '@/api/client'
import { isEnded } from '@/features/runs/runInfo'
import { limitKindLabel, localTime, utcTime } from './usageLimit'

/**
 * A time in the viewer's zone, with UTC in its tooltip. The tooltip opens on hover, keyboard focus and touch (mobile
 * first), and the element carries the machine-readable time.
 */
export function LimitTime({ at, now }: { at: string | undefined | null; now?: number }) {
  if (!at) return <>unknown</>
  return (
    <Tooltip label={utcTime(at)} events={{ hover: true, focus: true, touch: true }}>
      <time dateTime={at} tabIndex={0} style={{ whiteSpace: 'nowrap', textDecoration: 'underline dotted', textUnderlineOffset: 2 }}>{localTime(at, now)}</time>
    </Tooltip>
  )
}

/** A run's usage-limit state, when it has one: waiting at its limit (live), or parked at it (ended, ADR-0079). */
export function RunLimitBadge({ run, now }: { run: Run; now?: number }) {
  if (isEnded(run)) {
    if (run.end_reason !== 'usage_limit') return null
    return (
      <Badge color="owlYellow" variant="outline" data-testid="run-limit-badge" style={{ flexShrink: 0 }}>
        parked at usage limit
      </Badge>
    )
  }
  const l = run.limit
  if (!l) return null
  return (
    <Badge color="owlYellow" variant="light" data-testid="run-limit-badge" style={{ flexShrink: 0, overflow: 'visible', textTransform: 'none' }}>
      {limitKindLabel(l.kind)} · resets {l.resets_at ? <LimitTime at={l.resets_at} now={now} /> : 'at an unknown time'}
    </Badge>
  )
}
