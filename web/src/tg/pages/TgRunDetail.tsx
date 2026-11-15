import { Card, Group, Stack, Text, Title } from '@mantine/core'
import { useRun, useTelegramTerminalWatch } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { TerminalView } from '@/features/terminal/TerminalView'
import { RunState } from '@/components/StatusBadge'
import { harnessLabel, kindLabel, runDuration, runStateText, runTitle } from '@/features/runs/runInfo'
import { datetime, isSet } from '@/lib/format'

/** One run, read-only: who it is, its state and timing; and, only when the space opted in, its live terminal, watch-only. */
export function TgRunDetail({ spaceId, runId }: { spaceId: string; runId: string }) {
  const run = useRun(spaceId, runId)
  // Hidden until the server says the space opted in and this link may watch (an error counts as off).
  const watch = useTelegramTerminalWatch(spaceId).data === true
  if (run.error) return <ErrorAlert error={run.error} />
  if (!run.data) return null
  const r = run.data
  const row = (k: string, v: string | null | undefined) => v ? <Group key={k} justify="space-between" wrap="nowrap" gap="xs"><Text size="sm" c="dimmed">{k}</Text><Text size="sm" ta="right" style={{ minWidth: 0, overflowWrap: 'anywhere' }}>{v}</Text></Group> : null
  return (
    <Stack gap="md">
      <AnchorLink to="/spaces/$spaceId/runs" params={{ spaceId }} size="xs">‹ Runs</AnchorLink>
      <Group justify="space-between" wrap="nowrap"><Title order={4}>{runTitle(r)}</Title><RunState run={r} /></Group>
      <Text size="sm">{runStateText(r)}</Text>
      <Card>
        <Stack gap={6}>
          {row('Kind', kindLabel(r.kind))}
          {row('Harness', `${harnessLabel(r.harness)}${r.harness_version ? ` ${r.harness_version}` : ''}`)}
          {row('Created', datetime(r.created_at))}
          {row('Started', isSet(r.started_at) ? datetime(r.started_at) : null)}
          {row('Ended', isSet(r.ended_at) ? datetime(r.ended_at) : null)}
          {row('Duration', runDuration(r))}
          {row('End reason', r.end_reason)}
          {row('Exit code', r.exit_code === undefined ? null : String(r.exit_code))}
          {row('Run', r.id)}
        </Stack>
      </Card>
      {r.error && <Text size="sm" c="owlRed">{r.error}</Text>}
      {watch ? (
        <Stack gap={4} data-testid="tg-terminal">
          <Title order={5}>Terminal (watch only)</Title>
          <TerminalView spaceId={spaceId} runId={runId} mode="live" />
        </Stack>
      ) : (
        <Text size="xs" c="dimmed">The terminal is not available in the Telegram app. Open SpaceMan in a browser to watch or take over a run.</Text>
      )}
    </Stack>
  )
}
