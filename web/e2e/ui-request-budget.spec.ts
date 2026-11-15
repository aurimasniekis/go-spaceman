import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env } from './stack'

/**
 * A fresh tab must cost a few requests per endpoint, however long the change feed's history is (incident,
 * 2026-11-13): a new tab replayed every retained change from the start and refetched the affected queries once per
 * change, so opening the inbox made 760 `GET /human-actions` and 229 `GET /system/restore` and `GET /spaces` pairs.
 *
 * With a history of space and human-action changes, a fresh tab opens the inbox, then the overview, the terminals and
 * the spaces pages, and the requests per endpoint stay within a small budget.
 */

/** The most requests one endpoint may get over the whole journey. */
const BUDGET: Record<string, number> = {
  '/api/v1/human-actions': 4, // the pending badge and the inbox list, each refreshed once when the feed connects
  '/api/v1/system/restore': 2,
  '/api/v1/spaces': 4,
  '/api/v1/nodes': 4,
  '/api/v1/attention': 4,
}

test('a fresh tab makes a few requests per endpoint, whatever the change history', async ({ page, context }) => {
  test.setTimeout(180_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'request budget journey password'
  const id = Date.now().toString(36)
  await asRoot.call('POST', '/users', { display_name: 'Budget Tester', email: `budget-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`budget-${id}@example.com`, password)
  const nodes = await api.call<{ items: { id: string }[] }>('GET', '/nodes')
  const nodeId = nodes.items[0]?.id ?? (await api.call<{ node: { id: string } }>('POST', '/nodes', { name: `budget-${id}` })).node.id
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `budget-${id}`, name: `Budget ${id}`, node_id: nodeId }, { 'Idempotency-Key': `budget-${id}` })

  // History: changes the new tab must not replay one by one.
  for (let i = 0; i < 60; i++) {
    await api.call('POST', `/spaces/${sp.id}/human-actions`, { type: 'todo', title: `Budget item ${i}` }, { 'Idempotency-Key': `budget-${id}-${i}` })
    const s = await api.call<{ name: string; goals: string; standing_instructions: string; disconnect_policy: string; version: number }>('GET', `/spaces/${sp.id}`)
    await api.call('PUT', `/spaces/${sp.id}`, { name: s.name, description: `revision ${i}`, goals: s.goals, standing_instructions: s.standing_instructions,
      disconnect_policy: s.disconnect_policy }, { 'If-Match': `"${s.version}"` })
  }

  const tab = await context.newPage()
  const counts = new Map<string, number>()
  tab.on('request', (r) => {
    const path = new URL(r.url()).pathname
    if (path.startsWith('/api/')) counts.set(path, (counts.get(path) ?? 0) + 1)
  })
  await tab.goto('/human')
  await tab.waitForTimeout(10_000)
  for (const path of ['/', '/terminals', '/spaces']) {
    await tab.locator(`nav a[href="${path}"]`).first().click()
    await tab.waitForTimeout(4000)
  }
  const top = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8)
  test.info().annotations.push({ type: 'requests', description: top.map(([p, n]) => `${n}× ${p}`).join(', ') })
  for (const [path, max] of Object.entries(BUDGET)) expect(counts.get(path) ?? 0, `requests to ${path}`).toBeLessThanOrEqual(max)
  const total = [...counts.values()].reduce((a, b) => a + b, 0)
  expect(total, 'API requests over the whole journey').toBeLessThan(120)
})
