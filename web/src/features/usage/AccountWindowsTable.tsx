import { Badge, Card, Group, Table, Text, Title } from '@mantine/core'
import type { AccountState } from '@/api/client'
import { pct, WINDOW_LABEL, windowStale } from '@/features/accounts/windows'
import { LimitTime } from '@/features/limits/LimitTime'
import { harnessName } from './state'

function Bar({ name, w }: { name: string; w: AccountState['windows'][number] }) {
  const used = pct(w.used_pct)
  const label = WINDOW_LABEL[w.kind] ?? w.kind
  return (
    <div style={{ minWidth: 150 }}>
      <Group justify="space-between" gap="xs" wrap="nowrap"><Text size="xs">{label}</Text><Text size="xs" fw={600}>{used} % used</Text></Group>
      <div role="progressbar" aria-label={`${name}, ${label}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={used} aria-valuetext={`${used} percent used`}
        style={{ height: 8, borderRadius: 4, background: 'var(--mantine-color-default-border)', overflow: 'hidden' }}>
        <div style={{ width: `${used}%`, height: '100%', background: used >= 90 ? 'var(--mantine-color-owlRed-filled)' : used >= 70 ? 'var(--mantine-color-owlYellow-filled)' : 'var(--mantine-color-owlBlue-filled)' }} />
      </div>
      <Text size="xs" c="dimmed" component="div">
        {w.resets_at ? <>resets <LimitTime at={w.resets_at} /></> : 'reset time unknown'}
        {windowStale(w) && <Badge ml={6} size="xs" color="owlYellow" variant="light">may be out of date</Badge>}
      </Text>
    </div>
  )
}

/**
 * Every account bound to a space the caller reads, with its latest 5-hour and 7-day window and its open usage limits.
 * A percentage is what the last run on the account reported, not a live reading; an account no run has reported for
 * shows no bar rather than an empty one. `tokens` (per account, in the range shown above) is optional.
 */
export function AccountWindowsTable({ items, tokens }: { items: AccountState[]; tokens?: Record<string, number> }) {
  const sorted = [...items].sort((a, b) => a.name.localeCompare(b.name))
  return (
    <Card p={0} data-testid="usage-accounts">
      <div style={{ padding: 'var(--mantine-spacing-md) var(--mantine-spacing-md) 0' }}>
        <Title order={5} mb={4}>Accounts</Title>
        <Text size="sm" c="dimmed" mb="sm">How much of each rate-limit window an account has used, as the last run on it reported it, and the limits it has open.</Text>
      </div>
      {sorted.length === 0 ? <Text size="sm" c="dimmed" p="md" pt={0} data-testid="usage-accounts-empty">No account is bound to a space you can read.</Text> : (
        <Table.ScrollContainer minWidth={720} scrollAreaProps={{ viewportProps: { tabIndex: 0, role: 'region', 'aria-label': 'Accounts, table' } }}>
          <Table verticalSpacing="xs" aria-label="Accounts">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Account</Table.Th><Table.Th>5-hour window</Table.Th><Table.Th>7-day window</Table.Th><Table.Th>Open limits</Table.Th>
                {tokens && <Table.Th ta="right">Tokens in range</Table.Th>}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {sorted.map((a) => {
                const five = a.windows.find((w) => w.kind === 'five_hour')
                const seven = a.windows.find((w) => w.kind === 'seven_day')
                return (
                  <Table.Tr key={a.account_id} data-testid={`usage-account-${a.name}`}>
                    <Table.Td><Text size="sm" fw={600}>{a.name}</Text>{a.harness && <Badge size="xs" variant="light">{harnessName(a.harness)}</Badge>}</Table.Td>
                    <Table.Td>{five ? <Bar name={a.name} w={five} /> : <Text size="xs" c="dimmed">not reported</Text>}</Table.Td>
                    <Table.Td>{seven ? <Bar name={a.name} w={seven} /> : <Text size="xs" c="dimmed">not reported</Text>}</Table.Td>
                    <Table.Td>
                      {a.limits.length === 0 ? <Text size="xs" c="dimmed">none</Text> : a.limits.map((l) => (
                        <Text key={`${l.kind}-${l.seen_at}`} size="xs"><Badge size="xs" color="owlRed" variant="light" mr={6}>{WINDOW_LABEL[l.kind] ?? l.kind}</Badge>{l.resets_at ? <>resets <LimitTime at={l.resets_at} /></> : 'reset time unknown'}</Text>
                      ))}
                    </Table.Td>
                    {tokens && <Table.Td ta="right">{(tokens[a.account_id] ?? 0).toLocaleString('en-US')}</Table.Td>}
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Card>
  )
}
