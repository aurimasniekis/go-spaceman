import { Alert, Stack, Text } from '@mantine/core'
import { IconArrowsExchange } from '@tabler/icons-react'
import { useAccountSwitches, useSpaceAccounts } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { LimitTime } from '@/features/limits/LimitTime'
import { limitKindLabel } from '@/features/limits/usageLimit'
import { reasonLabel, switchesOfRun } from './switches'

/**
 * The account switch a run belongs to, on its page (ADR-0090): "Moved to account B (5-hour limit on A, resets …) → the
 * new run" for a run that was moved on, and "Continued from the run on account A" for the run that took over.
 * Nothing shows for a run no switch touched.
 */
export function RunSwitchNotice({ spaceId, runId }: { spaceId: string; runId: string }) {
  const switches = useAccountSwitches(spaceId)
  const bound = useSpaceAccounts(spaceId)
  const name = (id?: string) => (id ? (bound.data?.find((b) => b.account_id === id)?.name ?? 'a removed account') : 'another account')
  const { from, into } = switchesOfRun(switches.data ?? [], runId)
  if (!from && !into) return null
  return (
    <Stack gap="xs" mb="md" data-testid="run-switch">
      {from && (
        <Alert color={from.state === 'failed' ? 'owlRed' : 'owlBlue'} variant="light" icon={<IconArrowsExchange size={16} />}>
          <Text size="sm">
            {from.state === 'fell_back' || from.state === 'cancelled' ? (
              <>No switch: this run waited at its limit instead{from.reason ? ` because ${reasonLabel(from.reason)}` : ''}.</>
            ) : (
              <>Moved to account <b>{name(from.to_account_id)}</b> ({limitKindLabel(from.limit_kind)} on {name(from.from_account_id)}
                {from.limit_resets_at && <>, resets <LimitTime at={from.limit_resets_at} /></>}){' '}
                {from.new_run_id ? <>→ <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: from.new_run_id }} size="sm">the new run</AnchorLink></> : <>(the new run has not started yet)</>}
                {from.state === 'failed' && from.reason ? `. It failed because ${reasonLabel(from.reason)}` : ''}.</>
            )}
          </Text>
        </Alert>
      )}
      {into && (
        <Alert color="owlBlue" variant="light" icon={<IconArrowsExchange size={16} />}>
          <Text size="sm">
            Continued from <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: into.from_run_id }} size="sm">the run</AnchorLink> on account <b>{name(into.from_account_id)}</b>, which reached its {limitKindLabel(into.limit_kind)}.
          </Text>
        </Alert>
      )}
    </Stack>
  )
}
