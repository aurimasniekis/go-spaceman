import { Button, Center, Stack, Text, Title } from '@mantine/core'
import { webApp } from './webapp'

/** Shown when the Mini App has no session (plan §3.4.2): one button, no retry. */
export function ReopenScreen({ reason }: { reason: string }) {
  return (
    <Center h="100vh" p="md">
      <Stack align="center" gap="sm" maw={360} ta="center">
        <Title order={3}>Session ended</Title>
        <Text size="sm">{reason}</Text>
        <Button onClick={() => webApp()?.close()}>Close</Button>
      </Stack>
    </Center>
  )
}
