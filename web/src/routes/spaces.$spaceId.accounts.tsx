import { Stack } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { LocalErrorAlert } from '@/components/LocalErrorBoundary'
import { AccountPoolsCard } from '@/features/accounts/AccountPoolsCard'
import { AccountSwitchesCard } from '@/features/accounts/AccountSwitchesCard'
import { AccountWindowsPanel } from '@/features/accounts/AccountWindowsPanel'
import { FailoverSettingsCard } from '@/features/accounts/FailoverSettingsCard'
import { SpaceAccountsCard } from '@/features/accounts/SpaceAccountsCard'
import { SectionHeader } from '@/features/spaces/SpaceHeader'

export const Route = createFileRoute('/spaces/$spaceId/accounts')({
  component: AccountsPage,
  errorComponent: ({ error, reset }) => <LocalErrorAlert error={error} title="Accounts could not be shown" reset={reset} />,
})

function AccountsPage() {
  const { spaceId } = Route.useParams()
  return (
    <>
      <SectionHeader title="Accounts" description="The accounts this space's runs use, their failover order and limits, and where runs moved when one ran out" />
      <Stack gap="md">
        <SpaceAccountsCard spaceId={spaceId} />
        <AccountWindowsPanel spaceId={spaceId} />
        <AccountPoolsCard spaceId={spaceId} />
        <FailoverSettingsCard spaceId={spaceId} />
        <AccountSwitchesCard spaceId={spaceId} />
      </Stack>
    </>
  )
}
