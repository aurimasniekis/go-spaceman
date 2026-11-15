import type { StreamHandlers, WatchTransport } from '@/api/transport'
import { endMiniAppSession, miniAppFetchInit } from '@/api/session'
import { isSessionEnded } from './session'

/** One parsed server-sent event. */
export interface SseEvent {
  type: string
  data: string
  id: string
}

/**
 * An incremental parser of the text/event-stream format (WHATWG HTML §9.2): lines end in LF, CR or CRLF; a blank line
 * dispatches the event; `:` lines are comments; the last id is kept across events (as EventSource's lastEventId).
 */
export class SseParser {
  private buf = ''
  private type = ''
  private data: string[] = []
  private id = ''
  private first = true
  private cr = false

  constructor(private readonly emit: (e: SseEvent) => void) {}

  push(chunk: string) {
    if (this.first) {
      this.first = false
      if (chunk.startsWith('﻿')) chunk = chunk.slice(1)
    }
    // A CR at the end of the previous chunk was a line end; a LF right after it belongs to it.
    if (this.cr && chunk.startsWith('\n')) chunk = chunk.slice(1)
    this.cr = chunk.endsWith('\r')
    this.buf += chunk
    const lines = this.buf.split(/\r\n|\n|\r/)
    this.buf = lines.pop() ?? ''
    // A trailing CR leaves '' in buf, which is right: the line ended.
    for (const l of lines) this.line(l)
  }

  private line(l: string) {
    if (l === '') {
      if (this.data.length > 0) this.emit({ type: this.type || 'message', data: this.data.join('\n'), id: this.id })
      this.type = ''
      this.data = []
      return
    }
    if (l.startsWith(':')) return
    const i = l.indexOf(':')
    const field = i < 0 ? l : l.slice(0, i)
    let value = i < 0 ? '' : l.slice(i + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') this.type = value
    else if (field === 'data') this.data.push(value)
    else if (field === 'id' && !value.includes('\0')) this.id = value
  }
}

/**
 * The Mini App's transport: fetch with `Authorization: Telegram <token>` and `credentials: 'omit'` (EventSource can do
 * neither). It never reconnects by itself and never retries a 401: that ends the session, and the shell shows the
 * "reopen the app" screen. Any other failure is reported as closed, and useWatch's own backoff decides about a retry.
 */
export const fetchSseTransport: WatchTransport = {
  supported: () => typeof fetch === 'function' && typeof ReadableStream !== 'undefined',
  // No /auth/me on this host: the session is valid while the shell still holds it.
  sessionStillValid: () => Promise.resolve(!isSessionEnded()),
  open(url: string, h: StreamHandlers) {
    const ac = new AbortController()
    let done = false
    const closed = (status?: number) => {
      if (done) return
      done = true
      h.onClosed(status)
    }
    // A response we will not read: abort the request and drop its body, so the connection is not left open.
    const discard = (res: Response) => {
      ac.abort()
      res.body?.cancel().catch(() => undefined)
    }
    void (async () => {
      try {
        const res = await fetch(url, miniAppFetchInit({ headers: { Accept: 'text/event-stream' }, cache: 'no-store', signal: ac.signal }))
        if (res.status === 401) {
          // Step-up 401s (mfa_required) also end here. Eventually they should show a "finish MFA" message rather than
          // the generic reopen screen; TG-9b-2 and the Limited-consistency task own that. Behaviour is unchanged until then.
          discard(res)
          done = true
          await endMiniAppSession(res)
          return
        }
        if (!res.ok || !res.body || !(res.headers.get('Content-Type') ?? '').startsWith('text/event-stream')) {
          discard(res)
          return closed(res.status)
        }
        h.onOpen()
        const parser = new SseParser((e) => {
          if (!done) h.onEvent(e.type, e.data, e.id)
        })
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
        for (;;) {
          const { value, done: eof } = await reader.read()
          if (eof) break
          parser.push(value)
        }
        closed()
      } catch {
        if (!ac.signal.aborted) closed()
      }
    })()
    return {
      close: () => {
        done = true
        ac.abort()
      },
    }
  },
}
