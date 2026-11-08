import { Alert } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { useMe, useSystemStatus } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SystemView } from '@/features/system/SystemView'
import { hasGlobal } from '@/lib/access'

export const Route = createFileRoute('/system')({ component: SystemPage })

/** This instance's status (SM-OBS-006): health, versions, activity and where it runs. Administrators only. */
function SystemPage() {
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const status = useSystemStatus(admin)
  return (
    <>
      <PageHeader title="System" description="This SpaceMan instance: health, versions, activity and where it runs." />
      {me && !admin && <Alert color="owlYellow">The System page is for global administrators.</Alert>}
      <ErrorAlert error={status.error} />
      {status.data && <SystemView s={status.data} />}
    </>
  )
}
