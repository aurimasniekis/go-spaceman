import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, type LaunchOptions, type Page } from '@playwright/test'

import { Api, env, startAgent } from './stack'

/**
 * Scroll-overflow checks for the UI (unwanted scrollbars). Chromium in Playwright hides scrollbars by default
 * (--hide-scrollbars), which also hides this class of bug: a dialog or a strip that is a pixel too small shows a
 * scrollbar only when scrollbars take space, as they do on Windows, Linux and macOS with "always show scrollbars". Specs
 * that use these checks launch Chromium with real scrollbars (see `realScrollbars`).
 */

/**
 * Launch options for Chromium with classic (space-taking) scrollbars, keeping SM_CHROMIUM_ARGS. Chromium only: the
 * opt-in Firefox and WebKit projects (SM_E2E_BROWSERS) launch with their defaults (WebKit refuses Chromium's flags).
 */
export const realScrollbars = async ({ browserName }: { browserName: string }, use: (o: LaunchOptions) => Promise<void>) =>
  use(browserName === 'chromium' ? { ignoreDefaultArgs: ['--hide-scrollbars'], args: (process.env.SM_CHROMIUM_ARGS ?? '').split(/\s+/).filter(Boolean) } : {})

export interface Problem {
  /** A short CSS-like description of the element. */
  el: string
  what: string
  scroll: string
}

/**
 * Finds unwanted scroll overflow on the page as shown:
 *  - the page itself scrolls sideways;
 *  - a dialog's (modal's or drawer's) content is wider than the dialog, whether it scrolls sideways or is cut off;
 *  - a modal scrolls vertically although it is shorter than its maximum height (the content would fit);
 *  - any scroll container overflows by a sliver (1-4 px): a layout artefact (negative margins, borders, rounding), never
 *    content someone needs to scroll to, which still shows a full scrollbar;
 *  - a truncated text squeezed under 40 px by its neighbours (cut off);
 *  - with `nested`, a tall inner panel that scrolls while the page scrolls too (double scrollbars).
 * Elements that are not rendered (display: none, zero size) are ignored.
 */
export async function overflowProblems(page: Page, opts: { nested?: boolean } = {}): Promise<Problem[]> {
  return page.evaluate((nested) => {
    const out: { el: string; what: string; scroll: string }[] = []
    const scrolls = (v: string) => v === 'auto' || v === 'scroll'
    const describe = (e: Element) => {
      const label = e.getAttribute('aria-label') || e.getAttribute('data-testid') || ''
      const cls = String((e as HTMLElement).className || '').split(/\s+/).filter((c) => c.startsWith('mantine-') || c.startsWith('sm-')).slice(0, 2).join('.')
      return `${e.tagName.toLowerCase()}${cls ? '.' + cls : ''}${label ? `[${label}]` : ''}`
    }
    const doc = document.documentElement
    if (doc.scrollWidth > doc.clientWidth) out.push({ el: 'page', what: 'page scrolls sideways', scroll: `${doc.scrollWidth}/${doc.clientWidth}` })
    // Cut-off content: a truncated text squeezed to (almost) nothing by its neighbours.
    for (const e of Array.from(document.querySelectorAll<HTMLElement>('[data-truncate]'))) {
      const rect = e.getBoundingClientRect()
      if (rect.height > 0 && (e.textContent ?? '').trim().length > 3 && rect.width < 40)
        out.push({ el: describe(e), what: 'text truncated to nothing', scroll: `w ${Math.round(rect.width)} "${(e.textContent ?? '').trim().slice(0, 30)}"` })
    }
    for (const e of Array.from(document.querySelectorAll<HTMLElement>('body *'))) {
      if (e.clientWidth === 0 && e.clientHeight === 0) continue
      const scroll = `w ${e.scrollWidth}/${e.clientWidth} h ${e.scrollHeight}/${e.clientHeight}`
      if (e.matches('.mantine-Modal-content, .mantine-Drawer-content')) {
        // Whatever the overflow style: content wider than the dialog is either cut off or scrolls sideways.
        if (e.scrollWidth > e.clientWidth) out.push({ el: describe(e), what: 'dialog content is wider than the dialog', scroll })
        if (e.scrollHeight > e.clientHeight && e.matches('.mantine-Modal-content:not([data-full-screen])')) {
          const max = parseFloat(getComputedStyle(e).maxHeight)
          const limit = Number.isFinite(max) ? max : window.innerHeight
          if (e.offsetHeight < limit - 1) out.push({ el: describe(e), what: 'modal scrolls vertically although its content fits', scroll })
        }
        continue
      }
      const cs = getComputedStyle(e)
      const dx = scrolls(cs.overflowX) ? e.scrollWidth - e.clientWidth : 0
      const dy = scrolls(cs.overflowY) ? e.scrollHeight - e.clientHeight : 0
      if (dx <= 0 && dy <= 0) continue
      // A terminal box scrolls by whatever its fixed rows exceed it (terminal-scroll.spec.ts), a few pixels included, and
      // so does a column marked as a scroll region (the terminals workspace) by whatever the screen height leaves.
      // A table that scrolls sideways is as wide as its data, by however little: only its vertical slivers count.
      const table = e.querySelector('table') !== null
      const sliver = (dx > 0 && dx <= 4 && !table) || (dy > 0 && dy <= 4)
      if (sliver && !e.matches('.sm-terminal, [data-scroll-region]')) out.push({ el: describe(e), what: 'overflows by a sliver', scroll })
      // Double scrollbars: the page scrolls and so does a tall panel inside it (reported by the audit, not asserted). The
      // navigation sidebar is fixed and scrolls on its own.
      if (nested && dy > 4 && doc.scrollHeight > doc.clientHeight && e.clientHeight > window.innerHeight / 2 && !e.closest('[role="dialog"], .mantine-AppShell-navbar'))
        out.push({ el: describe(e), what: 'page and a tall inner panel both scroll vertically', scroll })
    }
    return out
  }, opts.nested ?? false)
}

export interface World {
  space: string
  /** A second space, peer of the first; it sends the first a cross-space request. */
  peer: string
  item: string
  /** A stopped run (its recording). */
  run: string
  /** A running run; `stop` ends it. */
  live: string
  prompt: string
  template: string
  subscription: string
}

const PASSWORD = 'overflow journey password'

/** Stops the seed's running run. */
export async function stopWorld(page: Page, w: World) {
  const root = new Api(page.request, env('SM_URL'), readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  await root.call('POST', `/spaces/${w.space}/runs/${w.live}/stop`, { mode: 'kill', reason: 'overflow journey done' }).catch(() => undefined)
}

/**
 * Signs in a new administrator and seeds one of everything a page or a drawer lists (through the API), so that each
 * dialog has something to open: a running and a stopped run, inbox items, tasks, human actions, messages, schedules, a
 * repository, a knowledge base, a cross-space request, an event subscription, an MCP server, a Git key and catalog
 * entries. Names are long enough to wrap the way real ones do.
 */
export async function seedWorld(page: Page, tag = ''): Promise<World> {
  // `tag` keeps names unique when the seed runs more than once on one stack.
  const t = tag ? `-${tag}` : ''
  const base = env('SM_URL')
  const root = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const email = `overflow${t}@example.com`
  await root.call('POST', '/users', { display_name: 'Oli Overflow', email, password: PASSWORD, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(email, PASSWORD)
  await api.call('PUT', '/auth/me/notification-preferences', { enabled: false, desktop: false, new_actions: true, assigned_to_me: true, escalations: true,
    reminders: false, clarifications: true, min_priority: 'low', muted_spaces: [] }, { 'If-Match': '"0"' }).catch(() => undefined)

  // The stack's node when an earlier journey enrolled it, otherwise a new one.
  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'overflow-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  type Space = { id: string; version: number; workdir: string }
  const sp = await api.call<Space>('POST', '/spaces', { slug: `overflow-payments${t}`, name: 'Payments platform and settlement', node_id: nodeId,
    description: 'Card processing services and their release train', goals: 'Keep the settlement pipeline green.' })
  const peer = await api.call<Space>('POST', '/spaces', { slug: `overflow-docs${t}`, name: 'Docs site', node_id: nodeId })
  // An archived space (Purge) and a node that is not enrolled (its enrollment token can be reissued without harm).
  const old = await api.call<Space>('POST', '/spaces', { slug: `overflow-old${t}`, name: 'Old reports', node_id: nodeId })
  await api.call('POST', `/spaces/${old.id}/state`, { state: 'archived' }, { 'If-Match': `"${old.version}"` })
  await api.call('POST', '/nodes', { name: `overflow-spare${t}`, description: 'Not enrolled yet' })
  const acct = await api.call<{ id: string }>('POST', '/accounts', { name: `overflow-stub${t}`, kind: 'claude_oauth_token', secret: 'stub-token', description: 'Stub account' })
  for (const s of [sp, peer]) await api.call('PUT', `/spaces/${s.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  for (const s of [sp, peer]) await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${s.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  // Catalog: a prompt with two versions, a profile, a role template and the space's roles.
  type P = { id: string; versions: { id: string; state: string; version: number }[] }
  const prompt = await api.call<P>('POST', '/prompts', { slug: `overflow-control${t}`, name: 'Control', kind: 'role', body: '## Your role\n\nCoordinate this space.' })
  const publish = async () => {
    const d = (await api.call<P>('GET', `/prompts/${prompt.id}`)).versions.find((v) => v.state === 'draft')!
    await api.call('POST', `/prompts/${prompt.id}/publish`, undefined, { 'If-Match': `"${d.version}"` })
  }
  await publish()
  await api.call('PUT', `/prompts/${prompt.id}/draft`, { body: '## Your role\n\nCoordinate this space. Ask before production changes.', notes: 'Ask first' })
  await publish()
  const profile = await api.call<{ id: string }>('PUT', '/harness-profiles/new', { slug: `overflow-claude${t}`, name: 'Claude Code (default model)', harness: 'claude-code', model: '',
    settings: { permission_mode: 'acceptEdits' } })
  const tpl = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: `overflow-control${t}`, name: 'Control', kind: 'control', profile_id: profile.id, prompt_id: prompt.id,
    memory_conventions: 'Keep decisions in memory/decisions.md.', settings: {} })
  await api.call('PUT', `/spaces/${sp.id}/roles/control`, { template_id: tpl.id, instructions: 'Settlement services live in services/settlement.', settings: {} })
  const tKB = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: `overflow-kb${t}`, name: 'KB support', kind: 'kb', profile_id: profile.id, settings: {} })
  await api.call('PUT', `/spaces/${sp.id}/roles/kb-support`, { template_id: tKB.id, settings: {} })
  const prompts = await api.call<{ items: { id: string; slug: string }[] }>('GET', '/prompts')
  const dreamPrompt = prompts.items.find((x) => x.slug === 'dream-consolidation')
  const tDream = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: `overflow-dreamer${t}`, name: 'Dreamer', kind: 'dreamer', profile_id: profile.id,
    prompt_id: dreamPrompt?.id, settings: {} })
  await api.call('PUT', `/spaces/${sp.id}/roles/dreamer`, { template_id: tDream.id, settings: {} })
  await api.call('POST', `/spaces/${sp.id}/dreams`, { slug: 'nightly', name: 'Nightly consolidation', space_role: 'dreamer', memory_roles: ['implementor', 'control'],
    interval_hours: 24, max_minutes: 20, max_files: 20, instructions: 'Focus on settlement decisions.' })

  // Runs, one at a time (the node runs at most 4 workers, and earlier journeys may have left some running): a worker that
  // writes memory and is stopped (a recording, a memory checkpoint), then one left running.
  type Run = { id: string }
  const observed = async (id: string) => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${id}`)).observed
  const done = await api.call<Run>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'implementor', harness: 'claude-code',
    prompt: 'Add the retry budget.\n/write decisions.md # Decisions\\n\\n- settlement retries use a budget of 5 per batch.\n' })
  await expect.poll(() => observed(done.id), { timeout: 30_000 }).toBe('running')
  await page.waitForTimeout(3_000)
  await api.call('POST', `/spaces/${sp.id}/runs/${done.id}/stop`, { mode: 'kill', reason: 'finished the change' })
  await expect.poll(() => observed(done.id), { timeout: 30_000 }).toMatch(/exited|failed/)
  const run = await api.call<Run>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'reviewer', harness: 'claude-code', prompt: 'Review open pull requests.' })
  await expect.poll(() => observed(run.id), { timeout: 30_000 }).toBe('running')

  // Inbox, queue, human actions and messages.
  const item = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/inbox`, { title: 'Settlement retries exhaust the budget during the bank window',
    urgency: 'high', content_type: 'text/markdown', body: 'Retries **exhaust** the budget.\n\n```\nretry budget exhausted for batch 2026-11-02T01:00:00Z (attempt 5 of 5)\n```\n\n' +
      // Rich Markdown that is wider than a phone: it scrolls inside its own block, never the page.
      `| ${Array.from({ length: 10 }, (_, i) => `settlement_column_${i}`).join(' | ')} |\n|${' --- |'.repeat(10)}\n|${' value |'.repeat(10)}\n\n` +
      '```go\nfunc retryBudget(batch *Batch) int { return batch.Size * 5 } // a line longer than a phone screen is wide, by a lot\n```\n\n' +
      '```mermaid\nflowchart LR\n  A[Bank window opens] --> B{Retry budget left?} --> C[Retry the batch] --> D[Alert the operator]\n```' })
  await api.call('POST', `/spaces/${sp.id}/inbox`, { title: 'Production alert', urgency: 'urgent', content_type: 'application/json', body: '{"name":"settlement lag","severity":"critical"}' })
  type T = { id: string; version: number }
  const task = await api.call<T>('POST', `/spaces/${sp.id}/tasks`, { status: 'ready', title: 'Add a retry budget per settlement batch instead of per item',
    objective: 'Retries are counted per item today; count them per batch.', priority: 'high',
    acceptance: '- [ ] Retries span the bank maintenance window\n- [ ] Alert when the budget is exhausted' })
  await api.call('POST', `/spaces/${sp.id}/tasks`, { status: 'ready', title: 'Export refunds as CSV' })
  await api.call('POST', `/spaces/${sp.id}/tasks/${task.id}/blockers`, { kind: 'external', note: 'waiting for the finance team to confirm the columns' })
  await api.call('POST', `/spaces/${sp.id}/human-actions`, { type: 'question', title: 'Should retries span the whole bank maintenance window?', task_id: task.id,
    priority: 'high', request: { options: ['Yes', 'No, stop at the window'] } })
  await api.call('POST', `/spaces/${sp.id}/human-actions`, { type: 'todo', title: "Confirm the bank's maintenance window", request: { steps: ['Email the bank', 'Record the window'] } })
  await api.call('POST', `/spaces/${sp.id}/messages`, { to: 'control', subject: 'Hold deploys until the bank window closes', request: true, body: 'The window ends at **06:00 UTC**.' })

  // A custom human action type.
  const dw = await api.call<{ id: string; versions: { version: number }[] }>('POST', '/human-types', { slug: `deploy-window${t}`, name: 'Deploy window',
    description: 'Agree on when a production deployment may happen.',
    request_schema: { type: 'object', properties: { change: { type: 'string', title: 'Change' } }, additionalProperties: false },
    response_schema: { type: 'object', properties: { window_start: { type: 'string', format: 'date-time', title: 'Window starts' } }, additionalProperties: false },
    outcomes: ['scheduled', 'declined'], default_priority: 'high', default_expiry_minutes: 1440 })
  await api.call('POST', `/human-types/${dw.id}/publish`, undefined, { 'If-Match': `"${dw.versions[0].version}"` })

  // Schedules, a repository (a local bare remote) and a knowledge base (a Git repository in the space's folder).
  await api.call('POST', `/spaces/${sp.id}/schedules`, { name: 'weekday review', description: 'Morning look at the queue', kind: 'cron', cron: '0 9 * * 1-5',
    timezone: 'Europe/Vilnius', missed: 'run_once', target: 'inbox', title: 'Morning review', body: 'Review the queue.' })
  const gitEnv = { ...process.env, GIT_AUTHOR_NAME: 'Oli', GIT_AUTHOR_EMAIL: 'oli@example.com', GIT_COMMITTER_NAME: 'Oli', GIT_COMMITTER_EMAIL: 'oli@example.com' }
  const remote = join(env('SM_DIR'), 'remotes', `overflow${t}.git`)
  const seed = join(env('SM_DIR'), 'remotes', `overflow-seed${t}`)
  mkdirSync(seed, { recursive: true })
  execFileSync('git', ['init', '-q', '--bare', '-b', 'main', remote], { env: gitEnv })
  writeFileSync(join(seed, 'README.md'), '# Settlement service\n')
  for (const args of [['init', '-q', '-b', 'main'], ['add', '-A'], ['commit', '-q', '-m', 'Skeleton'], ['remote', 'add', 'origin', remote], ['push', '-q', 'origin', 'main']])
    execFileSync('git', ['-C', seed, ...args], { env: gitEnv })
  await api.call('POST', `/spaces/${sp.id}/repositories`, { name: 'settlement', path: 'repos/settlement', url: `file://${remote}`, default_branch: 'main',
    description: 'Settlement batching service' })
  const kbRepo = join(env('SM_DIR'), 'agent', 'spaces', sp.workdir, 'repos', 'overflow-kb')
  mkdirSync(kbRepo, { recursive: true })
  writeFileSync(join(kbRepo, 'README.md'), '# Payments knowledge base\n')
  for (const args of [['init', '-q', '-b', 'trunk'], ['add', '-A'], ['commit', '-q', '-m', 'Start'], ['branch', 'main']]) execFileSync('git', ['-C', kbRepo, ...args], { env: gitEnv })
  await api.call('POST', `/spaces/${sp.id}/kbs`, { slug: 'payments', name: 'Payments knowledge', repo: 'repos/overflow-kb', base_branch: 'main', space_role: 'kb-support',
    write_policy: 'review', max_sessions: 1, description: 'Decisions and runbooks.' })

  // Cross-space: the docs site's control asks payments a question.
  const noInbound = { discover: false, questions: false, delegation: false, knowledge: false, kbs: [], disclosure: 'none', max_per_hour: 20, max_pending: 5 }
  await api.call('PUT', `/spaces/${sp.id}/xspace/peers/${peer.id}`, { inbound: { ...noInbound, discover: true, questions: true, disclosure: 'summary' },
    outbound: { questions: true, delegation: false, knowledge: false } })
  await api.call('PUT', `/spaces/${peer.id}/xspace/peers/${sp.id}`, { inbound: noInbound, outbound: { questions: true, delegation: false, knowledge: false } })
  const asker = await api.call<Run>('POST', `/spaces/${peer.id}/runs`, { kind: 'control', role: 'control', harness: 'claude-code', prompt: 'Keep the guides current.\n' +
    `/call spaceman_xspace_request {"to":"overflow-payments${t}","kind":"question","title":"Which retry budget should the settlement guide document?","body":"What is the current budget?","deadline_minutes":1440}` })
  await expect.poll(async () => (await api.call<{ items: unknown[] }>('GET', `/spaces/${sp.id}/xspace/requests`)).items.length, { timeout: 30_000 }).toBeGreaterThan(0)
  await api.call('POST', `/spaces/${peer.id}/runs/${asker.id}/stop`, { mode: 'kill', reason: 'asked' })

  // Events, the MCP gateway, a Git key and an API token.
  const pulse = env('SM_PULSE_DIR')
  const conn = await api.call<{ id: string }>('POST', '/event-connections', { name: `pulse-dev${t}`, url: readFileSync(join(pulse, 'url'), 'utf8').trim(),
    creds: readFileSync(join(pulse, 'spaceman.creds'), 'utf8'), enabled: true })
  const catalog = await api.call<{ items: { slug: string; id: string; versions: { id: string }[] }[] }>('GET', '/event-templates')
  const issueTpl = catalog.items.find((t) => t.slug === 'github-issue')!
  const sub = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/subscriptions`, { name: 'settlement-service issues', connection_id: conn.id, template_version_id: issueTpl.versions[0].id,
    follow: 'latest', vars: { account_key: 'example-org', repository: 'example-org/settlement-service' } })
  const mcpUrl = readFileSync(env('SM_MCP_URL_FILE'), 'utf8').trim()
  const mcp = await api.call<{ id: string }>('POST', '/mcp-servers', { slug: `overflow-tickets${t}`, name: 'Ticket tracker', url: mcpUrl, allow_private: true,
    description: 'Development fake.', headers: { Authorization: 'Bearer dev-mcp-token' } })
  await api.call('POST', `/mcp-servers/${mcp.id}/grants`, { space_id: sp.id, role: 'control' })
  await api.call('POST', '/mcp-servers/spaceman', { slug: `overflow-spaceman${t}`, name: 'SpaceMan admin', preset: 'admin', tools: [], approval: 'writes',
    grants: [{ role: 'admin', domain: '*' }], expires_in_days: 30 })
  await api.call('POST', '/git-ssh-keys', { name: `overflow-bot${t}`, description: 'Read-only machine user', spaces: [sp.id] })
  await api.call('POST', '/tokens', { name: `overflow-ci${t}`, grants: [{ role: 'viewer', domain: '*' }], expires_in_days: 30 })
  return { space: sp.id, peer: peer.id, item: item.id, run: done.id, live: run.id, prompt: prompt.id, template: issueTpl.id, subscription: sub.id }
}
