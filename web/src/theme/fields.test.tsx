import { MantineProvider, NumberInput, Select, Textarea, TextInput } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { theme } from './theme'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

/** The wrapper's parts in document order, by Mantine's static class names. */
function parts(label: string): string[] {
  const input = screen.getAllByLabelText(label)[0]
  const root = input.closest('.mantine-InputWrapper-root')!
  return [...root.querySelectorAll('.mantine-InputWrapper-label, .mantine-InputWrapper-description, .mantine-InputWrapper-error, .mantine-Input-wrapper')]
    .filter((el) => el.parentElement === root)
    .map((el) => ['label', 'description', 'error'].find((p) => el.classList.contains(`mantine-InputWrapper-${p}`)) ?? 'input')
}

describe('field descriptions', () => {
  it('go under the input, so fields in one row line up; the error comes last', () => {
    render(
      <MantineProvider theme={theme}>
        <TextInput label="Name" description="Shown in lists" error="Required" />
        <Textarea label="Notes" description="Markdown" />
        <NumberInput label="Limit" description="Per day" error="Too high" />
        <Select label="Role" description="Who runs it" data={['a', 'b']} />
        <TextInput label="Plain" />
      </MantineProvider>,
    )
    expect(parts('Name')).toEqual(['label', 'input', 'description', 'error'])
    expect(parts('Notes')).toEqual(['label', 'input', 'description'])
    expect(parts('Limit')).toEqual(['label', 'input', 'description', 'error'])
    expect(parts('Role')).toEqual(['label', 'input', 'description'])
    expect(parts('Plain')).toEqual(['label', 'input'])
    // The error stays tied to the input for assistive technology.
    expect(screen.getByLabelText('Name')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText('Required')).toBeInTheDocument()
  })
})
