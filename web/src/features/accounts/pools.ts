/** The priority an entry without one has (the API: 0 or omitted is 100). */
export const DEFAULT_PRIORITY = 100

export type PoolEntry = { account_id: string; name?: string; priority?: number }

/** An entry's effective priority. */
export const priorityOf = (e: { priority?: number }) => (e.priority && e.priority > 0 ? e.priority : DEFAULT_PRIORITY)

/** Entries in failover order: lower priority first, ties in their given order (the binding order). */
export function inPoolOrder<T extends { priority?: number }>(items: T[]): T[] {
  return items.map((e, i) => ({ e, i })).sort((a, b) => priorityOf(a.e) - priorityOf(b.e) || a.i - b.i).map((x) => x.e)
}

/** Moves the entry at `from` by `delta` places. Out of range is a no-op. */
export function moveEntry<T>(items: T[], from: number, delta: number): T[] {
  const to = from + delta
  if (from < 0 || from >= items.length || to < 0 || to >= items.length) return items
  const next = items.slice()
  const [x] = next.splice(from, 1)
  next.splice(to, 0, x)
  return next
}

/** Writes an ordered list as priorities 10, 20, 30…: the order is explicit and there is room to insert between. */
export function withPriorities<T extends object>(ordered: T[]): (T & { priority: number })[] {
  return ordered.map((e, i) => ({ ...e, priority: (i + 1) * 10 }))
}
