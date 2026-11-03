import { ActionIcon, Badge, Box, Group, Loader, SegmentedControl, Slider, Stack, Text, Tooltip } from '@mantine/core'
import { IconDownload, IconPlayerPauseFilled, IconPlayerPlayFilled, IconPlayerSkipBackFilled } from '@tabler/icons-react'
import type { Terminal } from 'ghostty-web'
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, unwrap, type RecordingRecord } from '@/api/client'
import { API_PREFIX } from '@/api/session'
import { endReason, sessionStillValid } from '@/api/streams'
import { clock } from '@/lib/format'
import { createTerminal, fitFont } from './ghostty'
import { apply, emptySummary, indexAt, MAX_IDLE_MS, offsetMs, renderTo, type RecordingSummary } from './records'

interface Props {
  spaceId: string
  runId: string
  /** live streams the running terminal; replay plays the stored recording. */
  mode: 'live' | 'replay'
  /** Set while the viewer holds a takeover of the terminal (SM-UI-004): the terminal's keys go here. */
  onInput?: ((data: string) => void) | null
}

/** Mounts a ghostty-web terminal into a div and disposes it on unmount. */
function useTerminal() {
  const ref = useRef<HTMLDivElement>(null)
  const box = useRef<HTMLDivElement>(null)
  const [term, setTerm] = useState<Terminal | null>(null)
  const [error, setError] = useState('')
  // Keep the columns inside the page: refit when the page or the terminal size changes.
  useEffect(() => {
    if (!term || !box.current) return
    const el = box.current
    const fit = () => fitFont(term, el)
    fit()
    const ro = new ResizeObserver(fit)
    ro.observe(el)
    const sub = term.onResize(() => requestAnimationFrame(fit))
    return () => {
      ro.disconnect()
      sub.dispose()
    }
  }, [term])
  useEffect(() => {
    let disposed = false
    let t: Terminal | null = null
    if (!ref.current) return
    createTerminal(ref.current)
      .then((created) => {
        if (disposed) created.dispose()
        else {
          t = created
          setTerm(created)
        }
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
    return () => {
      disposed = true
      t?.dispose()
    }
  }, [])
  return { ref, box, term, error }
}

type LiveState = 'connecting' | 'live' | 'ended' | 'denied'

function StatusBadges({ sum, live, typing }: { sum: RecordingSummary; live?: LiveState; typing?: boolean }) {
  return (
    <Group gap={6}>
      {live === 'live' && (
        <Badge color={typing ? 'owlYellow' : 'owlGreen'} leftSection={<Box w={6} h={6} style={{ borderRadius: 3, background: 'currentColor' }} />}>
          {typing ? 'Live · you are typing' : 'Live · watch-only'}
        </Badge>
      )}
      {live === 'connecting' && <Badge color="owlYellow">Connecting…</Badge>}
      {live === 'ended' && <Badge color="gray">Stream ended</Badge>}
      {live === 'denied' && <Badge color="owlRed">Access lost</Badge>}
      {sum.cols > 0 && <Badge color="gray" variant="outline">{sum.cols}×{sum.rows}</Badge>}
      {sum.gaps > 0 && (
        <Tooltip label="The recorder re-attached or a viewer fell behind; output in between may be missing">
          <Badge color="owlYellow" variant="outline">{sum.gaps} gap{sum.gaps === 1 ? '' : 's'}</Badge>
        </Tooltip>
      )}
      {sum.truncated && <Badge color="owlRed" variant="outline">Truncated (quota)</Badge>}
      {sum.closed && (
        <Badge color={sum.exitCode === 0 ? 'owlGreen' : 'owlRed'} variant="outline">
          Exited{sum.exitCode !== undefined ? ` ${sum.exitCode}` : ''}{sum.reason ? ` · ${sum.reason}` : ''}
        </Badge>
      )}
    </Group>
  )
}

/**
 * The live, watch-only terminal: stored output first, then live output, over Server-Sent Events. The server ends the
 * stream when the viewer's access does (SM-AUTH-007): a removed grant shows "Access lost"; an invalid session is
 * re-checked and either resumes (rotated session) or goes to sign-in.
 */
function LiveTerminal({ spaceId, runId, onInput }: Omit<Props, 'mode'>) {
  const { ref, box, term, error } = useTerminal()
  // Watch-only unless the viewer holds a takeover; ghostty-web reads disableStdin on every key (0.4.0 source).
  useEffect(() => {
    if (!term || !onInput) return
    term.options.disableStdin = false
    const sub = term.onData(onInput)
    term.focus()
    return () => {
      sub.dispose()
      term.options.disableStdin = true
    }
  }, [term, onInput])
  const [sum, setSum] = useState<RecordingSummary>(emptySummary)
  const [state, setState] = useState<LiveState>('connecting')
  const [attempt, setAttempt] = useState(0)
  // What this terminal has drawn, kept across reconnects so output is never applied twice.
  const drawn = useRef<RecordingSummary>(emptySummary())
  useEffect(() => {
    if (!term) return
    let closed = false
    const after = drawn.current.lastSeq
    const es = new EventSource(`${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/runs/${encodeURIComponent(runId)}/terminal${after > 0 ? `?after=${after}` : ''}`)
    es.onopen = () => setState('live')
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? 'ended' : 'connecting')
    es.addEventListener('record', (ev) => {
      const r = JSON.parse((ev as MessageEvent<string>).data) as RecordingRecord
      // EventSource resumes after a reconnect with Last-Event-ID, so a record is never applied twice.
      if (r.seq <= drawn.current.lastSeq) return
      drawn.current = apply(term, drawn.current, r)
      setSum(drawn.current)
    })
    es.addEventListener('end', (ev) => {
      es.close()
      const reason = endReason(ev)
      if (reason === 'forbidden') setState('denied')
      else if (reason !== 'unauthenticated') setState('ended')
      else {
        setState('connecting')
        void sessionStillValid().then((ok) => {
          if (closed) return
          if (ok) setAttempt((n) => n + 1)
          else setState('ended')
        })
      }
    })
    return () => {
      closed = true
      es.close()
    }
  }, [term, spaceId, runId, attempt])
  return (
    <Stack gap="xs">
      <StatusBadges sum={sum} live={state} typing={!!onInput} />
      {state === 'denied' && <Text c="owlRed" size="sm">Your access to this terminal was removed.</Text>}
      {error && <Text c="owlRed" size="sm">Terminal failed to load: {error}</Text>}
      <Box ref={box} className="sm-terminal" style={{ overflow: 'auto', maxHeight: '70vh' }}>
        <div ref={ref} data-testid="terminal" aria-label={onInput ? 'Run terminal (taken over: your keys go to the program)' : 'Run terminal (watch-only)'} />
      </Box>
    </Stack>
  )
}

const SPEEDS = ['1', '2', '4', '8']

/** Replays the stored recording with play/pause, speed and seeking (SM-REC-003). */
function ReplayTerminal({ spaceId, runId }: Omit<Props, 'mode'>) {
  const { ref, box, term, error } = useTerminal()
  const [records, setRecords] = useState<RecordingRecord[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [pos, setPos] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [speed, setSpeed] = useState('1')
  const [sum, setSum] = useState<RecordingSummary>(emptySummary)
  const posRef = useRef(0)
  const sumRef = useRef<RecordingSummary>(emptySummary())

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const all: RecordingRecord[] = []
      let after = 0
      for (;;) {
        const page = unwrap(
          await api.GET('/spaces/{space_id}/runs/{run_id}/recording', { params: { path: { space_id: spaceId, run_id: runId }, query: { after, limit: 5000 } } }),
        )
        all.push(...page.records)
        if (!page.more || cancelled) break
        after = page.next_after
      }
      if (!cancelled) setRecords(all)
    })().catch((e: unknown) => !cancelled && setLoadError(e instanceof Error ? e.message : String(e)))
    return () => {
      cancelled = true
    }
  }, [spaceId, runId])

  const seek = useCallback(
    (index: number) => {
      if (!term || !records) return
      const s = renderTo(term, records, index)
      sumRef.current = s
      posRef.current = index
      setSum(s)
      setPos(index)
    },
    [term, records],
  )

  // Show the whole recording first (the most useful state of a finished run); play restarts from the beginning.
  useEffect(() => {
    if (term && records) seek(records.length)
  }, [term, records, seek])

  useEffect(() => {
    if (!playing || !term || !records) return
    let timer: ReturnType<typeof setTimeout>
    const step = () => {
      const i = posRef.current
      if (i >= records.length) {
        setPlaying(false)
        return
      }
      sumRef.current = apply(term, sumRef.current, records[i])
      posRef.current = i + 1
      setPos(i + 1)
      setSum(sumRef.current)
      const next = records[i + 1]
      const wait = next ? Math.min((next.t - records[i].t) / 1000, MAX_IDLE_MS) / Number(speed) : 0
      timer = setTimeout(step, wait)
    }
    timer = setTimeout(step, 0)
    return () => clearTimeout(timer)
  }, [playing, speed, term, records])

  const total = records ? offsetMs(records, records.length - 1) : 0
  const download = `${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/runs/${encodeURIComponent(runId)}/recording/download`

  return (
    <Stack gap="xs">
      <Group justify="space-between" wrap="nowrap">
        <Group gap="xs" wrap="nowrap">
          <Tooltip label="Restart">
            <ActionIcon aria-label="Restart replay" disabled={!records?.length} onClick={() => { seek(0); setPlaying(true) }}>
              <IconPlayerSkipBackFilled size={14} />
            </ActionIcon>
          </Tooltip>
          <Tooltip label={playing ? 'Pause' : 'Play'}>
            <ActionIcon
              aria-label={playing ? 'Pause replay' : 'Play replay'}
              disabled={!records?.length}
              onClick={() => {
                if (!playing && records && posRef.current >= records.length) seek(0)
                setPlaying(!playing)
              }}
            >
              {playing ? <IconPlayerPauseFilled size={14} /> : <IconPlayerPlayFilled size={14} />}
            </ActionIcon>
          </Tooltip>
          <SegmentedControl aria-label="Replay speed" value={speed} onChange={setSpeed} data={SPEEDS.map((s) => ({ value: s, label: `${s}×` }))} />
          <Text size="xs" c="dimmed" className="mono">
            {records ? `${clock(offsetMs(records, pos))} / ${clock(total)}` : '—'}
          </Text>
        </Group>
        <Tooltip label="Download (zstd JSONL)">
          <ActionIcon component="a" href={download} aria-label="Download recording">
            <IconDownload size={14} />
          </ActionIcon>
        </Tooltip>
      </Group>
      {records && records.length > 0 && (
        <Slider
          aria-label="Seek"
          thumbLabel="Replay position"
          min={0}
          max={Math.max(1, total)}
          value={offsetMs(records, pos)}
          label={(v) => clock(v)}
          onChange={(v) => {
            setPlaying(false)
            seek(indexAt(records, v))
          }}
          size="xs"
        />
      )}
      <StatusBadges sum={sum} />
      {(error || loadError) && <Text c="owlRed" size="sm">{error || loadError}</Text>}
      {!records && !loadError && <Loader size="xs" />}
      {records?.length === 0 && <Text size="sm" c="dimmed">No recording is stored for this run yet.</Text>}
      <Box ref={box} className="sm-terminal" style={{ overflow: 'auto', maxHeight: '70vh' }}>
        <div ref={ref} data-testid="terminal" aria-label="Recorded terminal" />
      </Box>
    </Stack>
  )
}

export function TerminalView({ spaceId, runId, mode, onInput }: Props) {
  return mode === 'live' ? <LiveTerminal spaceId={spaceId} runId={runId} onInput={onInput} /> : <ReplayTerminal spaceId={spaceId} runId={runId} />
}
