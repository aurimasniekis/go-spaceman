import { describe, expect, it } from 'vitest'
import type { Run } from '@/api/client'
import { agentMessageBody, formatDuration, runLabels, runStateText, runTitle, runTone, sortRuns } from './runInfo'

const base = { id: 'run_1', role: 'control', kind: 'control', harness: 'claude-code', observed: 'running', desired: 'running', turn: 0,
  created_at: '2026-11-06T10:00:00Z', started_at: '2026-11-06T10:00:00Z' } as unknown as Run
const run = (p: Partial<Run>): Run => ({ ...base, ...p }) as Run
const now = Date.parse('2026-11-06T10:12:30Z')

describe('run display', () => {
  it('derives one tone from the observed state and the program status', () => {
    expect(runTone(run({ status: 'working' }))).toBe('working')
    expect(runTone(run({ status: 'idle' }))).toBe('idle')
    expect(runTone(run({ status: 'blocked' }))).toBe('attention')
    expect(runTone(run({ status_kind: 'permission' }))).toBe('attention')
    expect(runTone(run({ observed: 'starting' }))).toBe('starting')
    expect(runTone(run({ observed: 'unknown' }))).toBe('offline')
    expect(runTone(run({ observed: 'failed' }))).toBe('failed')
    expect(runTone(run({ observed: 'exited' }))).toBe('ended')
  })
  it('says the state in words', () => {
    expect(runStateText(run({ status: 'working' }), now)).toBe('working · 12m')
    expect(runStateText(run({ status: 'done' }), now)).toBe('idle')
    expect(runStateText(run({ status_kind: 'permission' }), now)).toBe('waiting for permission')
    expect(runStateText(run({ observed: 'exited', ended_at: '2026-11-06T10:10:30Z', end_reason: 'killed' }), now)).toBe('exited 2m ago · killed')
    expect(runStateText(run({ observed: 'unknown' }), now)).toBe('node offline')
  })
  it('formats durations in their two largest units', () => {
    expect(formatDuration(45_000)).toBe('45s')
    expect(formatDuration(750_000)).toBe('12m')
    expect(formatDuration(330_000)).toBe('5m 30s')
    expect(formatDuration(3 * 3600_000 + 5 * 60_000)).toBe('3h 5m')
    expect(formatDuration(2 * 86400_000 + 4 * 3600_000)).toBe('2d 4h')
  })
  it('titles, numbers duplicates and orders live agents first', () => {
    expect(runTitle(run({ turn: 4 }))).toBe('control #4')
    const a = run({ id: 'a', role: 'kb-support', kind: 'kb' }), b = run({ id: 'b', role: 'kb-support', kind: 'kb' }), c = run({ id: 'c', role: 'control' })
    expect([...runLabels([a, b, c]).values()]).toEqual(['kb-support (1)', 'kb-support (2)', 'control'])
    const ended = run({ id: 'e', observed: 'exited', role: 'old' })
    expect(sortRuns([ended, a, c]).map((r) => r.id)).toEqual(['c', 'a', 'e'])
  })
})

describe('message the agent', () => {
  it('forces delivery so a busy program still receives the message', () => {
    expect(agentMessageBody('stop and read the plan')).toEqual({ text: 'stop and read the plan', force: true })
  })
})
