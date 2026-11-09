import { Alert, Button, Center, Group, Loader, Stack, Text } from '@mantine/core'
import { IconAlertTriangle, IconRefresh } from '@tabler/icons-react'
import { useRouter, type ErrorComponentProps } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { isChunkLoadError, reloadOnce, reloadPending } from '@/lib/chunkReload'
import { errorMessage } from './ErrorAlert'

/**
 * The router's error screen. A part of the app that failed to download (an upgrade while the tab was open, or a
 * dropped connection) reloads the page once to pick up the current build; if that already happened, it says so and
 * offers a reload. While a reload is pending, every error shows the loader: they all follow from the missing chunk and
 * are gone once the page reloads. Any other error shows its message with Reload and Try again.
 */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter()
  const chunk = isChunkLoadError(error)
  const [reloading, setReloading] = useState(() => chunk || reloadPending())
  useEffect(() => {
    if (reloadPending()) return
    if (chunk && !reloadOnce(Date.now(), undefined, error)) setReloading(false)
  }, [chunk, error])
  const reload = () => window.location.reload()

  if (reloading) {
    return (
      <Center h={300} role="status" aria-label="Loading the latest version">
        <Loader size="sm" />
      </Center>
    )
  }
  return (
    <Center p="md">
      <Alert color={chunk ? 'owlYellow' : 'owlRed'} title={chunk ? 'SpaceMan was updated' : 'This page failed to load'}
        icon={<IconAlertTriangle size={16} />} maw={560} w="100%" role="alert">
        <Stack gap="xs" align="flex-start">
          <Text size="sm">
            {chunk
              ? 'A part of the app could not be downloaded. SpaceMan may have been upgraded since this tab was opened, or the connection dropped. Reload to get the current version.'
              : errorMessage(error)}
          </Text>
          {chunk && <Text size="xs" c="dimmed">{errorMessage(error)}</Text>}
          <Group gap="xs">
            <Button size="xs" leftSection={<IconRefresh size={14} />} onClick={reload}>Reload</Button>
            {!chunk && <Button size="xs" variant="default" onClick={() => { reset(); void router.invalidate() }}>Try again</Button>}
          </Group>
        </Stack>
      </Alert>
    </Center>
  )
}
