import { type Browser, expect, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env } from './stack'

/** Helpers of the Markdown rendering specs (ui-markdown*.spec.ts). */

// A 4×4 PNG.
export const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAQAAAAECAIAAAAmkwkpAAAAEklEQVR4nGP4z8CAB+GTG8HSALfKY52fTcuYAAAAAElFTkSuQmCC', 'base64')

export async function open(page: Page, url: string, theme: 'light' | 'dark') {
  await page.addInitScript((t) => window.localStorage.setItem('mantine-color-scheme-value', t), theme)
  await page.goto(url)
  await expect(page.locator('html')).toHaveAttribute('data-mantine-color-scheme', theme)
}

/** Seeds a user, a space and an inbox item through the API; returns the signed-in storage state to reuse per context. */
export async function seedItem(browser: Browser, name: string, title: string, makeBody: (attachment: string) => string) {
  const base = env('SM_URL')
  const id = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`
  const password = 'markdown journey password'
  const email = `${name}-${id}@example.com`
  const seedCtx = await browser.newContext({ baseURL: base })
  const seed = await seedCtx.newPage()
  await new Api(seed.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
    .call('POST', '/users', { display_name: 'Mona Markdown', email, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(seed.request, base)
  await api.signIn(email, password)
  const nodes = await api.call<{ items: { id: string }[] }>('GET', '/nodes')
  const nodeId = nodes.items[0]?.id ?? (await api.call<{ node: { id: string } }>('POST', '/nodes', { name: `${name}-${id}` })).node.id
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `${name}-${id}`, name: 'Markdown rendering', node_id: nodeId })
  const att = await api.upload(sp.id, 'chart.png', 'image/png', PNG)
  const item = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/inbox`, { title, urgency: 'high', content_type: 'text/markdown', body: makeBody(att.id) })
  const state = await seedCtx.storageState()
  await seedCtx.close()
  return { base, state, sp, att, item }
}
