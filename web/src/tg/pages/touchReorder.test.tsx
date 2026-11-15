import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { Task } from '@/api/client'
import { dropBody, REORDER_ATTR, useTouchReorder } from './touchReorder'

const t = (id: string, parent?: string) => ({ id, parent_id: parent }) as Task

describe('dropBody', () => {
  const tasks = [t('a'), t('b'), t('c'), t('x', 'a')]
  it('goes after a lower target and before a higher one, so the last place is reachable', () => {
    expect(dropBody(tasks, 'a', 'c')).toEqual({ after_id: 'c' })
    expect(dropBody(tasks, 'c', 'a')).toEqual({ before_id: 'a' })
  })
  it('refuses itself, other parents and unknown ids', () => {
    expect(dropBody(tasks, 'a', 'a')).toBeNull()
    expect(dropBody(tasks, 'a', 'x')).toBeNull()
    expect(dropBody(tasks, 'a', 'nope')).toBeNull()
  })
})

function Probe({ onDrop }: { onDrop: (a: string, b: string) => void }) {
  const r = useTouchReorder(onDrop)
  return (
    <div>
      {['a', 'b'].map((id) => (
        <div key={id} {...{ [REORDER_ATTR]: id }} data-testid={`row-${id}`}>
          <button aria-label={`grip ${id}`} {...r.handle(id)} />
        </div>
      ))}
      <output>{`${r.dragging}>${r.over}`}</output>
    </div>
  )
}

describe('useTouchReorder', () => {
  it('reports one drop of the dragged row on the row under the finger', () => {
    const onDrop = vi.fn()
    document.elementFromPoint = () => screen.getByTestId('row-b')
    render(<Probe onDrop={onDrop} />)
    const grip = screen.getByLabelText('grip a')
    fireEvent.pointerDown(grip, { pointerType: 'touch', pointerId: 1 })
    fireEvent.pointerMove(grip, { pointerType: 'touch', pointerId: 1, clientX: 5, clientY: 300 })
    expect(screen.getByRole('status')).toHaveTextContent('a>b')
    fireEvent.pointerUp(grip, { pointerType: 'touch', pointerId: 1 })
    expect(onDrop).toHaveBeenCalledExactlyOnceWith('a', 'b')
    expect(screen.getByRole('status')).toHaveTextContent('null>null')
  })
  it('ignores the mouse (native drag handles it) and a cancelled touch', () => {
    const onDrop = vi.fn()
    document.elementFromPoint = () => screen.getByTestId('row-b')
    render(<Probe onDrop={onDrop} />)
    const grip = screen.getByLabelText('grip a')
    fireEvent.pointerDown(grip, { pointerType: 'mouse', pointerId: 1 })
    fireEvent.pointerUp(grip, { pointerType: 'mouse', pointerId: 1 })
    fireEvent.pointerDown(grip, { pointerType: 'touch', pointerId: 2 })
    fireEvent.pointerMove(grip, { pointerType: 'touch', pointerId: 2, clientY: 300 })
    fireEvent.pointerCancel(grip, { pointerType: 'touch', pointerId: 2 })
    expect(onDrop).not.toHaveBeenCalled()
  })
})
