import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { TopicsNote } from './TgSettings'

const names = new Map([['spc_a', 'Alpha']])
const show = (topics: { available: boolean; spaces: string[]; refused: boolean }) =>
  render(<MantineProvider><TopicsNote topics={topics} names={names} /></MantineProvider>)

describe('TopicsNote', () => {
  beforeEach(() => {
    window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  })
  it('says the bot is off when topics are not available', () => {
    show({ available: false, spaces: [], refused: false })
    expect(screen.getByText(/not available: the Telegram bot is not running/)).toBeTruthy()
  })
  it('lists the spaces that use topics, by name', () => {
    show({ available: true, spaces: ['spc_a', 'spc_x'], refused: false })
    expect(screen.getByText(/Topics are on for: Alpha, spc_x\./)).toBeTruthy()
    expect(screen.getByText(/turned on per space/)).toBeTruthy()
    expect(screen.queryByText(/refused/)).toBeNull()
  })
  it('says when no space uses them', () => {
    show({ available: true, spaces: [], refused: false })
    expect(screen.getByText('None of your spaces use topics mode.')).toBeTruthy()
  })
  it('tells what to do when Telegram refused a topic', () => {
    show({ available: true, spaces: ['spc_a'], refused: true })
    expect(screen.getByText(/refused to create a topic.*nable topics for this bot in BotFather/)).toBeTruthy()
  })
})
