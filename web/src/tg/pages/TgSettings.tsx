import { Alert, Badge, Card, Group, Stack, Text } from '@mantine/core'
import { useMe } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { PageHeader } from '@/components/PageHeader'
import { datetime } from '@/lib/format'

/**
 * Your Telegram settings in the Mini App: who this session acts as, what it may do, and when it ends. Linking, unlinking and
 * the notification time zone are account operations that a Telegram link cannot perform (ADR-0083: they stay in the web), so
 * this page only shows the link's state and says where to change the rest.
 */
export function TgSettings() {
  const me = useMe().data
  const grants = me?.user?.grants ?? []
  return (
    <Stack gap="md">
      <AnchorLink to="/" size="xs">‹ Overview</AnchorLink>
      <PageHeader title="Telegram settings" description="This Telegram link and what it can do" />
      <Card>
        <Stack gap={6}>
          <Group justify="space-between"><Text size="sm" c="dimmed">Signed in as</Text><Text size="sm">{me?.user?.display_name ?? '—'}</Text></Group>
          <Group justify="space-between"><Text size="sm" c="dimmed">Session ends</Text><Text size="sm">{me?.expires_at ? datetime(me.expires_at) : '—'}</Text></Group>
        </Stack>
      </Card>
      <Stack gap="xs">
        <Text fw={600}>Your access</Text>
        {grants.length === 0 && <Text size="sm" c="dimmed">No grants.</Text>}
        {grants.map((g) => (
          <Group key={`${g.domain}:${g.role}`} justify="space-between" wrap="nowrap">
            <Text size="sm" truncate>{g.domain === '*' ? 'All spaces' : g.domain}</Text>
            <Badge variant="light">{g.role}</Badge>
          </Group>
        ))}
        <Text size="xs" c="dimmed">A link may be capped below your role. The server decides what each action may do.</Text>
      </Stack>
      <Alert variant="light" title="Linking and notifications">
        Link or unlink Telegram accounts, and set your time zone for the digest and quiet hours, under Account → Telegram in the SpaceMan web app.
      </Alert>
    </Stack>
  )
}
