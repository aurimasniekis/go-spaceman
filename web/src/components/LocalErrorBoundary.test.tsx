import { MantineProvider } from '@mantine/core'
import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LocalErrorBoundary } from './LocalErrorBoundary'

// jsdom has no matchMedia; MantineProvider reads the colour scheme from it.
beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})
afterEach(() => vi.restoreAllMocks())

let broken = true
function Widget() {
  if (broken) throw new Error('Duplicate options are not supported')
  return <p>widget</p>
}

describe('local error boundary', () => {
  it('keeps a failing part from blanking the page and lets it try again', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {}) // React logs the caught error
    render(
      <MantineProvider>
        <p>queue list</p>
        <LocalErrorBoundary title="This task could not be shown"><Widget /></LocalErrorBoundary>
      </MantineProvider>,
    )
    expect(screen.getByText('queue list')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('This task could not be shown')
    expect(screen.getByRole('alert')).toHaveTextContent('Duplicate options are not supported')
    broken = false
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(screen.getByText('widget')).toBeInTheDocument()
  })
})
