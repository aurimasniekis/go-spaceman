import { createFileRoute, Navigate } from '@tanstack/react-router'
import { isMiniApp } from '@/api/session'
import { TgSettings } from '@/tg/pages/TgSettings'

/** The Telegram Mini App's settings page; in the web it is the account page's Telegram section. */
export const Route = createFileRoute('/settings')({ component: () => (isMiniApp() ? <TgSettings /> : <Navigate to="/account" />) })
