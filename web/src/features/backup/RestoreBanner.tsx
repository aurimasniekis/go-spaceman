import { Alert, Button, Group, Text } from '@mantine/core'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { api, unwrap } from '@/api/client'
import { keys, useRestoreNotice } from '@/api/queries'
import { datetime } from '@/lib/format'
import { notifyError } from '@/lib/notify'

/**
 * Shown to everyone after central was restored from a backup (SM-STORE-006): every space came up paused with new
 * execution authority and needs an explicit resume. It stays while any of those spaces is still paused, or until an
 * administrator dismisses it.
 */
export function RestoreBanner({ canAcknowledge }: { canAcknowledge: boolean }) {
  const qc = useQueryClient()
  const q = useRestoreNotice(true)
  const n = q.data
  if (!n?.restored || !n.state) return null
  if (n.state.acknowledged_at && n.still_paused === 0) return null
  const ack = async () => {
    try {
      unwrap(await api.POST('/system/restore/acknowledge'))
      void qc.invalidateQueries({ queryKey: keys.restore })
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <Alert color="owlYellow" title="Restored from a backup — spaces are paused" mb="md">
      <Group justify="space-between" wrap="nowrap" align="flex-start">
        <Text size="sm">
          This installation was restored from backup <code>{n.state.backup_id}</code> (taken {datetime(n.state.backup_created_at)}). Every space came up
          paused and execution authority was renewed; runs from before the restore were stopped. {n.still_paused} of {n.state.paused_spaces.length} space(s)
          are still paused: check each space, then resume it. <Link to="/backups">Details</Link>
        </Text>
        {canAcknowledge && !n.state.acknowledged_at && <Button color="owlYellow" variant="light" onClick={() => void ack()}>Dismiss</Button>}
      </Group>
    </Alert>
  )
}
