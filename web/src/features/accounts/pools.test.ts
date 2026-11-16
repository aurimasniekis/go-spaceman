import { describe, expect, it } from 'vitest'
import { inPoolOrder, moveEntry, priorityOf, withPriorities } from './pools'

describe('pool order', () => {
  it('treats a missing or zero priority as 100', () => {
    expect(priorityOf({})).toBe(100)
    expect(priorityOf({ priority: 0 })).toBe(100)
    expect(priorityOf({ priority: 5 })).toBe(5)
  })
  it('orders by priority, ties by the given order', () => {
    const items = [{ id: 'a', priority: 100 }, { id: 'b' }, { id: 'c', priority: 10 }]
    expect(inPoolOrder(items).map((x) => x.id)).toEqual(['c', 'a', 'b'])
  })
  it('moves an entry and ignores moves off the ends', () => {
    expect(moveEntry(['a', 'b', 'c'], 2, -1)).toEqual(['a', 'c', 'b'])
    expect(moveEntry(['a', 'b', 'c'], 0, -1)).toEqual(['a', 'b', 'c'])
    expect(moveEntry(['a', 'b', 'c'], 2, 1)).toEqual(['a', 'b', 'c'])
  })
  it('numbers an order 10, 20, 30', () => {
    expect(withPriorities([{ account_id: 'x' }, { account_id: 'y' }]).map((e) => e.priority)).toEqual([10, 20])
  })
})
