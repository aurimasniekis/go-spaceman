import type { Task } from '@/api/client'

/** Siblings of a task in the effective order (same parent). */
export function siblings(tasks: Task[], t: Task): Task[] {
  return tasks.filter((x) => (x.parent_id ?? '') === (t.parent_id ?? ''))
}

/** The move request that puts task t one place up or down among its siblings, or null at the edge. */
export function step(tasks: Task[], t: Task, dir: -1 | 1): { before_id?: string; after_id?: string } | null {
  const sib = siblings(tasks, t)
  const i = sib.findIndex((x) => x.id === t.id)
  const j = i + dir
  if (i < 0 || j < 0 || j >= sib.length) return null
  return dir < 0 ? { before_id: sib[j].id } : { after_id: sib[j].id }
}

/** The move request for dropping dragged before target (only among siblings). */
export function dropOn(dragged: Task, target: Task): { before_id: string } | null {
  if (dragged.id === target.id || (dragged.parent_id ?? '') !== (target.parent_id ?? '')) return null
  return { before_id: target.id }
}
