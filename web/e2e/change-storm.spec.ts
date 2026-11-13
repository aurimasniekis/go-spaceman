import { expect, test, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, restartAgent, startAgent } from './stack'

/**
 * A burst of run changes must not become a burst of requests (incident, 2026-11-13): a node reconnecting re-reported
 * every run in its journal, each report was a run change, and each change refetched the space's runs list in every
 * open tab while the earlier request was cancelled but never aborted. A browser tab ended up holding hundreds of
 * runs?limit=200 requests it never read, and central kept every response.
 *
 * With the overview open: a burst of run changes (runs launched and stopped) refetches the runs list a few times, never
 * with two requests in flight at once; restarting the node agent (its hello re-reports every run, none changed)
 * changes no run, so the list isn't fetched again at all.
 */

interface Counter {
  __smRunChanges?: number
}

/** Counts the runs list requests the page makes and the most in flight at once. */
function watchRuns(page: Page, spaceId: string) {
  const s = { total: 0, inFlight: 0, maxInFlight: 0 }
  const isList = (url: string) => new URL(url).pathname === `/api/v1/spaces/${spaceId}/runs`
  page.on('request', (r) => {
    if (r.method() !== 'GET' || !isList(r.url())) return
    s.total++
    s.inFlight++
    s.maxInFlight = Math.max(s.maxInFlight, s.inFlight)
  })
  const done = (r: { method(): string; url(): string }) => {
    if (r.method() === 'GET' && isList(r.url())) s.inFlight--
  }
  page.on('requestfinished', done)
  page.on('requestfailed', done)
  return s
}

const runChanges = (page: Page) => page.evaluate(() => (window as unknown as Counter).__smRunChanges ?? 0)

test('a burst of run changes or a node reconnect does not flood the runs list', async ({ page }) => {
  test.setTimeout(240_000)
  // Counts the run changes the page receives from the change feed.
  await page.addInitScript(() => {
    const Native = window.EventSource
    class Counting extends Native {
      constructor(url: string | URL, init?: EventSourceInit) {
        super(url, init)
        this.addEventListener('change', (ev) => {
          try {
            if (JSON.parse((ev as MessageEvent<string>).data).resource_type === 'run') {
              const w = window as unknown as { __smRunChanges?: number }
              w.__smRunChanges = (w.__smRunChanges ?? 0) + 1
            }
          } catch {
            // not a change
          }
        })
      }
    }
    window.EventSource = Counting
  })
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'change storm journey password'
  const id = Date.now().toString(36)
  await asRoot.call('POST', '/users', { display_name: 'Storm Tester', email: `storm-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`storm-${id}@example.com`, password)
  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'storm-node' })
    startAgent(node.token)
    nodeId = node.node.id
  }
  const connected = () => expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  await connected()
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `storm-${id}`, name: `Storm ${id}`, node_id: nodeId }, { 'Idempotency-Key': `change-storm-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `storm-stub-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  await page.goto('/')
  await expect(page.getByText('Active runs').first()).toBeVisible()
  await page.waitForTimeout(1500) // the change feed connects
  const s = watchRuns(page, sp.id)

  // A burst: runs launched and stopped, a few changes each.
  const runs: string[] = []
  for (let round = 0; round < 3; round++) {
    const batch: string[] = []
    for (let i = 0; i < 3; i++) {
      const r = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: `storm${i}`, harness: 'claude-code', prompt: 'tick' })
      batch.push(r.id)
    }
    for (const r of batch) await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${r}`)).observed, { timeout: 30_000 }).toBe('running')
    for (const r of batch) await api.call('POST', `/spaces/${sp.id}/runs/${r}/stop`, { mode: 'kill' })
    runs.push(...batch)
  }
  for (const r of runs) await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${r}`)).observed, { timeout: 30_000 }).toMatch(/exited|failed/)
  await page.waitForTimeout(1500)
  const changes = await runChanges(page)
  test.info().annotations.push({ type: 'burst', description: `${changes} run changes, ${s.total} runs list requests, at most ${s.maxInFlight} in flight` })
  expect(changes, 'the burst produced run changes').toBeGreaterThan(runs.length * 2)
  expect(s.total, 'run changes are coalesced into fewer list refetches').toBeLessThan(changes)
  expect(s.maxInFlight, 'a refetch never starts while one is in flight').toBeLessThanOrEqual(1)

  // The node agent restarts: its hello re-reports every run, and none changed.
  const before = { requests: s.total, changes }
  await restartAgent()
  await connected()
  await page.waitForTimeout(3000)
  const reconnectChanges = (await runChanges(page)) - before.changes
  test.info().annotations.push({ type: 'reconnect', description: `${reconnectChanges} run changes, ${s.total - before.requests} runs list requests` })
  expect(reconnectChanges, 'a reconnect whose runs are unchanged changes no run').toBe(0)
  expect(s.total - before.requests, 'and refetches no runs list').toBe(0)
})
