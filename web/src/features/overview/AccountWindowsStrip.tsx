import { Badge, Card, Group, Stack, Text } from '@mantine/core'
import type { AccountState } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { pct, windowStale } from '@/features/accounts/windows'

const SHORT: Record<string, string> = { five_hour: '5 h', seven_day: '7 d' }

function Bar({ account, kind, used, stale }: { account: string; kind: string; used: number; stale: boolean }) {
  const color = used >= 90 ? 'var(--mantine-color-owlRed-filled)' : used >= 70 ? 'var(--mantine-color-owlYellow-filled)' : 'var(--mantine-color-owlBlue-filled)'
  return (
    <div style={{ flex: 1, minWidth: 0 }}>
      <Group justify="space-between" gap={4} wrap="nowrap">
        <Text size="xs">{SHORT[kind] ?? kind}</Text>
        <Text size="xs" fw={600}>{used} %{stale ? ' ?' : ''}</Text>
      </Group>
      <div role="progressbar" aria-label={`${account}, ${SHORT[kind] ?? kind} window${stale ? ', may be out of date' : ''}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={used} aria-valuetext={`${used} percent used`}
        style={{ height: 6, borderRadius: 3, background: 'var(--mantine-color-default-border)', overflow: 'hidden', opacity: stale ? 0.6 : 1 }}>
        <div style={{ width: `${used}%`, height: '100%', background: color }} />
      </div>
    </div>
  )
}

/**
 * One compact row per account: its 5-hour and 7-day window bars and any open usage limit (plan §2.2). An observation
 * that is old or past its reset is marked "?" and dimmed. Hidden when no account is bound to a readable space.
 */
export function AccountWindowsStrip({ accounts, now = Date.now() }: { accounts: AccountState[]; now?: number }) {
  if (accounts.length === 0) return null
  return (
    <Card data-testid="account-windows-strip">
      <Group justify="space-between" mb={6} wrap="nowrap">
        <Text fw={600}>Accounts</Text>
        <AnchorLink to={'/usage' as never} size="sm">Details in Usage</AnchorLink>
      </Group>
      <Stack gap={10} component="ul" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
        {accounts.map((a) => (
          <li key={a.account_id} style={{ minWidth: 0 }}>
            <Group gap="xs" mb={2} wrap="nowrap">
              <Text size="sm" fw={500} truncate style={{ minWidth: 0 }}>{a.name}</Text>
              {a.limits.length > 0 && <Badge size="xs" color="owlRed" variant="light" style={{ flexShrink: 0 }}>{a.limits.length === 1 ? 'at a limit' : `${a.limits.length} limits`}</Badge>}
            </Group>
            {a.windows.length === 0 ? <Text size="xs" c="dimmed">No window reported yet</Text> : (
              <Group gap="md" wrap="nowrap">
                {a.windows.map((w) => <Bar key={w.kind} account={a.name} kind={w.kind} used={pct(w.used_pct)} stale={windowStale(w, now)} />)}
              </Group>
            )}
          </li>
        ))}
      </Stack>
    </Card>
  )
}
