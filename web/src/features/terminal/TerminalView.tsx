import { ActionIcon, Badge, Box, Button, Group, Loader, SegmentedControl, Slider, Stack, Text, Tooltip } from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconDownload, IconPlayerPauseFilled, IconPlayerPlayFilled, IconPlayerSkipBackFilled } from '@tabler/icons-react'
import type { Terminal } from 'ghostty-web'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, unwrap, type RecordingPage, type RecordingRecord, type RecordingTail, type Run } from '@/api/client'
import { API_PREFIX } from '@/api/session'
import { endReason, sessionStillValid } from '@/api/streams'
import { clock } from '@/lib/format'
import { createTerminal, disposeTerminal, fitFont, followCursor, holdDrawing, regularCell, rowHeight, scrollWithWheelAndTouch } from './ghostty'
import { jumpToLive, keepingPosition, scrolledAway } from './scroll'
import {
  apply,
  emptySummary,
  feed,
  type Feed,
  indexAt,
  MAX_IDLE_MS,
  nextTail,
  offsetMs,
  type RecordingSummary,
  ReplayIndex,
  SLICE_MS,
  TAIL_BYTES,
  tailRecords,
} from './records'
import { scrollbackRows } from './scrollback'
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
  // At the top of the scrollback, which still has room for earlier output (ADR-0081: where loading it is offered).
  const [atTop, setAtTop] = useState(false)
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
        // The box at its top, and the scrollback at its top (or empty: the output so far fits the screen).
        const kept = term.getScrollbackLength()
        setAtTop(term.buffer.active.type !== 'alternate' && el.scrollTop <= 1 && term.viewportY >= kept - 1 && kept < scrollbackRows(term.cols))
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
  return { away, atTop, jump }
}

/**
 * Returns a function to call with the backlog after each slice of a feed: the terminal stops drawing while a backlog
 * is left and draws once when it is gone (holdDrawing). Call it with 0 when the feed stops.
 */
function catchingUp(term: Terminal): (backlog: number) => void {
  let release: (() => void) | null = null
  return (backlog) => {
    if (backlog > 0) release ??= holdDrawing(term)
    else if (release) {
      release()
      release = null
    }
  }
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

function StatusBadges({ sum, live, typing, loading }: { sum: RecordingSummary; live?: LiveState; typing?: boolean; loading?: boolean }) {
  return (
    <Group gap={6}>
      {loading && (
        <Badge color="gray" variant="light" leftSection={<Loader size={10} color="currentColor" />} data-testid="terminal-loading">
          Loading output…
        </Badge>
      )}
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
    // The box's outer width (less its padding and border), not clientWidth: a vertical scrollbar that comes and goes
    // with the content would change the fit by a column.
    return fitDimensions(box.current.getBoundingClientRect().width - 14, areaHeight(), cell.width, cell.height)
  }, [term, box])
  const { away, atTop, jump } = useScrolledAway(term, box)
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
  const [loading, setLoading] = useState(false)
  const [attempt, setAttempt] = useState(0)
  // Streams in a row that the server refused (reset by one that opens).
  const failures = useRef(0)
  const visible = useTabVisible()
  // What this terminal has drawn, kept across reconnects so output is never applied twice.
  const drawn = useRef<RecordingSummary>(emptySummary())
  // How much stored output the first catch-up asks for (ADR-0081): the end of the recording, so the current screen
  // shows without downloading the whole history. 0 asks for all of it.
  const tailBytes = useRef(TAIL_BYTES)
  // Bytes of output before what this terminal drew (the server's tail event); while above 0, earlier output can be
  // loaded when the viewer reaches the top of the scrollback.
  const [earlier, setEarlier] = useState(0)
  // Set while earlier output loads: the viewer's lines from the bottom, restored once it is drawn.
  const reload = useRef<number | null>(null)
  const [reloads, setReloads] = useState(0)
  const [reloading, setReloading] = useState(false)
  const loadEarlier = useCallback(() => {
    if (!term || reload.current !== null) return
    reload.current = Math.round(term.viewportY)
    tailBytes.current = nextTail(tailBytes.current)
    setReloading(true)
    setReloads((n) => n + 1)
  }, [term])
  useEffect(() => {
    // A hidden tab releases the stream; coming back resumes after the last record drawn.
    if (!term || !visible) return
    let closed = false
    const catchUp = catchingUp(term)
    if (reload.current !== null) {
      // Earlier output: draw the larger tail on a clean terminal, keeping the old picture until it is done.
      catchUp(1)
      term.reset()
      drawn.current = emptySummary()
    }
    const after = drawn.current.lastSeq
    // The stored output arrives first, all at once: it is applied a slice at a time (feed), showing "Loading output…"
    // meanwhile. A viewer reading earlier output stays where they are while new output arrives.
    let shown = 0
    const fed = feed(term, drawn.current, (s, backlog) => {
      drawn.current = s
      catchUp(backlog)
      if (backlog === 0 && reload.current !== null) {
        // Back to the lines the viewer was reading, now with earlier output above them.
        term.scrollToBottom()
        term.scrollLines(-reload.current)
        reload.current = null
        setReloading(false)
      }
      // While catching up, the badges are updated twice a second rather than after every slice (each update renders).
      if (backlog > 0 && performance.now() - shown < 500) return
      shown = performance.now()
      setSum(s)
      setLoading(backlog > 0)
    }, { wrap: (writes) => keepingPosition(term, writes) })
    let queued = after
    let first = true
    const query = after > 0 ? `?after=${after}` : tailBytes.current > 0 ? `?tail=${tailBytes.current}` : ''
    const es = new EventSource(`${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/runs/${encodeURIComponent(runId)}/terminal${query}`)
    let retry: ReturnType<typeof setTimeout> | undefined
    es.onopen = () => {
      failures.current = 0
      setState('live')
    }
    es.onerror = () => {
      if (es.readyState !== EventSource.CLOSED) {
        setState('connecting')
        return
      }
      // The server answered with an error (central busy reading recordings answers 503, ADR-0081), which the browser
      // never retries: try again, backing off, a few times.
      if (failures.current >= 8) {
        setState('ended')
        return
      }
      setState('connecting')
      const delay = Math.min(30_000, 1_000 * 2 ** failures.current++)
      retry = setTimeout(() => !closed && setAttempt((n) => n + 1), delay)
    }
    // A tail catch-up starts with what the output before it left: the size, modes, gaps (records with seq 0). A server
    // without tails sends none and the whole recording follows, as before.
    let sawTail = false
    es.addEventListener('tail', (ev) => {
      // One per stream; another would count its gaps twice.
      if (sawTail) return
      sawTail = true
      const tail = JSON.parse((ev as MessageEvent<string>).data) as RecordingTail
      setEarlier(tail.skipped_bytes)
      if (first) catchUp(1)
      first = false
      fed.push(tailRecords(tail))
    })
    es.addEventListener('record', (ev) => {
      const r = JSON.parse((ev as MessageEvent<string>).data) as RecordingRecord
      // EventSource resumes after a reconnect with Last-Event-ID, so a record is never applied twice.
      if (r.seq <= queued) return
      queued = r.seq
      // The whole recording, with no tail event before it: nothing comes before what this terminal draws.
      if (first && after === 0) setEarlier(0)
      // Drawing is held from the stream's first record (the stored output starts there; its first slice resizes the
      // terminal, which would draw it at once). Later, only a backlog left after a slice holds it.
      if (first) catchUp(1)
      first = false
      fed.push([r])
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
      clearTimeout(retry)
      es.close()
      // Records not applied yet are dropped; the next stream resumes after the last one applied.
      fed.stop()
      catchUp(0)
      setLoading(false)
    }
  }, [term, spaceId, runId, attempt, visible, reloads])
  return (
    <Stack gap="xs" style={{ minWidth: 0 }}>
      <Group justify="space-between" gap="xs" wrap="wrap">
        <StatusBadges sum={sum} live={state} typing={!!onInput} loading={loading || (!term && !error)} />
        {resize && <TerminalSizeControl spaceId={spaceId} run={resize.run} blocker={resize.blocker} measure={measure} />}
      </Group>
      {state === 'denied' && <Text c="owlRed" size="sm">Your access to this terminal was removed.</Text>}
      {error && <Text c="owlRed" size="sm">Terminal failed to load: {error}</Text>}
      <Box pos="relative" style={{ minWidth: 0 }}>
        <Box ref={box} className="sm-terminal" data-earlier={earlier} data-reloading={reloading} style={{ overflow: 'auto', maxHeight: '70vh', overscrollBehavior: 'contain' }}>
          <div ref={ref} data-testid="terminal" aria-label={onInput ? 'Run terminal (taken over: your keys go to the program)' : 'Run terminal (watch-only)'} />
        </Box>
        {atTop && earlier > 0 && !loading && !reloading && (
          <Button
            size="xs"
            radius="xl"
            variant="default"
            leftSection={<IconArrowUp size={14} />}
            onClick={loadEarlier}
            data-testid="load-earlier"
            style={{ position: 'absolute', top: 12, left: '50%', transform: 'translateX(-50%)', zIndex: 2, boxShadow: 'var(--mantine-shadow-md)' }}
          >
            Load earlier output
          </Button>
        )}
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

/**
 * One page of a run's recording. Central reads a bounded number of recordings at once (ADR-0081) and answers 503 with
 * Retry-After when it is busy: the page is asked for again after that, a few times, while the view is open.
 */
async function recordingPage(
  spaceId: string,
  runId: string,
  query: { after?: number; limit?: number; tail?: number },
  cancelled: () => boolean,
): Promise<RecordingPage> {
  for (let attempt = 1; ; attempt++) {
    const res = await api.GET('/spaces/{space_id}/runs/{run_id}/recording', { params: { path: { space_id: spaceId, run_id: runId }, query } })
    if (res.response.status !== 503 || attempt >= 10 || cancelled()) return unwrap(res)
    const wait = Number(res.response.headers.get('Retry-After')) || 2
    await new Promise((r) => setTimeout(r, Math.min(wait, 30) * 1000))
  }
}

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

  // The end of the recording first (ADR-0081): its last screen shows while the rest loads for play and seeking.
  const [tailPage, setTailPage] = useState<RecordingPage | null>(null)
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const first = await recordingPage(spaceId, runId, { tail: TAIL_BYTES }, () => cancelled)
      // A server without tails answers with the first page: then the whole recording loads first, as before.
      if (!cancelled && first.tail) setTailPage(first)
      const all: RecordingRecord[] = []
      let after = 0
      // Pages of 1000 records: the browser parses each response in one task (5000 took about a second at 4× throttling).
      for (;;) {
        const page = await recordingPage(spaceId, runId, { after, limit: 1000 }, () => cancelled)
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

  // Seeking renders from a clean screen (terminal state is not invertible), a slice at a time (feed): a long run's
  // recording is too much output to apply in one go. It starts at most TAIL_BYTES of output before the position, after
  // the size and modes in effect there (ReplayIndex, the same tail a live terminal starts from). Playback waits until
  // the screen has caught up.
  const [rendering, setRendering] = useState(false)
  const index = useMemo(() => (records ? new ReplayIndex(records) : null), [records])
  const feedRef = useRef<{ fed: Feed; catchUp: (backlog: number) => void } | null>(null)
  const stopFeed = useCallback(() => {
    feedRef.current?.fed.stop()
    feedRef.current?.catchUp(0)
    feedRef.current = null
  }, [])
  /** Draws a tail on a clean screen: what comes before it, then its records; done runs once all of it is drawn. */
  const render = useCallback(
    (tail: RecordingTail, recs: readonly RecordingRecord[], done?: () => void) => {
      if (!term) return
      stopFeed()
      term.reset()
      setRendering(true)
      const catchUp = catchingUp(term)
      catchUp(1)
      const fed = feed(term, emptySummary(), (s, backlog) => {
        catchUp(backlog)
        if (backlog > 0) return
        feedRef.current = null
        sumRef.current = s
        setSum(s)
        setRendering(false)
        done?.()
      })
      feedRef.current = { fed, catchUp }
      fed.push([...tailRecords(tail), ...recs])
    },
    [term, stopFeed],
  )
  const seek = useCallback(
    (to: number) => {
      if (!term || !records || !index) return
      stopFeed()
      const end = Math.min(to, records.length)
      posRef.current = end
      setPos(end)
      if (end === 0) {
        term.reset()
        sumRef.current = emptySummary()
        setSum(sumRef.current)
        setRendering(false)
        return
      }
      setRendering(true)
      // Index the modes up to the window's start a slice at a time, then draw the window.
      const start = index.windowStart(end, TAIL_BYTES)
      // Holds the seek's place until the window is drawn: a later seek or stopFeed replaces it, which cancels this one.
      const pending = { fed: { push() {}, backlog: () => 0, stop() {} } as Feed, catchUp: () => {} }
      feedRef.current = pending
      const step = () => {
        if (feedRef.current !== pending) return
        if (!index.advance(start, SLICE_MS)) {
          setTimeout(step, 0)
          return
        }
        const { records: window, tail } = index.tail(end, TAIL_BYTES)
        render(tail, window)
      }
      step()
    },
    [term, records, index, stopFeed, render],
  )
  useEffect(() => stopFeed, [term, records, stopFeed])

  // The tail's last screen while the rest of the recording loads. tailShown is the last record it drew, set only once
  // all of it is drawn: the whole recording can arrive first (a slow device), and then stops the tail's feed.
  const tailShown = useRef<number | null>(null)
  useEffect(() => {
    if (!term || !tailPage?.tail || records) return
    tailShown.current = null
    render(tailPage.tail, tailPage.records, () => (tailShown.current = tailPage.next_after))
  }, [term, tailPage, records, render])

  // Show the whole recording first (the most useful state of a finished run); play restarts from the beginning. When
  // the tail already shows it, only the position and badges are set.
  useEffect(() => {
    if (!term || !records) return
    const last = records.length > 0 ? records[records.length - 1].seq : 0
    if (tailShown.current !== null && tailShown.current === last) {
      posRef.current = records.length
      setPos(records.length)
      let s = emptySummary()
      for (const r of records) s = apply(null, s, r)
      sumRef.current = s
      setSum(s)
      return
    }
    seek(records.length)
  }, [term, records, seek])

  useEffect(() => {
    if (!playing || rendering || !term || !records) return
    let timer: ReturnType<typeof setTimeout>
    const step = () => {
      // A seek is rendering from a clean screen: a step scheduled before it (its timer can fire before this effect is
      // cleaned up) would write a record into the middle of that. Playback restarts when the seek is done (rendering).
      if (feedRef.current) return
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
  }, [playing, rendering, speed, term, records])

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
      <StatusBadges sum={sum} loading={rendering || (!term && !error)} />
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
