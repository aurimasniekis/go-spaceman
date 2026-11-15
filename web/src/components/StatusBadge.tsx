import { Badge, Group, Tooltip } from '@mantine/core'
import type { Node, Run, Space } from '@/api/client'
import { RunLimitBadge } from '@/features/limits/LimitTime'

const runColor: Record<Run['observed'], string> = {
  pending: 'gray',
  starting: 'owlYellow',
  running: 'owlGreen',
  stopping: 'owlYellow',
  exited: 'gray',
  failed: 'owlRed',
  lost: 'owlRed',
  unknown: 'owlPurple',
}

/**
 * A run's observed state; "unknown" means its node is disconnected (never reported as failed). A run at a usage limit,
 * or parked at one, also shows that limit (ADR-0079).
 */
export function RunState({ run }: { run: Run }) {
  const tip =
    run.observed === 'unknown'
      ? 'The node is disconnected; the run state will reconcile when it reconnects'
      : run.status
        ? `Agent status: ${run.status}`
        : undefined
  const badge = <Badge color={runColor[run.observed]}>{run.observed}{run.desired === 'stopped' && run.observed === 'running' ? ' → stop' : ''}</Badge>
  const state = tip ? <Tooltip label={tip}>{badge}</Tooltip> : badge
  const limit = <RunLimitBadge run={run} />
  return run.limit || run.end_reason === 'usage_limit' ? <Group gap={4} wrap="wrap">{state}{limit}</Group> : state
}

export function SpaceState({ space }: { space: Space }) {
  const color = space.state === 'active' ? 'owlGreen' : space.state === 'paused' ? 'owlYellow' : 'gray'
  return <Badge color={color}>{space.state}{space.state === 'paused' && space.pause_mode ? ` · ${space.pause_mode}` : ''}</Badge>
}

export function NodeState({ node }: { node: Node }) {
  if (node.state !== 'active') return <Badge color={node.state === 'pending' ? 'owlYellow' : 'owlRed'}>{node.state}</Badge>
  return <Badge color={node.connected ? 'owlGreen' : 'gray'}>{node.connected ? 'connected' : 'offline'}</Badge>
}
