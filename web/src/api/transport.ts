import { sessionStillValid } from './streams'

/**
 * How the change feed (GET /api/v1/watch) is read. The web UI keeps the browser's EventSource (cookie session,
 * automatic reconnect); the Telegram Mini App uses a fetch-based reader (tg/sseTransport.ts), because EventSource
 * cannot send an Authorization header (ADR-0083 §4.6).
 */
export interface StreamHandlers {
  /** The stream is open (the response headers arrived). */
  onOpen(): void
  /** One server-sent event. `lastEventId` is the stream's last id so far ('' if none). */
  onEvent(type: string, data: string, lastEventId: string): void
  /** The transport is reconnecting by itself (EventSource only; a fetch stream never does). */
  onReconnecting(): void
  /** The stream is closed for good: the caller decides about a retry. `status` is the HTTP status when there was one. */
  onClosed(status?: number): void
}

export interface StreamHandle {
  close(): void
}

export interface WatchTransport {
  /** False where the transport cannot run at all (no EventSource): the feed stays off. */
  supported(): boolean
  open(url: string, h: StreamHandlers): StreamHandle
  /** Re-checks the session before a retry (web: GET /auth/me; Mini App: whether the session is still held). */
  sessionStillValid(): Promise<boolean>
}

/** The web UI's transport: EventSource, unchanged behaviour. */
export const eventSourceTransport: WatchTransport = {
  supported: () => typeof EventSource !== 'undefined',
  open(url, h) {
    const src = new EventSource(url)
    src.onopen = () => h.onOpen()
    src.onerror = () => {
      if (src.readyState === EventSource.CLOSED) h.onClosed()
      else h.onReconnecting()
    }
    for (const type of ['change', 'position', 'gap', 'end']) {
      src.addEventListener(type, (ev) => h.onEvent(type, (ev as MessageEvent<string>).data, (ev as MessageEvent<string>).lastEventId))
    }
    return { close: () => src.close() }
  },
  sessionStillValid,
}

let current: WatchTransport = eventSourceTransport

/** Selects the transport useWatch uses; the Mini App entry sets it once, before it renders. */
export function setWatchTransport(t: WatchTransport) {
  current = t
}

export function watchTransport(): WatchTransport {
  return current
}
