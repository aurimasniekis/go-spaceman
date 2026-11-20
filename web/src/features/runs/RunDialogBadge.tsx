import { Badge, Tooltip } from '@mantine/core'
import type { Run } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { isBlockedOnDialog } from './runInfo'

/**
 * "Blocked on dialog": the run waits at an interactive dialog on its terminal that SpaceMan does not answer (ADR-0102).
 * It links to the space's messages, where the notice says why and shows the screen (secrets masked) when the node sent
 * one; after a node agent restart there is no screen, and the notice says so.
 */
export function RunDialogBadge({ run, testId }: { run: Run; testId?: string }) {
  if (!isBlockedOnDialog(run)) return null
  return (
    <Tooltip label="Waiting at a dialog SpaceMan does not answer. Open the notice for why and the screen text." events={{ hover: true, focus: true, touch: true }}>
      <AnchorLink to="/spaces/$spaceId/messages" params={{ spaceId: run.space_id }} search={{ view: 'system' }} underline="never" aria-label="Blocked on dialog: open the notice in messages" style={{ flexShrink: 0, whiteSpace: 'nowrap' }} data-testid={testId ?? 'run-dialog-badge'}>
        <Badge component="span" color="owlYellow" variant="light" style={{ cursor: 'pointer', overflow: 'visible', textTransform: 'none', whiteSpace: 'nowrap' }}>blocked on dialog <span aria-hidden>→</span></Badge>
      </AnchorLink>
    </Tooltip>
  )
}
