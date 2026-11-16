import { Badge, Card, Group, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { useAccountWindows, useSpaceAccounts } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { LimitTime } from '@/features/limits/LimitTime'
import { pct, WINDOW_LABEL, windowStale } from './windows'

/**
 * The 5-hour and 7-day windows of the space's accounts as the last run on each reported them (AS-T7). A percentage is
 * an observation, not a live reading: its age shows, and one that is old or past its reset says so. An account no run
 * has reported for shows no bar rather than an empty one.
 */
export function AccountWindowsPanel({ spaceId }: { spaceId: string }) {
  const windows = useAccountWindows(spaceId)
  const bound = useSpaceAccounts(spaceId)
  const items = windows.data ?? []
  const withoutReport = (bound.data ?? []).filter((b) => !items.some((a) => a.account_id === b.account_id && a.windows.length > 0))
  return (
    <Card data-testid="account-windows">
      <Title order={5} mb={4}>Account windows</Title>
      <Text size="sm" c="dimmed" mb="sm">How much of each rate-limit window the accounts have used, as the last run on the account reported it.</Text>
      <ErrorAlert error={windows.error} />
      <SimpleGrid cols={{ base: 1, md: 2 }}>
        {items.filter((a) => a.windows.length > 0).map((a) => (
          <Stack key={a.account_id} gap={6} data-testid={`windows-${a.name}`}>
            <Group gap="xs"><Text fw={600} size="sm">{a.name}</Text>{a.harness && <Badge size="xs" variant="light">{a.harness === 'codex' ? 'Codex' : 'Claude Code'}</Badge>}</Group>
            {a.windows.map((w) => {
              const stale = windowStale(w)
              const used = pct(w.used_pct)
              return (
                <div key={w.kind}>
                  <Group justify="space-between" gap="xs" wrap="nowrap">
                    <Text size="xs">{WINDOW_LABEL[w.kind] ?? w.kind}</Text>
                    <Text size="xs" fw={600}>{used} % used</Text>
                  </Group>
                  <div role="progressbar" aria-label={`${a.name}, ${WINDOW_LABEL[w.kind] ?? w.kind}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={used} aria-valuetext={`${used} percent used`}
                    style={{ height: 8, borderRadius: 4, background: 'var(--mantine-color-default-border)', overflow: 'hidden' }}>
                    <div style={{ width: `${used}%`, height: '100%', background: used >= 90 ? 'var(--mantine-color-owlRed-filled)' : used >= 70 ? 'var(--mantine-color-owlYellow-filled)' : 'var(--mantine-color-owlBlue-filled)' }} />
                  </div>
                  <Text size="xs" c="dimmed" component="div">
                    {w.resets_at ? <>resets <LimitTime at={w.resets_at} /></> : 'reset time unknown'} · observed <LimitTime at={w.observed_at} />
                    {stale && <Badge ml={6} size="xs" color="owlYellow" variant="light">may be out of date</Badge>}
                  </Text>
                </div>
              )
            })}
          </Stack>
        ))}
      </SimpleGrid>
      {withoutReport.length > 0 && (
        <Text size="xs" c="dimmed" mt="sm">No window reported yet for {withoutReport.map((b) => b.name).join(', ')}. A run reports its account's windows once it has run.</Text>
      )}
      {!windows.isLoading && items.length === 0 && withoutReport.length === 0 && <Text size="sm" c="dimmed">No account is bound to this space.</Text>}
    </Card>
  )
}
