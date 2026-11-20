import { readFileSync } from 'node:fs'

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page, type Route } from '@playwright/test'

import { Api, env, settle } from './stack'

/**
 * Account switching in the UI (ADR-0090, AS-T8): a space's Accounts page (pool order and role pools saved through the
  * real API, the failover and headroom settings, windows, the switches list) and its Usage page. Checked at 390 and
 * 1280 px, in light and dark, without sideways scrolling, and with axe (WCAG 2.1 A/AA).
 *
 * Accounts, bindings, pools and limits are real. The account windows, the switches and the usage series are served by
 * route stubs: they come from runs that report usage and reach limits, which this stack's stub harness doesn't do (the
 * API side is covered by the Go app tests).
 *
 * It creates its own administrator, so it must sort after smoke.spec.ts, which needs a stack that is not set up yet.
 */

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

test('accounts and usage: pools, settings, windows, switches and the usage page', async ({ page }) => {
  test.setTimeout(180_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'account switching journey password'
  await asRoot.call('POST', '/users', { display_name: 'Ada Admin', email: 'switching-admin@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('switching-admin@example.com', password)
  const node = await api.call<{ node: { id: string } }>('POST', '/nodes', { name: 'switching-node' }, { 'Idempotency-Key': 'switching-node' })
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'switching', name: 'Switching', node_id: node.node.id }, { 'Idempotency-Key': 'switching-space' })
  const main = await api.call<{ id: string }>('POST', '/accounts', { name: 'team-max', kind: 'claude_oauth_token', secret: 'stub-token-main' })
  const spare = await api.call<{ id: string }>('POST', '/accounts', { name: 'spare-pro', kind: 'claude_oauth_token', secret: 'stub-token-spare' })
  await api.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: main.id, default: true, priority: 10 }, { account_id: spare.id, priority: 20 }] })

  // A fresh space has no roles; the pool editor lists them, so the list is stubbed (the pool endpoints are real).
  await page.route(`**/api/v1/spaces/${sp.id}/roles`, (r) => json(r, { items: [{ name: 'control' }, { name: 'implementor' }] }))

  const now = Date.now()
  const iso = (ms: number) => new Date(ms).toISOString()
  const day = 86_400_000
  const today = Math.floor(now / day) * day
  await page.route(`**/api/v1/spaces/${sp.id}/account-windows`, (r) => json(r, { items: [
    { account_id: main.id, name: 'team-max', harness: 'claude-code', windows: [
      { kind: 'five_hour', used_pct: 63, resets_at: iso(now + 2 * 3600_000), observed_at: iso(now - 4 * 60_000) },
      { kind: 'seven_day', used_pct: 88, resets_at: iso(now + 3 * day), observed_at: iso(now - 4 * 60_000) }] },
    { account_id: spare.id, name: 'spare-pro', harness: 'claude-code', windows: [{ kind: 'five_hour', used_pct: 12, resets_at: iso(now - 3600_000), observed_at: iso(now - 5 * 3600_000) }] },
  ] }))
  await page.route(`**/api/v1/spaces/${sp.id}/account-switches`, (r) => json(r, { items: [
    { id: 'asw_e2e', space_id: sp.id, instance_id: 'ins_e2e', role: 'implementor', run_kind: 'worker', harness: 'claude-code', from_run_id: 'run_e2e_a', new_run_id: 'run_e2e_b',
      from_account_id: main.id, to_account_id: spare.id, cause: 'usage_limit', limit_kind: 'five_hour', limit_resets_at: iso(now + 2 * 3600_000), state: 'succeeded', version: 3,
      created_at: iso(now - 3600_000), updated_at: iso(now - 3500_000) },
    { id: 'asw_e2e2', space_id: sp.id, instance_id: 'ins_e2e', role: 'reviewer', run_kind: 'worker', harness: 'claude-code', from_run_id: 'run_e2e_c', from_account_id: spare.id, to_account_id: '',
      cause: 'usage_limit', limit_kind: 'seven_day', state: 'fell_back', reason: 'no_eligible_account', version: 1, created_at: iso(now - 2 * 3600_000), updated_at: iso(now - 2 * 3600_000) },
  ] }))
  const row = (d: number, group: string, tokens: number, usd: number) => ({ bucket_start: iso(today - d * day).replace('.000Z', 'Z'), group, total_tokens: tokens, input_tokens: Math.round(tokens * 0.2),
    output_tokens: Math.round(tokens * 0.05), cache_read_tokens: Math.round(tokens * 0.7), cache_write_tokens: Math.round(tokens * 0.05), reasoning_tokens: 0, requests: Math.round(tokens / 4000), total_cost_usd: usd })
  const items = [6, 5, 4, 3, 2, 1, 0].flatMap((d) => [row(d, 'implementor', 400_000 + d * 90_000, 1.2 + d * 0.2), row(d, 'reviewer', 120_000 + (d % 3) * 60_000, 0.4), row(d, 'control', 40_000, 0.1)])
  const sum = (k: 'total_tokens' | 'input_tokens' | 'output_tokens' | 'cache_read_tokens' | 'cache_write_tokens' | 'requests' | 'total_cost_usd') => items.reduce((n, r) => n + r[k], 0)
  await page.route(new RegExp(`/api/v1/spaces/${sp.id}/usage/series`), (r) => json(r, { bucket: 'day', group_by: 'role', from: iso(today - 6 * day), to: iso(today + day), items,
    cost_note: "The cost is the harness's own list-price estimate, not a bill; tokens are the primary unit.",
    totals: { group: '', total_tokens: sum('total_tokens'), input_tokens: sum('input_tokens'), output_tokens: sum('output_tokens'), cache_read_tokens: sum('cache_read_tokens'),
      cache_write_tokens: sum('cache_write_tokens'), reasoning_tokens: 0, requests: sum('requests'), total_cost_usd: sum('total_cost_usd') } }))

  for (const scheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: scheme })
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
      await page.goto(`/spaces/${sp.id}/accounts`)
      const what = `accounts ${width}px ${scheme}`
      await expect(page.getByTestId('windows-team-max')).toContainText('63 % used')
      await expect(page.getByTestId('windows-spare-pro')).toContainText('may be out of date')
      const sw = page.getByTestId('account-switches')
      await expect(sw).toContainText('team-max → spare-pro')
      await expect(sw).toContainText('because no other account of the pool was free')
      await expect(page.getByTestId('failover-settings')).toContainText('Launch headroom')
      await settle(page)
      await noSideways(page, what)
      await axe(page, what)
    }
  }

  // Pool order: move the default account later and save; the real API keeps the order.
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto(`/spaces/${sp.id}/accounts`)
  await page.getByRole('button', { name: 'Move team-max later' }).click()
  await page.getByRole('button', { name: 'Save order' }).click()
  await expect(page.getByText('Pool order saved')).toBeVisible()
  const bound = await api.call<{ items: { account_id: string; priority: number }[] }>('GET', `/spaces/${sp.id}/accounts`)
  expect(bound.items.find((b) => b.account_id === main.id)?.priority).toBeGreaterThan(bound.items.find((b) => b.account_id === spare.id)?.priority ?? 0)

  // A role pool: add an account to control's pool, save, then remove it again.
  const pool = page.getByTestId('role-pool-control')
  await pool.getByRole('textbox', { name: 'Add an account to the pool of control' }).click()
  await page.getByRole('option', { name: /spare-pro/ }).click()
  await pool.getByRole('button', { name: 'Save pool' }).click()
  await expect(page.getByText('Pool of control saved')).toBeVisible()
  const pools = await api.call<{ items: { role: string; items: { account_id: string }[] }[] }>('GET', `/spaces/${sp.id}/account-pools`)
  expect(pools.items.find((p) => p.role === 'control')?.items.map((i) => i.account_id)).toEqual([spare.id])
  await pool.getByRole('button', { name: 'Remove spare-pro from the pool of control' }).click()
  await pool.getByRole('button', { name: 'Remove pool' }).click()
  await expect(page.getByText("control uses the space's accounts again")).toBeVisible()

  // The settings go through the space's limit overrides.
  const settings = page.getByTestId('failover-settings')
  await settings.getByLabel(/Wait instead of switching/).fill('30')
  await settings.getByLabel(/Launch headroom/).fill('20')
  await settings.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('Account switching settings saved')).toBeVisible()
  const lim = await api.call<{ limits: { key: string; override: number | null }[] }>('GET', `/spaces/${sp.id}/limits`)
  expect(lim.limits.find((l) => l.key === 'usage_limit_wait_if_reset_within')?.override).toBe(30)
  expect(lim.limits.find((l) => l.key === 'usage_limit_min_headroom_percent')?.override).toBe(20)

  // Usage: tokens first, the cost labelled as an estimate, a table of the same numbers.
  for (const scheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: scheme })
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
      await page.goto(`/spaces/${sp.id}/usage`)
      const what = `usage ${width}px ${scheme}`
      await expect(page.getByTestId('usage-totals')).toContainText('Estimated cost')
      await expect(page.getByTestId('usage-totals')).toContainText('list price, not a bill')
      await expect(page.getByRole('table', { name: 'Usage by role' })).toContainText('implementor')
      await page.getByRole('img', { name: /tokens\. / }).first().focus()
      await expect(page.getByTestId('usage-tooltip')).toBeVisible()
      await settle(page)
      await noSideways(page, what)
      await axe(page, what)
    }
  }
})
