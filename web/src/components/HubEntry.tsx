import { Navigate } from '@tanstack/react-router'
import { Alert } from '@mantine/core'
import { useMe } from '@/api/queries'
import { PageHeader } from '@/components/PageHeader'
import { hubOf, visibleTabs, type HubKey } from '@/lib/nav'

/** A hub's entry route: sends the user to the first tab they may see, or says there is nothing for them here. */
export function HubEntry({ hub }: { hub: HubKey }) {
  const me = useMe()
  // Unreachable on the web: AuthGate renders routes only once `me` has resolved, so don't add a loader here.
  if (!me.data) return null
  const h = hubOf(hub)
  const first = visibleTabs(h, me.data)[0]
  if (first) return <Navigate to={first.to} replace />
  return (
    <>
      <PageHeader title={h.label} />
      <Alert color="owlYellow">There is nothing here for your role.</Alert>
    </>
  )
}
