import { Text } from '@mantine/core'
import { useId, useState } from 'react'
import { compact, niceScale, OTHER, type Chart } from './series'

/**
 * Categorical slots in their fixed order (blue, orange, aqua, yellow, magenta, green, violet), each as a light and a
 * dark step validated against its surface; Other is neutral. A group's colour comes from its place in the sorted group
 * names, so it never depends on the data's rank. Text always wears text tokens: the colour only marks the segment.
 */
const SLOTS = [
  ['#2a78d6', '#3987e5'], ['#eb6834', '#d95926'], ['#1baf7a', '#199e70'], ['#eda100', '#c98500'],
  ['#e87ba4', '#d55181'], ['#008300', '#0ca30c'], ['#6250d6', '#9085e9'],
] as const
const OTHER_SLOT = ['#8a8a86', '#8d9aad'] as const

export const colorOf = (chart: Chart, group: string) => {
  const [l, d] = group === OTHER ? OTHER_SLOT : SLOTS[chart.groups.filter((g) => g !== OTHER).indexOf(group) % SLOTS.length]
  return `light-dark(${l}, ${d})`
}

const W = 720
const H = 260
const M = { top: 10, right: 8, bottom: 28, left: 44 }
const GAP = 2

/** The legend for two or more series: a swatch and the group's name in text colour. A single series needs none. */
export function UsageLegend({ chart }: { chart: Chart }) {
  if (chart.groups.length < 2) return null
  return (
    <ul aria-label="Legend" style={{ display: 'flex', flexWrap: 'wrap', gap: '4px 16px', listStyle: 'none', margin: '8px 0 0', padding: 0 }}>
      {chart.groups.map((g) => (
        <li key={g} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          <span aria-hidden style={{ width: 10, height: 10, borderRadius: 2, background: colorOf(chart, g), display: 'inline-block' }} />
          <Text span size="xs">{g}</Text>
        </li>
      ))}
    </ul>
  )
}

/**
 * Tokens per bucket as stacked bars. Hover or keyboard focus on a bar shows its figures; the same numbers are in the
 * table under the chart, which is the accessible view of it.
 */
export function UsageChart({ chart, label, bucket }: { chart: Chart; label: (iso: string) => string; bucket: 'hour' | 'day' }) {
  const [active, setActive] = useState<number | null>(null)
  const id = useId()
  const { top, ticks } = niceScale(chart.max)
  const iw = W - M.left - M.right
  const ih = H - M.top - M.bottom
  const n = chart.stacks.length
  const slot = iw / Math.max(1, n)
  const bw = Math.max(2, Math.min(28, slot * 0.7))
  const y = (v: number) => M.top + ih - (v / top) * ih
  const every = Math.ceil(n / (bucket === 'hour' ? 6 : 7))
  const tip = active !== null ? chart.stacks[active] : null
  return (
    <div style={{ position: 'relative', '--grid': 'light-dark(#dcdbd6, #3a3a38)', '--axis': 'light-dark(#52514e, #c3c2b7)', '--surface': 'var(--mantine-color-body)' } as React.CSSProperties}>
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" style={{ display: 'block', maxWidth: '100%', height: 'auto' }} role="group" aria-label="Tokens over time, stacked by group">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={M.left} x2={W - M.right} y1={y(t)} y2={y(t)} stroke="var(--grid)" strokeWidth={1} />
            <text x={M.left - 6} y={y(t)} textAnchor="end" dominantBaseline="middle" fontSize={11} fill="var(--axis)">{compact(t)}</text>
          </g>
        ))}
        {chart.stacks.map((s, i) => {
          const x = M.left + i * slot + (slot - bw) / 2
          let base = 0
          const desc = `${label(s.bucket)}: ${s.total.toLocaleString('en-US')} tokens${s.segments.length ? `. ${s.segments.map((g) => `${g.group} ${g.tokens.toLocaleString('en-US')}`).join(', ')}` : ''}`
          return (
            <g key={s.bucket} tabIndex={0} role="img" aria-label={desc} onMouseEnter={() => setActive(i)} onMouseLeave={() => setActive(null)} onFocus={() => setActive(i)} onBlur={() => setActive(null)}
              style={{ outline: 'none' }} className="usage-bar">
              {/* A hit target as tall as the plot, so a short bar is easy to reach. */}
              <rect x={M.left + i * slot} y={M.top} width={slot} height={ih} fill="transparent" />
              {s.segments.map((g) => {
                const h = Math.max(1, (g.tokens / top) * ih)
                const yy = y(base + g.tokens)
                base += g.tokens
                // The 2px surface gap between segments is a stroke in the surface colour.
                return <rect key={g.group} x={x} y={yy} width={bw} height={h} fill={colorOf(chart, g.group)} stroke="var(--surface)" strokeWidth={GAP / 2} rx={2} />
              })}
              {active === i && <rect x={M.left + i * slot} y={M.top} width={slot} height={ih} fill="none" stroke="var(--axis)" strokeWidth={1} strokeDasharray="3 3" />}
              {i % every === 0 && <text x={x + bw / 2} y={H - 8} textAnchor="middle" fontSize={11} fill="var(--axis)">{label(s.bucket)}</text>}
            </g>
          )
        })}
        <line x1={M.left} x2={W - M.right} y1={y(0)} y2={y(0)} stroke="var(--axis)" strokeWidth={1} />
      </svg>
      <div id={`${id}-tip`} role="status" aria-live="off" data-testid="usage-tooltip"
        style={{ position: 'absolute', top: 0, left: tip ? `clamp(0px, calc(${((M.left + (active! + 0.5) * slot) / W) * 100}% - 90px), calc(100% - 190px))` : 0, width: 180, padding: '6px 8px', borderRadius: 6,
          background: 'var(--mantine-color-body)', border: '1px solid var(--mantine-color-default-border)', boxShadow: 'var(--mantine-shadow-sm)', pointerEvents: 'none', display: tip ? 'block' : 'none', fontSize: 12 }}>
        {tip && (<>
          <Text size="xs" fw={600}>{label(tip.bucket)}</Text>
          <Text size="xs">{tip.total.toLocaleString('en-US')} tokens</Text>
          {tip.segments.map((g) => (
            <div key={g.group} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <span aria-hidden style={{ width: 8, height: 8, borderRadius: 2, background: colorOf(chart, g.group), flexShrink: 0 }} />
              <Text span size="xs" style={{ flex: 1, minWidth: 0 }} truncate>{g.group}</Text>
              <Text span size="xs">{compact(g.tokens)}</Text>
            </div>
          ))}
        </>)}
      </div>
    </div>
  )
}
