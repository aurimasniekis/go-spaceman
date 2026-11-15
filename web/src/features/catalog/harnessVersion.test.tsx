import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { HarnessVersionInput } from './HarnessVersionInput'
import { harnessVersionLabel, harnessVersionValue } from './harnessVersion'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

describe('harness version pin', () => {
  it('accepts empty, the channels and a strict X.Y.Z, trimmed', () => {
    for (const v of ['', 'latest', 'stable', '0.0.0', '2.1.322']) expect(harnessVersionValue(v)).toEqual({ value: v })
    expect(harnessVersionValue(' 2.1.317 ')).toEqual({ value: '2.1.317' })
  })

  it('refuses what the server refuses', () => {
    for (const v of ['Latest', 'rc', 'v2.1.3', '2.1', '2.1.3.4', '02.1.3', '2.1.3-rc.1', '1000000.0.0']) {
      expect(harnessVersionValue(v)).toHaveProperty('error')
    }
  })

  it('labels a pinned profile and nothing otherwise', () => {
    expect(harnessVersionLabel('2.1.317')).toBe('Claude Code 2.1.317')
    expect(harnessVersionLabel('')).toBe('')
    expect(harnessVersionLabel(undefined)).toBe('')
  })

  it('shows the hint and the error in the profile form', () => {
    render(<MantineProvider><HarnessVersionInput value="v2" onChange={() => {}} error="Use latest, stable or an exact version" /></MantineProvider>)
    expect(screen.getByRole('textbox', { name: 'Claude Code version' })).toHaveValue('v2')
    expect(screen.getByText(/Empty follows the node’s channel/)).toBeInTheDocument()
    expect(screen.getByText('Use latest, stable or an exact version')).toBeInTheDocument()
  })
})
