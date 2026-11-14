import { readFileSync } from 'node:fs'

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page, type Route } from '@playwright/test'

import { Api, env, settle } from './stack'

/**
 * Usage limits in the UI (ADR-0079, UL-T10): the Limits page's held accounts and waiting runs with until when, a
 * resume-now and a confirmed release, and the run badge on the space overview. Times show in the browser's zone
 * (Asia/Tokyo here, UTC+9 with no daylight saving) with UTC in the tooltip. Checked at 390 and 1280 px, without sideways
 * scrolling, and with axe (WCAG 2.1 A/AA).
 *
 * The holds, waits and the limited run are served by route stubs on top of the real stack (sign-in, space, node, every
 * other request): reaching a real usage limit needs a harness that hits one, which this stack's stub harness can't.
 * The API side of these endpoints is covered by the Go app tests (internal/app usage-limit and limit-wait e2e).
 */

test.use({ timezoneId: 'Asia/Tokyo' })

const WIDTHS = [390, 1280] as const

async function axe(page: Page, what: string) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  const found = res.violations.map((v) => `${v.id} [${v.impact}]: ${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')}`)
  expect(found, `${what}: accessibility violations`).toEqual([])
}

async function noSideways(page: Page, what: string) {
  const w = await page.evaluate(() => ({ page: document.documentElement.scrollWidth, view: document.documentElement.clientWidth }))
  expect(w.page, `${what}: page scrolls sideways`).toBeLessThanOrEqual(w.view)
}

const json = (route: Route, body: unknown) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
const hhmm = (d: Date, offsetHours: number) => {
  const t = new Date(d.getTime() + offsetHours * 3600_000)
  return `${String(t.getUTCHours()).padStart(2, '0')}:${String(t.getUTCMinutes()).padStart(2, '0')}`
}

test('usage limits: held accounts, waiting runs, resume now, release and the run badge', async ({ page }) => {
  test.setTimeout(120_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'usage limits journey password'
  await asRoot.call('POST', '/users', { display_name: 'Uma Admin', email: 'usage-admin@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('usage-admin@example.com', password)
  const node = await api.call<{ node: { id: string } }>('POST', '/nodes', { name: 'usage-node' }, { 'Idempotency-Key': 'usage-limits-node' })
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'usage-limits', name: 'Usage', node_id: node.node.id }, { 'Idempotency-Key': 'usage-limits-space' })

  // A weekly hold three hours out, minutes rounded so the shown time is exact.
  const reset = new Date(Math.ceil((Date.now() + 3 * 3600_000) / 60_000) * 60_000)
  const resetISO = reset.toISOString().replace('.000Z', 'Z')
  const utc = `${resetISO.slice(0, 16).replace('T', ' ')} UTC`
  const now = new Date().toISOString()
  const hold = { id: 'ulm_e2e', space_id: sp.id, scope: 'account', account_id: 'acc_e2e', kind: 'seven_day', source: 'transcript', resets_at: resetISO, seen_at: now, version: 1 }
  const shared = { scope: 'account', account_id: 'acc_other', kind: 'five_hour', source: 'transcript', resets_at: resetISO, seen_at: now, shared: true }
  let wait = { id: 'lwt_e2e', space_id: sp.id, run_id: 'run_e2e', run_kind: 'worker', role: 'implementor', node_id: node.node.id, instance_id: 'ins_e2e',
    harness: 'claude-code', resume_at: resetISO, release_at: new Date(Date.now() + 7 * 86400_000).toISOString(), state: 'waiting', attempts: 1,
    last_error: 'node_offline', task_id: 'tsk_e2e', usage_limit_id: 'ulm_e2e', version: 1, created_at: now, updated_at: now }
  const posts: string[] = []
  await page.route(`**/api/v1/spaces/${sp.id}/usage-limits`, (r) => json(r, { items: [hold, shared] }))
  await page.route(`**/api/v1/spaces/${sp.id}/limit-waits`, (r) => json(r, { items: [wait] }))
  await page.route(`**/api/v1/spaces/${sp.id}/limit-waits/lwt_e2e/*`, (r) => {
    const action = r.request().url().split('/').pop()!
    posts.push(action)
    // A refused resume (the node is offline) and an accepted release, as the API answers them.
    if (action === 'resume') return r.fulfill({ status: 409, contentType: 'application/problem+json', body: JSON.stringify({ title: 'Conflict', status: 409, detail: "the space's node is not connected" }) })
    wait = { ...wait, state: 'released', last_error: 'operator', updated_at: new Date().toISOString() }
    return json(r, wait)
  })

  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
    await page.goto(`/spaces/${sp.id}/limits`)
    const card = page.getByTestId('usage-limits')
    const h = card.getByTestId('usage-hold').first()
    await expect(h).toContainText('weekly limit')
    // Tokyo is UTC+9: the hold's time is shown there, with UTC in the tooltip on focus.
    const t = h.locator(`time[datetime="${resetISO}"]`)
    await expect(t).toHaveText(new RegExp(`^${hhmm(reset, 9)} (JST|GMT\\+9)$`))
    await t.focus()
    await expect(page.getByRole('tooltip')).toHaveText(utc)
    await expect(card.getByTestId('usage-hold').nth(1)).toContainText('shared · another space')
    const w = card.getByTestId('limit-wait')
    await expect(w).toContainText('waiting because the node is not connected')
    await expect(w).toContainText('Its task goes back to the queue at')
    await settle(page)
    await noSideways(page, `limits ${width}px`)
    await axe(page, `limits ${width}px`)
  }

  // Resume now: the refusal is shown, the wait keeps waiting.
  const w = page.getByTestId('limit-wait')
  await w.getByRole('button', { name: 'Resume now' }).click()
  await expect(page.getByRole('alert').filter({ hasText: "the space's node is not connected" })).toBeVisible()
  // Release asks first, then gives the task back.
  await w.getByRole('button', { name: 'Release' }).click()
  const dialog = page.getByRole('dialog', { name: 'Release this run?' })
  await expect(dialog).toContainText('Its task goes back to the queue, ready and unassigned')
  await axe(page, 'release dialog')
  await dialog.getByRole('button', { name: 'Release' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByTestId('limit-waits-ended')).toContainText('implementor · released at')
  await expect(page.getByTestId('limit-waits-empty')).toBeVisible()
  expect(posts).toEqual(['resume', 'release'])

  // The run badge: a live run at its limit on the space overview.
  const run = { id: 'run_live', space_id: sp.id, role: 'implementor', kind: 'worker', harness: 'claude-code', observed: 'running', desired: 'running', status: 'idle',
    turn: 2, created_at: now, started_at: now, created_by: 'user:usr_e2e', instance_id: 'ins_e2e', generation: 1, node_id: node.node.id, version: 1, launch: {},
    limit: { kind: 'five_hour', since: now, resets_at: resetISO } }
  await page.route(new RegExp(`/api/v1/spaces/${sp.id}/runs(\\?.*)?$`), (r) => json(r, { items: [run] }))
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
    await page.goto(`/spaces/${sp.id}`)
    const badge = page.getByTestId('run-limit-badge').first()
    await expect(badge).toContainText('5-hour limit · resets')
    await expect(badge.locator('time')).toHaveText(new RegExp(`^${hhmm(reset, 9)} `))
    await expect(page.getByText('at usage limit').first()).toBeVisible()
    await settle(page)
    await noSideways(page, `overview ${width}px`)
    await axe(page, `overview ${width}px`)
  }
})
