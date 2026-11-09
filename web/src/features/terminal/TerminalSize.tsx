import { Button, Group, Menu, NumberInput, Popover, Stack, Text, Tooltip } from '@mantine/core'
import { IconCheck, IconChevronDown, IconDimensions } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
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
 * The size menu of a live terminal. measure returns the size that fits the viewer's terminal area at the regular font
 * size (null before it can be measured). "Fit to window" refits while it stays selected and this page is open.
 */
export function TerminalSizeControl({ spaceId, run, blocker, measure, observe }: {
  spaceId: string
  run: Run
  /** Why the viewer cannot resize ('' when they can). */
  blocker: string
  measure: () => { cols: number; rows: number } | null
  /** The element whose size changes refit while "Fit to window" is applied. */
  observe: React.RefObject<HTMLElement | null>
}) {
  const qc = useQueryClient()
  const [choice, setChoice] = useState<SizeChoice>(loadChoice)
  const [fitting, setFitting] = useState(false)
  const [custom, setCustom] = useState(false)
  const [cols, setCols] = useState<number | string>(run.terminal_size?.cols ?? 120)
  const [rows, setRows] = useState<number | string>(run.terminal_size?.rows ?? 40)
  const [busy, setBusy] = useState(false)
  const path = { space_id: spaceId, run_id: run.id }
  const current = useRef(run.terminal_size)
  useEffect(() => {
    current.current = run.terminal_size
  }, [run.terminal_size])

  const put = useCallback(async (c: number, r: number) => {
    if (current.current && current.current.cols === c && current.current.rows === r) return
    setBusy(true)
    try {
      qc.setQueryData(keys.run(spaceId, run.id), unwrap(await api.PUT('/spaces/{space_id}/runs/{run_id}/terminal-size', {
        params: { path: { space_id: spaceId, run_id: run.id } }, body: { cols: c, rows: r } })))
    } catch (e) {
      notifyError(e, 'Could not resize the terminal')
      setFitting(false)
    } finally {
      setBusy(false)
    }
  }, [qc, spaceId, run.id])

  const apply = async (c: SizeChoice) => {
    setChoice(c)
    saveChoice(c)
    setCustom(false)
    setFitting(c.kind === 'fit')
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

  // Fit to window keeps fitting while it is applied: the window or the layout changed (debounced, so a drag sends one).
  useEffect(() => {
    const el = observe.current
    if (!fitting || blocker || !el) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const ro = new ResizeObserver(() => {
      clearTimeout(timer)
      timer = setTimeout(() => {
        const m = measure()
        if (m) void put(m.cols, m.rows)
      }, 400)
    })
    ro.observe(el)
    return () => {
      clearTimeout(timer)
      ro.disconnect()
    }
  }, [fitting, blocker, observe, measure, put])
  useEffect(() => {
    if (blocker) setFitting(false)
  }, [blocker])

  const selected = choiceKey(choice)
  const item = (c: SizeChoice, label: string) => (
    <Menu.Item key={choiceKey(c)} onClick={() => void apply(c)} leftSection={selected === choiceKey(c) ? <IconCheck size={14} /> : <span style={{ width: 14 }} />}
      data-testid={`terminal-size-${choiceKey(c)}`}>
      {label}
    </Menu.Item>
  )
  const customValid = typeof cols === 'number' && typeof rows === 'number' && inBounds(cols, rows)
  // An out-of-range number is shown as it is, with the field marked (not clamped silently).
  const colsBad = typeof cols === 'number' && (cols < BOUNDS.minCols || cols > BOUNDS.maxCols)
  const rowsBad = typeof rows === 'number' && (rows < BOUNDS.minRows || rows > BOUNDS.maxRows)
  const button = (
    <Button size="compact-xs" variant="default" leftSection={<IconDimensions size={14} />} rightSection={blocker ? undefined : <IconChevronDown size={12} />}
      disabled={!!blocker} loading={busy} aria-label={`Terminal size: ${sizeLabel(run)}`} data-testid="terminal-size">
      {sizeLabel(run)}{fitting && !blocker ? ' · fit' : ''}
    </Button>
  )
  if (blocker) return <Tooltip label={blocker} multiline w={260}><span>{button}</span></Tooltip>
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
              <Menu.Item onClick={() => setCustom(true)} leftSection={choice.kind === 'fixed' && !PRESETS.some(([c, r]) => c === choice.cols && r === choice.rows)
                ? <IconCheck size={14} /> : <span style={{ width: 14 }} />} data-testid="terminal-size-custom">
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
