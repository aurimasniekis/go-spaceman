import { describe, expect, it } from 'vitest'
import { applyThemeVariables, themeVariables } from './theme'

describe('Telegram theme', () => {
  it('maps the params to the app tokens and drops anything that is not #rrggbb', () => {
    const v = themeVariables({ bg_color: '#112233', text_color: '#ffffff', hint_color: 'red', link_color: '#abcdef; background:url(x)', button_color: '#2481cc', button_text_color: '#ffffff', secondary_bg_color: '#000000' })
    expect(v).toEqual({
      '--mantine-color-body': '#112233',
      '--mantine-color-text': '#ffffff',
      '--sm-surface': '#000000',
      '--mantine-primary-color-filled': '#2481cc',
      '--mantine-primary-color-filled-hover': '#2481cc',
      '--sm-on-filled': '#ffffff',
    })
    expect(themeVariables(undefined)).toEqual({})
  })
  it('clears variables a changed theme no longer sets', () => {
    const root = document.createElement('div')
    applyThemeVariables({ '--mantine-color-body': '#111111', '--mantine-color-text': '#eeeeee' }, root)
    expect(root.style.getPropertyValue('--mantine-color-text')).toBe('#eeeeee')
    applyThemeVariables({ '--mantine-color-body': '#222222' }, root)
    expect(root.style.getPropertyValue('--mantine-color-body')).toBe('#222222')
    expect(root.style.getPropertyValue('--mantine-color-text')).toBe('')
  })
})
