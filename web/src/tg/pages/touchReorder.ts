import { useCallback, useRef, useState, type PointerEvent } from 'react'
import type { Task } from '@/api/client'
import { siblings } from '@/features/queue/order'

/** The attribute a row carries so the pointer can find the row under the finger. */
export const REORDER_ATTR = 'data-reorder-id'

/**
 * The move request for dropping `from` on `over`: after the target when it moves down, before it when it moves up, so a
 * task can reach the last place too. Only among siblings (the server refuses other moves); null for a no-op.
 */
export function dropBody(tasks: Task[], from: string, over: string): { before_id?: string; after_id?: string } | null {
  const a = tasks.find((t) => t.id === from)
  const b = tasks.find((t) => t.id === over)
  if (!a || !b || a.id === b.id || (a.parent_id ?? '') !== (b.parent_id ?? '')) return null
  const sib = siblings(tasks, a)
  const i = sib.findIndex((t) => t.id === a.id)
  const j = sib.findIndex((t) => t.id === b.id)
  return i < j ? { after_id: b.id } : { before_id: b.id }
}

const EDGE = 72
const SPEED = 14

/**
 * Touch (and pen) reordering with pointer events: HTML5 drag and drop does not start from a finger. A grip handle
 * captures the pointer (touch-action: none keeps the page from scrolling under it), the row under the finger is found by
 * its REORDER_ATTR, the page scrolls near its edges, and the drop is reported once on release. Mouse input is left to the
 * rows' native drag, and the up/down buttons and Alt+arrows stay for everyone else.
 */
export function useTouchReorder(onDrop: (from: string, over: string) => void) {
  const [drag, setDrag] = useState<{ from: string; over: string | null } | null>(null)
  const state = useRef(drag)
  state.current = drag
  const set = (d: { from: string; over: string | null } | null) => {
    state.current = d
    setDrag(d)
  }

  const handle = useCallback(
    (id: string) => ({
      style: { touchAction: 'none' as const, cursor: 'grab' },
      onPointerDown: (e: PointerEvent<HTMLElement>) => {
        if (e.pointerType === 'mouse') return
        try {
          e.currentTarget.setPointerCapture?.(e.pointerId)
        } catch {
          // No active pointer to capture (a synthetic event); the handle still sees its own moves.
        }
        set({ from: id, over: null })
      },
      onPointerMove: (e: PointerEvent<HTMLElement>) => {
        const d = state.current
        if (!d || d.from !== id) return
        const row = document.elementFromPoint(e.clientX, e.clientY)?.closest(`[${REORDER_ATTR}]`)
        const over = row?.getAttribute(REORDER_ATTR) ?? null
        if (over !== d.over) set({ from: d.from, over })
        if (e.clientY < EDGE) window.scrollBy(0, -SPEED)
        else if (e.clientY > window.innerHeight - EDGE) window.scrollBy(0, SPEED)
      },
      onPointerUp: () => {
        const d = state.current
        set(null)
        if (d?.over && d.over !== d.from) onDrop(d.from, d.over)
      },
      onPointerCancel: () => set(null),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `set` only touches a ref and a state setter
    [onDrop],
  )
  return { handle, dragging: drag?.from ?? null, over: drag?.over ?? null }
}
