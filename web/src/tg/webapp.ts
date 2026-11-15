/** A Telegram button (BackButton, MainButton). Only what the shell uses. */
export interface TelegramButton {
  isVisible: boolean
  show(): void
  hide(): void
  onClick(cb: () => void): void
  offClick(cb: () => void): void
}

export interface TelegramMainButton extends TelegramButton {
  setParams(p: { text?: string; is_active?: boolean; is_visible?: boolean; color?: string; text_color?: string }): void
  showProgress(leaveActive?: boolean): void
  hideProgress(): void
}

/** The ThemeParams keys the shell maps (https://core.telegram.org/bots/webapps#themeparams). */
export interface TelegramThemeParams {
  bg_color?: string
  text_color?: string
  hint_color?: string
  link_color?: string
  button_color?: string
  button_text_color?: string
  secondary_bg_color?: string
  destructive_text_color?: string
}

/** The part of Telegram's WebApp object the shell uses (https://core.telegram.org/bots/webapps). */
export interface TelegramWebApp {
  initData: string
  initDataUnsafe?: { start_param?: string }
  colorScheme?: 'light' | 'dark'
  themeParams?: TelegramThemeParams
  BackButton?: TelegramButton
  MainButton?: TelegramMainButton
  ready(): void
  expand(): void
  close(): void
  onEvent?(event: 'themeChanged', cb: () => void): void
  offEvent?(event: 'themeChanged', cb: () => void): void
  openLink?(url: string): void
  downloadFile?(params: { url: string; file_name?: string }, cb?: (accepted: boolean) => void): void
  setHeaderColor?(color: string): void
  setBackgroundColor?(color: string): void
}

/** The WebApp object from telegram-web-app.js, or undefined outside Telegram (the script sets it only inside a client). */
export function webApp(): TelegramWebApp | undefined {
  return (window as unknown as { Telegram?: { WebApp?: TelegramWebApp } }).Telegram?.WebApp
}
