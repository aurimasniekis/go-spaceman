import { ActionIcon, Badge, Box, Button, Group, Loader, SegmentedControl, Slider, Stack, Text, Tooltip } from '@mantine/core'
import { IconArrowDown, IconDownload, IconPlayerPauseFilled, IconPlayerPlayFilled, IconPlayerSkipBackFilled } from '@tabler/icons-react'
import type { Terminal } from 'ghostty-web'
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, unwrap, type RecordingRecord, type Run } from '@/api/client'
import { API_PREFIX } from '@/api/session'
import { endReason, sessionStillValid } from '@/api/streams'
import { clock } from '@/lib/format'
import { createTerminal, disposeTerminal, fitFont, followCursor, regularCell, rowHeight, scrollWithWheelAndTouch } from './ghostty'
import { jumpToLive, keepingPosition, scrolledAway } from './scroll'
import { apply, emptySummary, indexAt, MAX_IDLE_MS, offsetMs, renderTo, type RecordingSummary } from './records'
import { fitDimensions, TerminalSizeControl } from './TerminalSize'

interface Props {
  spaceId: string
  runId: string
  /** live streams the running terminal; replay plays the stored recording. */
  mode: 'live' | 'replay'
  /** Set while the viewer holds a takeover of the terminal (SM-UI-004): the terminal's keys go here. */
  onInput?: ((data: string) => void) | null
  /** The live terminal's size menu (ADR-0075): the run, and why this viewer cannot resize it ('' when they can). */
  resize?: { run: Run; blocker: string }
}

/** The terminal area's height: the box's max-height (70vh) less its padding and border. */
const areaHeight = () => Math.floor(window.innerHeight * 0.7) - 14

/**
 * Mounts a ghostty-web terminal into a div and disposes it on unmount. `input` is the takeover holder's key handler
 * (null in watch mode): a full-screen program's wheel scrolling goes there, never anywhere while watching.
 */
function useTerminal(input: ((data: string) => void) | null = null) {
  const ref = useRef<HTMLDivElement>(null)
  const box = useRef<HTMLDivElement>(null)
  const [term, setTerm] = useState<Terminal | null>(null)
  const [error, setError] = useState('')
  const inputRef = useRef(input)
  useEffect(() => {
    inputRef.current = input
  }, [input])
  // Keep the columns inside the page: refit when the page or the terminal size changes.
  useEffect(() => {
    if (!term || !box.current) return
    const el = box.current
    const fit = () => fitFont(term, el)
    fit()
    const ro = new ResizeObserver(fit)
    ro.observe(el)
    const sub = term.onResize(() => requestAnimationFrame(fit))
    const unfollow = followCursor(term, el)
    const unwheel = scrollWithWheelAndTouch(term, el, () => inputRef.current)
    return () => {
      ro.disconnect()
      sub.dispose()
      unfollow()
      unwheel()
    }
  }, [term])
  useEffect(() => {
    let disposed = false
    let t: Terminal | null = null
    if (!ref.current) return
    createTerminal(ref.current)
      .then((created) => {
        if (disposed) disposeTerminal(created)
        else {
          t = created
          setTerm(created)
        }
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
    return () => {
      disposed = true
      if (t) disposeTerminal(t)
    }
  }, [])
  return { ref, box, term, error }
}

/**
 * Whether the viewer has scrolled up from live output, re-checked when the box or the scrollback moves and as output
 * arrives, with the action that goes back.
 */
function useScrolledAway(term: Terminal | null, box: React.RefObject<HTMLDivElement | null>) {
  const [away, setAway] = useState(false)
  useEffect(() => {
    const el = box.current
    if (!term || !el) return
    let frame = 0
    const check = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        // How many lines the view is up in the scrollback, for tests and assistive tools that can't read the canvas.
        el.dataset.scrollback = String(Math.round(term.viewportY))
        setAway(scrolledAway(term, el, rowHeight(term)))
      })
    }
    el.addEventListener('scroll', check, { passive: true })
    const scrolled = term.onScroll(check)
    const moved = term.onCursorMove(check)
    return () => {
      cancelAnimationFrame(frame)
      el.removeEventListener('scroll', check)
      scrolled.dispose()
      moved.dispose()
    }
  }, [term, box])
  const jump = useCallback(() => {
    if (!term || !box.current) return
    jumpToLive(term, box.current, rowHeight(term))
    setAway(false)
  }, [term, box])
  return { away, jump }
}

type LiveState = 'connecting' | 'live' | 'ended' | 'denied'

/** How long a hidden tab keeps its live terminal stream before releasing the connection. */
const HIDDEN_GRACE_MS = 5_000

/**
 * Whether the tab is visible, turning false only after it has been hidden for HIDDEN_GRACE_MS. Over HTTP/1.1 a browser
 * opens at most six connections to a host for all its tabs, so background tabs give their live stream back.
 */
function useTabVisible(): boolean {
  const [visible, setVisible] = useState(() => typeof document === 'undefined' || document.visibilityState !== 'hidden')
  useEffect(() => {
    if (typeof document === 'undefined') return
    let timer: ReturnType<typeof setTimeout> | undefined
    const onChange = () => {
      clearTimeout(timer)
      if (document.visibilityState === 'hidden') timer = setTimeout(() => setVisible(false), HIDDEN_GRACE_MS)
      else setVisible(true)
    }
    document.addEventListener('visibilitychange', onChange)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onChange)
    }
  }, [])
  return visible
}

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
      {sum.cols > 0 && <Badge color="gray" variant="outline" data-testid="terminal-badge-size">{sum.cols}×{sum.rows}</Badge>}
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
function LiveTerminal({ spaceId, runId, onInput, resize }: Omit<Props, 'mode'>) {
  const { ref, box, term, error } = useTerminal(onInput ?? null)
  const measure = useCallback(() => {
    const cell = term ? regularCell(term) : null
    if (!cell || !box.current) return null
    return fitDimensions(box.current.clientWidth - 14, areaHeight(), cell.width, cell.height)
  }, [term, box])
  const { away, jump } = useScrolledAway(term, box)
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
  const visible = useTabVisible()
  // What this terminal has drawn, kept across reconnects so output is never applied twice.
  const drawn = useRef<RecordingSummary>(emptySummary())
  useEffect(() => {
    // A hidden tab releases the stream; coming back resumes after the last record drawn.
    if (!term || !visible) return
    let closed = false
    const after = drawn.current.lastSeq
    const es = new EventSource(`${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/runs/${encodeURIComponent(runId)}/terminal${after > 0 ? `?after=${after}` : ''}`)
    es.onopen = () => setState('live')
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? 'ended' : 'connecting')
    es.addEventListener('record', (ev) => {
      const r = JSON.parse((ev as MessageEvent<string>).data) as RecordingRecord
      // EventSource resumes after a reconnect with Last-Event-ID, so a record is never applied twice.
      if (r.seq <= drawn.current.lastSeq) return
      // A viewer reading earlier output stays where they are while new output arrives.
      drawn.current = keepingPosition(term, () => apply(term, drawn.current, r))
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
  }, [term, spaceId, runId, attempt, visible])
  return (
    <Stack gap="xs" style={{ minWidth: 0 }}>
      <Group justify="space-between" gap="xs" wrap="wrap">
        <StatusBadges sum={sum} live={state} typing={!!onInput} />
        {resize && <TerminalSizeControl spaceId={spaceId} run={resize.run} blocker={resize.blocker} measure={measure} observe={box} />}
      </Group>
      {state === 'denied' && <Text c="owlRed" size="sm">Your access to this terminal was removed.</Text>}
      {error && <Text c="owlRed" size="sm">Terminal failed to load: {error}</Text>}
      <Box pos="relative" style={{ minWidth: 0 }}>
        <Box ref={box} className="sm-terminal" style={{ overflow: 'auto', maxHeight: '70vh', overscrollBehavior: 'contain' }}>
          <div ref={ref} data-testid="terminal" aria-label={onInput ? 'Run terminal (taken over: your keys go to the program)' : 'Run terminal (watch-only)'} />
        </Box>
        {away && (
          <Button
            size="xs"
            radius="xl"
            leftSection={<IconArrowDown size={14} />}
            onClick={jump}
            data-testid="jump-to-live"
            style={{ position: 'absolute', bottom: 12, left: '50%', transform: 'translateX(-50%)', zIndex: 2, boxShadow: 'var(--mantine-shadow-md)' }}
          >
            Jump to live
          </Button>
        )}
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
    <Stack gap="xs" style={{ minWidth: 0 }}>
      <Group justify="space-between" wrap="nowrap" gap="xs" data-testid="replay-toolbar">
        <Group gap="xs" wrap="wrap" style={{ minWidth: 0 }}>
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
          <ActionIcon style={{ flexShrink: 0 }} component="a" href={download} aria-label="Download recording">
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

/**
 * A run's terminal. Each run gets a terminal of its own (keyed by the run): what one run's terminal drew and how far
 * its stream had read never carry over to another run's, whoever renders this.
 */
export function TerminalView({ spaceId, runId, mode, onInput, resize }: Props) {
  const key = `${spaceId}/${runId}`
  return mode === 'live' ? <LiveTerminal key={key} spaceId={spaceId} runId={runId} onInput={onInput} resize={resize} /> : <ReplayTerminal key={key} spaceId={spaceId} runId={runId} />
}
