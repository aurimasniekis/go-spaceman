import { useEffect } from 'react'
import { useMantineColorScheme } from '@mantine/core'
import type { TelegramThemeParams } from './webapp'
import { webApp } from './webapp'

/**
 * Telegram's theme mapped onto the app's CSS variables (plan §4.6). Telegram's colour scheme picks Night or Light Owl;
 * its bg, text, hint, link and button colours then override the matching tokens. Only plain #rrggbb values are taken:
 * the params come from the client, but they end up in a style attribute, so nothing else passes.
 */
const HEX = /^#[0-9a-f]{6}$/i

const MAP: Array<[keyof TelegramThemeParams, string[]]> = [
  ['bg_color', ['--mantine-color-body']],
  ['text_color', ['--mantine-color-text']],
  ['hint_color', ['--mantine-color-dimmed']],
  ['link_color', ['--mantine-color-anchor']],
  ['secondary_bg_color', ['--sm-surface']],
  ['button_color', ['--mantine-primary-color-filled', '--mantine-primary-color-filled-hover']],
  ['button_text_color', ['--sm-on-filled']],
]

/** The CSS variables a ThemeParams object sets (only valid colours). */
export function themeVariables(p: TelegramThemeParams | undefined): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, vars] of MAP) {
    const v = p?.[key]
    if (typeof v === 'string' && HEX.test(v)) for (const name of vars) out[name] = v
  }
  return out
}

const applied = new Set<string>()

/** Applies the variables to the document root, clearing ones a changed theme no longer sets. */
export function applyThemeVariables(vars: Record<string, string>, root: HTMLElement = document.documentElement) {
  for (const name of applied) if (!(name in vars)) root.style.removeProperty(name)
  applied.clear()
  for (const [name, value] of Object.entries(vars)) {
    root.style.setProperty(name, value)
    applied.add(name)
  }
}

/** Follows Telegram's theme: on mount and on every themeChanged. */
export function useTelegramTheme() {
  const { setColorScheme } = useMantineColorScheme()
  useEffect(() => {
    const app = webApp()
    if (!app) return
    const apply = () => {
      if (app.colorScheme === 'light' || app.colorScheme === 'dark') setColorScheme(app.colorScheme)
      const vars = themeVariables(app.themeParams)
      applyThemeVariables(vars)
      const bg = vars['--mantine-color-body']
      if (bg) {
        app.setHeaderColor?.(bg)
        app.setBackgroundColor?.(bg)
      }
    }
    apply()
    app.onEvent?.('themeChanged', apply)
    return () => app.offEvent?.('themeChanged', apply)
  }, [setColorScheme])
}
