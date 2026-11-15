import { Button, Group, Menu, NumberInput, Popover, Stack, Text, Tooltip, VisuallyHidden } from '@mantine/core'
import { IconCheck, IconChevronDown, IconDimensions } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { api, unwrap, type Run } from '@/api/client'
import { keys } from '@/api/queries'
import { notifyError } from '@/lib/notify'

/**
 * A run's terminal size (ADR-0075). The size belongs to the run: an operator with terminal takeover sets it (while the
 * terminal is taken over, only the holder) and the node holds the program at it, so the agent redraws for it. Everyone
 * watching sees the run at that size, scaled to their window. This browser remembers the last choice and shows it
 * selected, but never applies it on its own: opening a run's page must not resize it under everyone else.
 */

/** The bounds of a terminal size: the API, the space limits and the node's runner.terminal_size use the same. */
export const BOUNDS = { minCols: 40, maxCols: 400, minRows: 10, maxRows: 200 } as const

export const PRESETS: readonly (readonly [number, number])[] = [[80, 24], [120, 40], [160, 50], [200, 60]]

/** What the viewer chose: fit to their window, the node's default, or a fixed size. */
export type SizeChoice = { kind: 'fit' } | { kind: 'default' } | { kind: 'fixed'; cols: number; rows: number }

const STORAGE_KEY = 'sm.terminalSize'

export function inBounds(cols: number, rows: number): boolean {
  return Number.isInteger(cols) && Number.isInteger(rows) && cols >= BOUNDS.minCols && cols <= BOUNDS.maxCols && rows >= BOUNDS.minRows && rows <= BOUNDS.maxRows
}

export function choiceKey(c: SizeChoice): string {
  return c.kind === 'fixed' ? `${c.cols}x${c.rows}` : c.kind
}

export function parseChoice(s: string | null | undefined): SizeChoice {
  if (s === 'default') return { kind: 'default' }
  const m = /^(\d+)x(\d+)$/.exec(s ?? '')
  if (m && inBounds(Number(m[1]), Number(m[2]))) return { kind: 'fixed', cols: Number(m[1]), rows: Number(m[2]) }
  return { kind: 'fit' }
}

/** The remembered choice (fit to window when nothing, or nothing valid, is stored or storage is unavailable). */
export function loadChoice(): SizeChoice {
  try {
    return parseChoice(window.localStorage.getItem(STORAGE_KEY))
  } catch {
    return { kind: 'fit' }
  }
}

export function saveChoice(c: SizeChoice) {
  try {
    window.localStorage.setItem(STORAGE_KEY, choiceKey(c))
  } catch {
    // storage unavailable: remembered for this page only
  }
}

/** The columns and rows of cell×cell characters that fit width×height, within the bounds. */
export function fitDimensions(width: number, height: number, cellWidth: number, cellHeight: number): { cols: number; rows: number } | null {
  if (!(width > 0 && height > 0 && cellWidth > 0 && cellHeight > 0)) return null
  const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v))
  return {
    cols: clamp(Math.floor(width / cellWidth), BOUNDS.minCols, BOUNDS.maxCols),
    rows: clamp(Math.floor(height / cellHeight), BOUNDS.minRows, BOUNDS.maxRows),
  }
}

/** The run's size as shown: its held size, or the node's default. */
export function sizeLabel(run: Run): string {
  return run.terminal_size ? `${run.terminal_size.cols}×${run.terminal_size.rows}` : 'Node default'
}

/** Why this viewer cannot resize the run's terminal, or '' when they can. */
export function resizeBlocker(run: Run, canTakeOver: boolean, holder: string, ended: boolean): string {
  if (!canTakeOver) return 'Resizing needs terminal takeover permission'
  if (ended || run.desired !== 'running') return 'The run has ended'
  if (!run.terminal_sizing) return 'The run’s node agent is offline or too old to resize terminals (node protocol 1.10 needed)'
  if (run.takeover && run.takeover.holder !== holder) return `${run.takeover.holder_label || run.takeover.holder} has taken over this terminal; only they can resize it`
  return ''
}

/**
 * Makes a menu item a radio item (one size is chosen at a time). Mantine's Menu.Item always renders role="menuitem"
 * after the props it is given, so the role is set on the element; React leaves it alone because its own value never
 * changes.
 */
function asRadio(el: HTMLElement | null) {
  el?.setAttribute('role', 'menuitemradio')
}

/** The choice that shows a run's size as it is: its held size, or the node's default. */
export function choiceOf(size: Run['terminal_size']): SizeChoice {
  return size ? { kind: 'fixed', cols: size.cols, rows: size.rows } : { kind: 'default' }
}

/**
 * The size menu of a live terminal. measure returns the size that fits the viewer's terminal area at the regular font
 * size (null before it can be measured). "Fit to window" refits when this browser's window changes size, while it
 * stays selected and this page is open. It never refits because the terminal changed: the terminal box follows the
 * run's size, so refitting on it would undo someone else's resize. Once the run's size is not the one this browser
 * last sent (someone else resized it, or cleared it), this browser stops fitting and shows the run's size instead.
 */
export function TerminalSizeControl({ spaceId, run, blocker, measure }: {
  spaceId: string
  run: Run
  /** Why the viewer cannot resize ('' when they can). */
  blocker: string
  measure: () => { cols: number; rows: number } | null
}) {
  const qc = useQueryClient()
  const [choice, setChoice] = useState<SizeChoice>(loadChoice)
  const [fitting, setFitting] = useState(false)
  const [custom, setCustom] = useState(false)
  const [cols, setCols] = useState<number | string>(run.terminal_size?.cols ?? 120)
  const [rows, setRows] = useState<number | string>(run.terminal_size?.rows ?? 40)
  const [busy, setBusy] = useState(false)
  const path = { space_id: spaceId, run_id: run.id }
  const reasonId = useId()
  const current = useRef(run.terminal_size)
  useEffect(() => {
    current.current = run.terminal_size
  }, [run.terminal_size])
  // What this browser last asked for while fitting, and how many of its requests are still open.
  const sent = useRef<{ cols: number; rows: number } | null>(null)
  const pending = useRef(0)

  const put = useCallback(async (c: number, r: number) => {
    sent.current = { cols: c, rows: r }
    if (current.current && current.current.cols === c && current.current.rows === r) return
    setBusy(true)
    pending.current++
    try {
      qc.setQueryData(keys.run(spaceId, run.id), unwrap(await api.PUT('/spaces/{space_id}/runs/{run_id}/terminal-size', {
        params: { path: { space_id: spaceId, run_id: run.id } }, body: { cols: c, rows: r } })))
    } catch (e) {
      notifyError(e, 'Could not resize the terminal')
      setFitting(false)
    } finally {
      setBusy(false)
      pending.current--
    }
  }, [qc, spaceId, run.id])

  const apply = async (c: SizeChoice) => {
    setChoice(c)
    saveChoice(c)
    setCustom(false)
    setFitting(c.kind === 'fit')
    sent.current = null
    if (c.kind === 'fixed') return put(c.cols, c.rows)
    if (c.kind === 'fit') {
      const m = measure()
      if (m) return put(m.cols, m.rows)
      return
    }
    if (!current.current) return
    setBusy(true)
    try {
      qc.setQueryData(keys.run(spaceId, run.id), unwrap(await api.DELETE('/spaces/{space_id}/runs/{run_id}/terminal-size', { params: { path } })))
    } catch (e) {
      notifyError(e, 'Could not reset the terminal size')
    } finally {
      setBusy(false)
    }
  }

  // Fit to window keeps fitting while it is applied and this browser's window (or its visual viewport, such as a
  // phone's rotation) changes size, debounced so a drag sends one request.
  useEffect(() => {
    if (!fitting || blocker) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const refit = () => {
      clearTimeout(timer)
      timer = setTimeout(() => {
        const m = measure()
        if (m) void put(m.cols, m.rows)
      }, 400)
    }
    window.addEventListener('resize', refit)
    window.visualViewport?.addEventListener('resize', refit)
    return () => {
      clearTimeout(timer)
      window.removeEventListener('resize', refit)
      window.visualViewport?.removeEventListener('resize', refit)
    }
  }, [fitting, blocker, measure, put])
  useEffect(() => {
    if (blocker) setFitting(false)
  }, [blocker])
  // Someone else resized or cleared the run: their size stands. Stop fitting and show the run's size as selected. This
  // looks only at changes of the run's size, and none while this browser's own request is open: the response brings
  // this browser's size (a newer one from someone else arrives after it, and is caught then).
  const size = run.terminal_size
  const fittingRef = useRef(fitting)
  fittingRef.current = fitting
  useEffect(() => {
    const s = sent.current
    if (!fittingRef.current || pending.current > 0 || !s) return
    if (size && size.cols === s.cols && size.rows === s.rows) return
    setFitting(false)
    setChoice(choiceOf(size))
  }, [size?.cols, size?.rows]) // only on a change of the run's size

  const selected = choiceKey(choice)
  const item = (c: SizeChoice, label: string) => (
    <Menu.Item key={choiceKey(c)} onClick={() => void apply(c)} leftSection={selected === choiceKey(c) ? <IconCheck size={14} /> : <span style={{ width: 14 }} />}
      ref={asRadio} aria-checked={selected === choiceKey(c)} data-testid={`terminal-size-${choiceKey(c)}`}>
      {label}
    </Menu.Item>
  )
  const customChecked = choice.kind === 'fixed' && !PRESETS.some(([c, r]) => c === choice.cols && r === choice.rows)
  const customValid = typeof cols === 'number' && typeof rows === 'number' && inBounds(cols, rows)
  // An out-of-range number is shown as it is, with the field marked (not clamped silently).
  const colsBad = typeof cols === 'number' && (cols < BOUNDS.minCols || cols > BOUNDS.maxCols)
  const rowsBad = typeof rows === 'number' && (rows < BOUNDS.minRows || rows > BOUNDS.maxRows)
  const button = (
    <Button size="compact-xs" variant="default" leftSection={<IconDimensions size={14} />} rightSection={<IconChevronDown size={12} />}
      loading={busy} aria-label={`Terminal size: ${sizeLabel(run)}`} data-testid="terminal-size">
      {sizeLabel(run)}{fitting ? ' · fit' : ''}
    </Button>
  )
  // Blocked: the button stays focusable (aria-disabled, not disabled) and its reason is its description, so keyboard
  // and screen reader users get it as well as the tooltip.
  if (blocker) {
    return (
      <>
        <Tooltip label={blocker} multiline w={260}>
          <Button size="compact-xs" variant="default" leftSection={<IconDimensions size={14} />} data-disabled aria-disabled="true"
            aria-describedby={reasonId} aria-label={`Terminal size: ${sizeLabel(run)}`} onClick={(e) => e.preventDefault()} data-testid="terminal-size">
            {sizeLabel(run)}
          </Button>
        </Tooltip>
        <VisuallyHidden id={reasonId}>{blocker}</VisuallyHidden>
      </>
    )
  }
  return (
    <Popover opened={custom} onChange={setCustom} position="bottom-start" withinPortal trapFocus>
      <Popover.Target>
        <span>
          <Menu position="bottom-start" withinPortal>
            <Menu.Target>{button}</Menu.Target>
            <Menu.Dropdown>
              <Menu.Label>Size of the run’s terminal</Menu.Label>
              {item({ kind: 'fit' }, 'Fit to window')}
              {PRESETS.map(([c, r]) => item({ kind: 'fixed', cols: c, rows: r }, `${c} × ${r}`))}
              <Menu.Item onClick={() => setCustom(true)} leftSection={customChecked ? <IconCheck size={14} /> : <span style={{ width: 14 }} />}
                ref={asRadio} aria-checked={customChecked} data-testid="terminal-size-custom">
                Custom…
              </Menu.Item>
              <Menu.Divider />
              {item({ kind: 'default' }, 'Node default')}
              <Text size="xs" c="dimmed" px="sm" py={4} maw={240}>Everyone watching sees this size. The agent’s program redraws for it.</Text>
            </Menu.Dropdown>
          </Menu>
        </span>
      </Popover.Target>
      <Popover.Dropdown>
        <form onSubmit={(e) => { e.preventDefault(); if (customValid) void apply({ kind: 'fixed', cols: cols, rows: rows }) }}>
          <Stack gap="xs">
            <Group gap="xs" wrap="nowrap" align="flex-end">
              <NumberInput label="Columns" w={96} min={BOUNDS.minCols} max={BOUNDS.maxCols} clampBehavior="none" allowDecimal={false} allowNegative={false}
                value={cols} onChange={setCols} error={colsBad} data-testid="terminal-size-cols" />
              <Text pb={6}>×</Text>
              <NumberInput label="Rows" w={88} min={BOUNDS.minRows} max={BOUNDS.maxRows} clampBehavior="none" allowDecimal={false} allowNegative={false}
                value={rows} onChange={setRows} error={rowsBad} data-testid="terminal-size-rows" />
            </Group>
            <Text size="xs" c="dimmed">{BOUNDS.minCols}–{BOUNDS.maxCols} columns, {BOUNDS.minRows}–{BOUNDS.maxRows} rows</Text>
            <Group justify="flex-end" gap="xs">
              <Button size="xs" variant="default" onClick={() => setCustom(false)}>Cancel</Button>
              <Button size="xs" type="submit" disabled={!customValid} data-testid="terminal-size-apply">Apply</Button>
            </Group>
          </Stack>
        </form>
      </Popover.Dropdown>
    </Popover>
  )
}
