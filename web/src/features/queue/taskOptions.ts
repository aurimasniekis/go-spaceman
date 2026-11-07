import type { Task } from '@/api/client'

/**
 * Picker options for tasks. Titles are not unique (a re-filed task keeps its title), so an option's value is always the
 * task ID and the title is only its label. Mantine rejects duplicate values; duplicate labels would be legal but
 * indistinguishable, so a title shared by several tasks also shows each task's status and the end of its ID.
 */
export function taskOptions(tasks: Pick<Task, 'id' | 'title' | 'status'>[]): { value: string; label: string }[] {
  const count = new Map<string, number>()
  for (const t of tasks) count.set(t.title, (count.get(t.title) ?? 0) + 1)
  const seen = new Set<string>()
  const out: { value: string; label: string }[] = []
  for (const t of tasks) {
    if (seen.has(t.id)) continue
    seen.add(t.id)
    out.push({ value: t.id, label: (count.get(t.title) ?? 0) > 1 ? `${t.title} (${t.status}, …${t.id.slice(-6)})` : t.title })
  }
  return out
}
