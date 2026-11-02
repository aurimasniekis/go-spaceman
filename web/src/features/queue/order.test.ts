import type { Task } from '@/api/client'
import { dropOn, step } from './order'

const t = (id: string, parent = ''): Task => ({ id, parent_id: parent }) as unknown as Task

describe('queue order', () => {
  const tasks = [t('a'), t('a1', 'a'), t('a2', 'a'), t('b'), t('c')]
  it('steps among siblings only', () => {
    expect(step(tasks, tasks[3], -1)).toEqual({ before_id: 'a' })
    expect(step(tasks, tasks[3], 1)).toEqual({ after_id: 'c' })
    expect(step(tasks, tasks[0], -1)).toBeNull()
    expect(step(tasks, tasks[2], -1)).toEqual({ before_id: 'a1' })
    expect(step(tasks, tasks[2], 1)).toBeNull()
  })
  it('drops only onto siblings', () => {
    expect(dropOn(tasks[4], tasks[0])).toEqual({ before_id: 'a' })
    expect(dropOn(tasks[1], tasks[3])).toBeNull()
    expect(dropOn(tasks[4], tasks[4])).toBeNull()
  })
})
