import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { TopicsNote } from './TgSettings'

const names = new Map([['spc_a', 'Alpha']])
type T = Parameters<typeof TopicsNote>[0]['topics']
const base: T = { available: true, mode: 'auto', active: true, spaces: [], refused: false }
const show = (topics: Partial<T>) =>
  render(<MantineProvider><TopicsNote topics={{ ...base, ...topics }} names={names} /></MantineProvider>)

describe('TopicsNote', () => {
  beforeEach(() => {
    window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  })
  it('says topics are unavailable, with the BotFather hint', () => {
    show({ available: false, active: false })
    expect(screen.getByText(/not available now.*not enabled for it in BotFather/)).toBeTruthy()
  })
  it('says each space gets a topic and lists the ones that exist, by name', () => {
    show({ spaces: ['spc_a', 'spc_x'] })
    expect(screen.getByText(/Each space gets a topic of its own/)).toBeTruthy()
    expect(screen.getByText(/Topics so far: Alpha, spc_x\./)).toBeTruthy()
  })
  it('says everything arrives in one chat when the layout is off', () => {
    show({ mode: 'off', active: false, spaces: ['spc_a'] })
    expect(screen.getByText('Everything arrives in this one chat.')).toBeTruthy()
    expect(screen.queryByText(/Topics so far/)).toBeNull()
  })
  it('tells what to do when Telegram refused a topic', () => {
    show({ active: false, refused: true })
    expect(screen.getByText(/refused to create a topic.*nable topics for this bot in BotFather/)).toBeTruthy()
  })
})
