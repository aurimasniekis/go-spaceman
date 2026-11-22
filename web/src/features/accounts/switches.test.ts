import { describe, expect, it } from 'vitest'
import type { AccountSwitch } from '@/api/client'
import { reasonLabel, switchesOfRun } from './switches'

const sw = (o: Partial<AccountSwitch>) => ({ id: 'asw_1', from_run_id: 'run_a', state: 'succeeded', ...o }) as AccountSwitch

describe('switchesOfRun', () => {
  it('finds the switch away from a run and the one into it', () => {
    const all = [sw({ id: 'asw_2', from_run_id: 'run_b', new_run_id: 'run_c' }), sw({ id: 'asw_1', from_run_id: 'run_a', new_run_id: 'run_b' })]
    expect(switchesOfRun(all, 'run_b')).toEqual({ from: all[0], into: all[1] })
    expect(switchesOfRun(all, 'run_x')).toEqual({})
  })
})
describe('reasonLabel', () => {
  it('words a known reason and tolerates an unknown one', () => {
    expect(reasonLabel('reset_near')).toBe('the limit was about to reset')
    expect(reasonLabel('something_new')).toBe('something new')
    expect(reasonLabel('task_moved')).toBe("the run's task moved on")
    expect(reasonLabel()).toBe('')
  })
  it('words a switch-back with its own wording where it has one', () => {
    expect(reasonLabel('no_eligible_account', 'switch_back')).toBe('its original account was not available any more')
    expect(reasonLabel('no_eligible_account', 'usage_limit')).toBe('no other account of the pool was free')
    expect(reasonLabel('no_eligible_account')).toBe('no other account of the pool was free')
    expect(reasonLabel('reset_near', 'switch_back')).toBe('the limit was about to reset')
  })
})
