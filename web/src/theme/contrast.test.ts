import { describe, expect, it } from 'vitest'
import { lightOwl, nightOwl } from './nightowl'
import { ACCENTS, CODE_TOKENS, DARK_DIMMED, LIGHT_DIMMED, owlBlue, owlGreen, owlPurple, owlRed, owlYellow, theme } from './theme'

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

export function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p)
  return (x + 0.05) / (y + 0.05)
}

// WCAG 2.1 AA: 4.5:1 for normal text. Body and dimmed text are checked on every surface they appear on.
describe('theme contrast', () => {
  const cases: [string, string, string][] = [
    ['dark text on body', nightOwl.fg, nightOwl.bg],
    ['dark text on raised', nightOwl.fg, nightOwl.bgRaised],
    ['dark dimmed on body', DARK_DIMMED, nightOwl.bg],
    ['dark dimmed on raised', DARK_DIMMED, nightOwl.bgRaised],
    ['dark dimmed on a hovered or selected row', DARK_DIMMED, '#1d3b53'],
    ['light text on body', lightOwl.fg, lightOwl.bg],
    ['light text on raised', lightOwl.fg, lightOwl.bgRaised],
    ['light dimmed on body', LIGHT_DIMMED, lightOwl.bg],
    ['light dimmed on raised', LIGHT_DIMMED, lightOwl.bgRaised],
    ['light dimmed on white cards', LIGHT_DIMMED, '#ffffff'],
    // Filled controls: white on the light primary shade, Night Owl background on the dark one (--sm-on-filled).
    ['light filled button', '#ffffff', owlBlue[(theme.primaryShade as { light: number }).light]],
    ['dark filled button', nightOwl.bg, owlBlue[(theme.primaryShade as { dark: number }).dark]],
    ['light primary text on body', owlBlue[(theme.primaryShade as { light: number }).light], lightOwl.bg],
  ]
  it.each(cases)('%s', (_, fg, bg) => {
    expect(contrast(fg, bg)).toBeGreaterThanOrEqual(4.5)
  })
})

// Every accent: filled and hover shades against their text color, and the dark accent as text on raised surfaces.
describe('accent contrast', () => {
  const accents = { owlBlue, owlPurple, owlGreen, owlRed, owlYellow }
  it.each(Object.entries(accents))('%s', (_, c) => {
    expect(contrast(nightOwl.bg, c[5])).toBeGreaterThanOrEqual(4.5) // dark filled
    expect(contrast(nightOwl.bg, c[6])).toBeGreaterThanOrEqual(4.5) // dark hover
    expect(contrast('#ffffff', c[7])).toBeGreaterThanOrEqual(4.5) // light filled
    expect(contrast('#ffffff', c[8])).toBeGreaterThanOrEqual(4.5) // light hover
    expect(contrast(c[5], nightOwl.bgRaised)).toBeGreaterThanOrEqual(4.5) // dark text on cards
    expect(contrast(c[4], nightOwl.bgRaised)).toBeGreaterThanOrEqual(4.5) // dark c="<color>" text (Mantine uses shade 4)
    expect(contrast(c[7], lightOwl.bgRaised)).toBeGreaterThanOrEqual(4.5) // light text on raised surfaces
  })
})

// Light-variant badges and alerts in Night Owl: the accent text on its 12 % tint over the raised surface stays readable
// (the tint is drawn over #0b2942; checking the text against the card itself and against the accent mixed in at 12 %).
describe('dark light-variant contrast', () => {
  const mix = (fg: string, bg: string, a: number) =>
    '#' + [1, 3, 5].map((i) => Math.round(parseInt(fg.slice(i, i + 2), 16) * a + parseInt(bg.slice(i, i + 2), 16) * (1 - a)).toString(16).padStart(2, '0')).join('')
  it.each(Object.entries(ACCENTS))('%s', (_, c) => {
    expect(contrast(c[4], nightOwl.bgRaised)).toBeGreaterThanOrEqual(4.5)
    expect(contrast(c[4], mix(c[4], nightOwl.bgRaised, 0.12))).toBeGreaterThanOrEqual(4.5)
    expect(contrast(c[4], mix(c[4], nightOwl.bg, 0.12))).toBeGreaterThanOrEqual(4.5)
  })
})

// Highlighted code: every token color on its scheme's code block surface (--sm-surface).
describe('code token contrast', () => {
  it.each(Object.entries(CODE_TOKENS.light))('light %s', (_, c) => expect(contrast(c, '#ffffff')).toBeGreaterThanOrEqual(4.5))
  it.each(Object.entries(CODE_TOKENS.dark))('dark %s', (_, c) => expect(contrast(c, nightOwl.bgRaised)).toBeGreaterThanOrEqual(4.5))
})
