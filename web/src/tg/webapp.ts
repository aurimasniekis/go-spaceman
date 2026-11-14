/** The part of Telegram's WebApp object the shell uses (https://core.telegram.org/bots/webapps). */
export interface TelegramWebApp {
  initData: string
  ready(): void
  expand(): void
  close(): void
}

/** The WebApp object from telegram-web-app.js, or undefined outside Telegram (the script sets it only inside a client). */
export function webApp(): TelegramWebApp | undefined {
  return (window as unknown as { Telegram?: { WebApp?: TelegramWebApp } }).Telegram?.WebApp
}
