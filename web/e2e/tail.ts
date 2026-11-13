import { expect, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/** Test hooks a page installs before it loads (addInitScript): the terminal probe and a count of the bytes it streams. */
export interface TailProbe {
  __smTerminals?: { count(): number; texts(): string[]; screens(): string[] }
  /** Characters of event data received on terminal streams, and over the recording API. */
  __smStreamed: { terminal: number; recording: number }
  /** When (page time) the screen first matched __smWant, and when the "Loading output…" badge last went away. */
  __smShown?: number
  __smWant?: string
}

/** The init script: the terminal probe, stream byte counts, and a watcher that times when a screen first appears. */
export function instrument(want: string) {
  const w = window as unknown as TailProbe & { __smTerminalProbe: boolean }
  w.__smTerminalProbe = true
  w.__smStreamed = { terminal: 0, recording: 0 }
  w.__smWant = want
  const Native = window.EventSource
  window.EventSource = class extends Native {
    constructor(url: string | URL, init?: EventSourceInit) {
      super(url, init)
      if (!String(url).includes('/terminal')) return
      for (const kind of ['record', 'tail']) this.addEventListener(kind, (ev) => (w.__smStreamed.terminal += (ev as MessageEvent<string>).data.length))
    }
  } as typeof EventSource
  const fetch0 = window.fetch.bind(window)
  window.fetch = async (input, init) => {
    const res = await fetch0(input, init)
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    if (url.includes('/recording')) {
      void res
        .clone()
        .text()
        .then((t) => (w.__smStreamed.recording += t.length))
    }
    return res
  }
  const re = new RegExp(want, 's')
  const check = () => {
    if (w.__smShown !== undefined) return
    const s = w.__smTerminals?.screens()[0]
    if (s && re.test(s)) w.__smShown = performance.now()
    else setTimeout(check, 50)
  }
  setTimeout(check, 50)
}

/** A user, a space with a stub account and a connected node, for a spec. */
export async function tailSpace(page: Page, name: string) {
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = `${name} journey password`
  const id = Date.now().toString(36)
  await asRoot.call('POST', '/users', { display_name: 'Tail Tester', email: `${name}-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`${name}-${id}@example.com`, password)
  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: `${name}-node` })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `${name}-${id}`, name: `${name} ${id}`, node_id: nodeId }, { 'Idempotency-Key': `${name}-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `${name}-stub-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)
  return { api, spaceId: sp.id }
}

/**
 * A prompt for the stub that redraws a status box `frames` times, the way Claude Code's classic renderer does (about
 * 715 bytes a frame), with a static line every 50 frames; and the screen it ends on.
 */
export function redrawPrompt(frames: number) {
  const parts: number[] = []
  for (let left = frames; left > 0; left -= 20_000) parts.push(Math.min(left, 20_000))
  const last = parts.at(-1)!
  return { prompt: parts.map((n) => `/redraw ${n}`).join('\n'), want: `static line ${Math.floor(last / 50)} of ${Math.floor(last / 50)}.*frame +${last} of +${last}.*tick \\d+` }
}

/**
 * Waits until a run's recording ends with the stub's first tick (its whole reply is stored), reading only its tail; a
 * server without tails (a before/after comparison) is paged through instead.
 */
export async function replied(api: Api, spaceId: string, runId: string) {
  type Page = { records: { k: string; d?: string }[]; tail?: object; more: boolean; next_after: number }
  const ticked = (p: Page) => p.records.some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('latin1').includes('tick'))
  await expect
    .poll(
      async () => {
        const page = await api.call<Page>('GET', `/spaces/${spaceId}/runs/${runId}/recording?tail=65536`)
        if (page.tail) return ticked(page)
        for (let p = page; ; ) {
          if (ticked(p)) return true
          if (!p.more) return false
          p = await api.call<Page>('GET', `/spaces/${spaceId}/runs/${runId}/recording?after=${p.next_after}&limit=5000`)
        }
      },
      { timeout: 300_000, intervals: [3_000] },
    )
    .toBe(true)
}
