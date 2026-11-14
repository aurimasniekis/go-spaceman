import { Alert, Anchor, Badge, Button, Card, Code, Group, Select, Stack, Table, Text, Title } from '@mantine/core'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, check, unwrap } from '@/api/client'
import type { components } from '@/api/schema.gen'
import { ErrorAlert } from '@/components/ErrorAlert'
import { QRCode } from '@/components/QRCode'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { browserZone, knownZones, zoneOptions } from '@/features/notifications/timezone'

type Code = components['schemas']['LinkCode']
type TgLink = components['schemas']['Link']

/** How often the confirm page asks for the code while it waits for Telegram (plan §3.1: every 2 s, bounded by the code's TTL). */
export const POLL_MS = 2000

/** The poll interval for a code's state: until Telegram redeemed and confirmed it (and the user answered), false once nothing more can change. */
export function pollInterval(state: string | undefined, expiresAt: string | undefined, now = Date.now()): number | false {
  if (state !== 'new' && state !== 'redeemed' && state !== 'telegram_confirmed') return false
  if (expiresAt && Date.parse(expiresAt) <= now) return false
  return POLL_MS
}

const linksKey = ['telegram-links'] as const
const codeKey = (id: string) => ['telegram-link-code', id] as const

/** Account → Telegram: the linked accounts (unlink, re-confirm) and the three-step linking flow (ADR-0083, plan §3.1). */
export function TelegramLinkCard() {
  const qc = useQueryClient()
  const links = useQuery({ queryKey: linksKey, queryFn: async ({ signal }) => unwrap(await api.GET('/me/telegram/links', { signal })) })
  const [created, setCreated] = useState<Code | null>(null)
  const [busy, setBusy] = useState(false)

  const start = async () => {
    setBusy(true)
    try {
      setCreated(unwrap(await api.POST('/me/telegram/link-codes')))
    } catch (e) {
      notifyError(e)
    } finally {
      setBusy(false)
    }
  }
  const refresh = () => void qc.invalidateQueries({ queryKey: linksKey })
  const unlink = async (l: TgLink) => {
    if (!window.confirm(`Unlink ${l.telegram.name}${l.telegram.username ? ` (@${l.telegram.username})` : ''}? Its next message is refused.`)) return
    try {
      check(await api.DELETE('/me/telegram/links/{link_id}', { params: { path: { link_id: l.id } } }))
      notifyOk('Telegram account unlinked')
      refresh()
    } catch (e) {
      notifyError(e)
    }
  }
  const reconfirm = async (l: TgLink) => {
    try {
      unwrap(await api.POST('/me/telegram/links/{link_id}/reconfirm', { params: { path: { link_id: l.id } } }))
      notifyOk('Telegram link re-confirmed')
      refresh()
    } catch (e) {
      notifyError(e)
    }
  }

  return (
    <div>
      <Group justify="space-between" mb="xs">
        <Title order={5}>Telegram</Title>
        {!created && <Button size="xs" loading={busy} onClick={() => void start()}>Link Telegram</Button>}
      </Group>
      <ErrorAlert error={links.error} />
      {created && <ConfirmLink code={created} onDone={() => { setCreated(null); refresh() }} />}
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr><Table.Th>Account</Table.Th><Table.Th>Cap</Table.Th><Table.Th>Linked</Table.Th><Table.Th>Re-confirm by</Table.Th><Table.Th w={190} /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(links.data ?? []).map((l) => (
              <Table.Tr key={l.id}>
                <Table.Td>{l.telegram.name}{l.telegram.username && <Text span c="dimmed"> @{l.telegram.username}</Text>}</Table.Td>
                <Table.Td>{l.max_role}</Table.Td>
                <Table.Td>{datetime(l.confirmed_at)}</Table.Td>
                <Table.Td>
                  {l.needs_reconfirm ? <Badge color="red">overdue: the link does nothing</Badge> : l.reconfirm_due_at ? datetime(l.reconfirm_due_at) : '—'}
                </Table.Td>
                <Table.Td>
                  <Group gap="xs" justify="flex-end" wrap="nowrap">
                    <Button size="compact-xs" variant={l.needs_reconfirm ? 'filled' : 'default'} onClick={() => void reconfirm(l)}>Re-confirm</Button>
                    <Button size="compact-xs" color="red" variant="light" onClick={() => void unlink(l)}>Unlink</Button>
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
            {links.data?.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text c="dimmed" size="sm">No Telegram account is linked. You can also send /unlink to the bot to end a link.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>
      </Card>
    </div>
  )
}

/**
 * The linking steps for one code. It short-polls the code every 2 s while it can still change (link codes write no
 * change-feed entries): the deep link and QR while it waits for /start, a wait while Telegram's own confirmation is
 * pending, and then the Telegram account that wants to link with Confirm and Cancel. The time zone line shows the
 * stored zone, or the browser's when there is none (saved on Confirm only then).
 */
export function ConfirmLink({ code, onDone }: { code: Code; onDone: () => void }) {
  const q = useQuery({
    queryKey: codeKey(code.id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/me/telegram/link-codes/{code_id}', { params: { path: { code_id: code.id } }, signal })),
    initialData: code,
    refetchInterval: (query) => pollInterval(query.state.data?.state, query.state.data?.expires_at),
  })
  const st = q.data
  const prefs = useQuery({ queryKey: ['notification-prefs'], queryFn: async ({ signal }) => unwrap(await api.GET('/auth/me/notification-preferences', { signal })) })
  const stored = prefs.data?.timezone ?? ''
  const browser = browserZone()
  const [picked, setPicked] = useState<string | null>(null)
  const [changing, setChanging] = useState(false)
  const zone = picked ?? (stored || browser)
  const [busy, setBusy] = useState(false)

  const confirm = async () => {
    setBusy(true)
    try {
      // The stored zone is never overwritten by the server; only a user with none gets this one saved.
      const l = unwrap(await api.POST('/me/telegram/link-codes/{code_id}/confirm', { params: { path: { code_id: code.id } }, body: zone ? { timezone: zone } : {} }))
      notifyOk(l.timezone_saved ? `Telegram linked; time zone set to ${zone}` : 'Telegram linked')
      onDone()
    } catch (e) {
      notifyError(e)
    } finally {
      setBusy(false)
    }
  }
  const cancel = async () => {
    try {
      unwrap(await api.POST('/me/telegram/link-codes/{code_id}/cancel', { params: { path: { code_id: code.id } } }))
    } catch (e) {
      notifyError(e)
    }
    onDone()
  }

  let body: React.ReactNode
  switch (st.state) {
    case 'new':
      body = (
        <Group align="flex-start" wrap="wrap">
          {code.deep_link && <QRCode value={code.deep_link} label="QR code of the Telegram link" />}
          <Stack gap={4} maw={420}>
            <Text size="sm">Open this link in Telegram and send <Code>/start</Code> to the bot. It works once and expires {datetime(st.expires_at)}.</Text>
            {code.deep_link && <Anchor href={code.deep_link} target="_blank" rel="noreferrer" size="sm">{code.deep_link.replace(/\?start=.*/, '?start=…')}</Anchor>}
          </Stack>
        </Group>
      )
      break
    case 'redeemed':
      body = <Text size="sm">Waiting: confirm in Telegram first. The bot asked the account that opened the link to confirm; do it only if you started this.</Text>
      break
    case 'telegram_confirmed':
      body = (
        <Stack gap="xs">
          <Text>
            Telegram user <b>{st.telegram?.name}</b>
            {st.telegram?.username ? <> (@{st.telegram.username}</> : <> (</>}, id {st.telegram?.id}) wants to link to your account. Is that you?
          </Text>
          <Group gap="xs" align="flex-end">
            {changing ? (
              <Select label="Time zone" searchable data={zoneOptions(knownZones(), stored, browser)} value={zone} onChange={(v) => setPicked(v ?? '')} w={260} />
            ) : (
              <Text size="sm">Timezone: {zone || 'not set'} {!stored && zone && <Text span c="dimmed">(from your browser, saved because you have none)</Text>}</Text>
            )}
            <Button size="compact-xs" variant="subtle" onClick={() => setChanging((c) => !c)}>{changing ? 'Done' : 'change'}</Button>
          </Group>
          <Group gap="xs">
            <Button loading={busy} onClick={() => void confirm()}>Confirm</Button>
            <Button variant="default" onClick={() => void cancel()}>Cancel</Button>
          </Group>
        </Stack>
      )
      break
    case 'cancelled':
      body = st.reuse_attempted ? (
        <Alert color="red" title="This code was used by someone else">
          Another Telegram account presented your code after one had already used it, so someone besides you has seen it. It was cancelled and nothing was linked.
          Create a new code and send it only to yourself.
          <Group mt="xs"><Button size="compact-sm" onClick={onDone}>Close</Button></Group>
        </Alert>
      ) : (
        <Alert color="gray" title="Cancelled">Nothing was linked.<Group mt="xs"><Button size="compact-sm" onClick={onDone}>Close</Button></Group></Alert>
      )
      break
    case 'expired':
      body = <Alert color="yellow" title="The code expired">Create a new one.<Group mt="xs"><Button size="compact-sm" onClick={onDone}>Close</Button></Group></Alert>
      break
    default:
      body = <Alert color="green" title="Linked"><Group mt="xs"><Button size="compact-sm" onClick={onDone}>Close</Button></Group></Alert>
  }
  return (
    <Card withBorder mb="sm">
      <ErrorAlert error={q.error} />
      {body}
      {(st.state === 'new' || st.state === 'redeemed') && <Group mt="sm"><Button size="compact-xs" variant="default" onClick={() => void cancel()}>Cancel</Button></Group>}
    </Card>
  )
}
