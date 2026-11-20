import type { UsageRow } from '@/api/client'
import { RANGES, type RangeKey } from './range'

export type GroupBy = 'space' | 'account' | 'model' | 'role' | 'harness'
export type FilterKey = 'space' | 'account' | 'model' | 'harness'
export const FILTER_KEYS: FilterKey[] = ['space', 'account', 'model', 'harness']
export const GROUPS: { value: GroupBy; label: string }[] = [
  { value: 'space', label: 'Space' }, { value: 'account', label: 'Account' }, { value: 'model', label: 'Model' }, { value: 'role', label: 'Role' }, { value: 'harness', label: 'Harness' },
]
/** The API takes at most 50 values per filter. */
export const MAX_FILTER = 50
export const DEFAULT_RANGE: RangeKey = '7d'

/** What the URL holds: strings only, filters as comma-separated lists, defaults left out so a link stays short. */
export interface UsageSearch { range?: RangeKey; group?: GroupBy; space?: string; account?: string; model?: string; harness?: string }

export interface UsageState { range: RangeKey; group: GroupBy; space: string[]; account: string[]; model: string[]; harness: string[] }

const list = (v: unknown): string[] => {
  if (typeof v !== 'string') return []
  return [...new Set(v.split(',').map((x) => x.trim()).filter(Boolean))].slice(0, MAX_FILTER)
}

/** Reads a router search object defensively: anything unknown is dropped, never thrown (a URL is user input). */
export function parseUsageSearch(s: Record<string, unknown>): UsageSearch {
  const out: UsageSearch = {}
  if (RANGES.some((r) => r.value === s.range)) out.range = s.range as RangeKey
  if (GROUPS.some((g) => g.value === s.group)) out.group = s.group as GroupBy
  for (const k of FILTER_KEYS) { const l = list(s[k]); if (l.length) out[k] = l.join(',') }
  return out
}

export function resolveUsage(s: UsageSearch, defaultGroup: GroupBy = 'space', allowed?: GroupBy[]): UsageState {
  const group = s.group && (!allowed || allowed.includes(s.group)) ? s.group : defaultGroup
  return { range: s.range ?? DEFAULT_RANGE, group, space: list(s.space), account: list(s.account), model: list(s.model), harness: list(s.harness) }
}

/** The URL form of a state: defaults and empty filters are omitted. */
export function serializeUsage(st: UsageState, defaultGroup: GroupBy = 'space'): UsageSearch {
  const out: UsageSearch = {}
  if (st.range !== DEFAULT_RANGE) out.range = st.range
  if (st.group !== defaultGroup) out.group = st.group
  for (const k of FILTER_KEYS) if (st[k].length) out[k] = st[k].slice(0, MAX_FILTER).join(',')
  return out
}

/** Adds `value` to a filter (a group's key from the table), or removes it when it is already there. */
export function toggleFilter(st: UsageState, key: FilterKey, value: string): UsageState {
  const cur = st[key]
  const next = cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value].slice(0, MAX_FILTER)
  return { ...st, [key]: next }
}

/** The filter a group-by's rows set when clicked; role rows set none (the API has the filter, the page does not offer it). */
export const filterOf = (g: GroupBy): FilterKey | null => (g === 'role' ? null : g)

const SUM = ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens', 'requests', 'total_tokens', 'total_cost_usd'] as const

/** One row per group over the whole range, biggest first (the table under the chart). */
export function perGroup(rows: UsageRow[]): UsageRow[] {
  const m = new Map<string, UsageRow>()
  for (const r of rows) {
    const a = m.get(r.group)
    if (!a) { m.set(r.group, { ...r, bucket_start: undefined }); continue }
    for (const k of SUM) a[k] += r[k]
  }
  return [...m.values()].sort((a, b) => b.total_tokens - a.total_tokens || a.group.localeCompare(b.group))
}

const HARNESS_NAME: Record<string, string> = { codex: 'Codex', 'claude-code': 'Claude Code', 'harbor-pilot': 'Harbor Pilot' }
export const harnessName = (h: string) => HARNESS_NAME[h] ?? h

export interface NotCounted { harness: string; runs: number; harbor_pilot_runs: number }

/** The "not counted" note's lines: harnesses with runs only (zero is not a finding), largest first. */
export function notCountedLines(items: NotCounted[] | undefined): string[] {
  return (items ?? []).filter((i) => i.runs > 0).sort((a, b) => b.runs - a.runs || a.harness.localeCompare(b.harness)).map((i) => {
    const runs = `${i.runs.toLocaleString('en-US')} ${harnessName(i.harness)} ${i.runs === 1 ? 'run' : 'runs'}`
    return i.harbor_pilot_runs > 0 ? `${runs} (${i.harbor_pilot_runs.toLocaleString('en-US')} read back from Harbor Pilot)` : runs
  })
}
