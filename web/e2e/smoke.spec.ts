import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, test, type Page } from '@playwright/test'

import { env, nextTotpWindow, startAgent } from './stack'
import { totp } from './totp'

/**
 * The first-run journey through the real UI against a `scripts/dev-stack.sh` stack: setup with the root token, sign-in
 * with TOTP enrollment, node creation and enrollment, a space, a launched run watched live, stopped, and replayed.
 * The harness is the stack's stub; no model is contacted. Screenshots go to SM_SHOTS (for review, not committed here).
 */

const shots = process.env.SM_SHOTS ?? join(process.cwd(), 'test-results', 'shots')
mkdirSync(shots, { recursive: true })
// Mantine transitions run in JavaScript (not CSS), so let them settle before capturing.
async function shot(page: Page, name: string) {
  await page.waitForTimeout(350)
  await page.screenshot({ path: join(shots, `${name}.png`), fullPage: false, animations: 'disabled' })
}

// A 1×1 PNG.
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64')

test.describe.configure({ mode: 'serial' })

test('first run: setup, sign-in, node, space, live run, replay', async ({ page }) => {
  test.setTimeout(480_000)
  const email = 'alex@example.com'
  // Generated per run (throwaway stack); saved with the TOTP secret in the stack directory for manual inspection.
  const password = `e2e-${randomBytes(18).toString('base64url')}`

  // Setup with the root token.
  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)
  await expect(page.getByText('SpaceMan is not set up yet')).toBeVisible()
  await shot(page, 'login-not-set-up')
  await page.getByRole('link', { name: 'Set up SpaceMan' }).click()
  await page.getByLabel('Root token').fill(readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  await page.getByLabel('Name').fill('Alex Admin')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByLabel('Confirm password').fill(password)
  await shot(page, 'setup')
  await page.getByRole('button', { name: 'Create administrator' }).click()
  await expect(page.getByText('Administrator created')).toBeVisible()
  await page.getByRole('link', { name: 'Sign in' }).click()

  // Password, then mandatory TOTP enrollment, recovery codes, and a fresh code.
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByText('Set up two-factor authentication')).toBeVisible()
  const secret = (await page.locator('code').first().innerText()).trim()
  expect(secret).toMatch(/^[A-Z2-7]+=*$/)
  await shot(page, 'mfa-enroll')
  await page.getByRole('textbox', { name: 'Code' }).fill(totp(secret))
  await page.getByRole('button', { name: 'Enable' }).click()
  await expect(page.getByText('Save your recovery codes')).toBeVisible()
  await page.getByRole('button', { name: /I saved them/ }).click()
  await nextTotpWindow()
  await page.getByRole('textbox', { name: 'Code' }).fill(totp(secret))
  await page.getByRole('button', { name: 'Verify' }).click()
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
  writeFileSync(join(env('SM_DIR'), 'admin.json'), JSON.stringify({ email, password, totp: secret }), { mode: 0o600 })

  // A node, enrolled with the one-time token shown once.
  await page.getByRole('link', { name: 'Nodes' }).click()
  await page.getByRole('button', { name: 'Add node' }).click()
  await page.getByLabel('Name').fill('dev-node')
  await page.getByLabel('Description').fill('Local development node')
  await page.getByRole('button', { name: 'Create and issue token' }).click()
  const token = (await page.getByTestId('secret').innerText()).trim()
  expect(token).toMatch(/^spaceman_en_/)
  await shot(page, 'node-enrollment-token')
  await page.getByRole('button', { name: 'Done' }).click()

  startAgent(token)
  await expect(page.getByText('connected', { exact: true })).toBeVisible({ timeout: 30_000 })
  await shot(page, 'nodes')

  // A space pinned to the node.
  await page.getByRole('link', { name: 'Spaces' }).click()
  await page.getByRole('button', { name: 'New space' }).click()
  await page.getByLabel('Name').fill('Payments platform')
  await page.getByLabel('Slug').fill('payments')
  await page.getByRole('textbox', { name: 'Pinned node' }).click()
  await page.getByRole('option', { name: 'dev-node' }).click()
  await page.getByLabel('Description', { exact: true }).fill('Card processing services and their release train')
  await page.getByLabel('Goals').fill('Keep the settlement pipeline green; ship the retry-budget change this week.')
  await page.getByRole('button', { name: 'Create space' }).click()
  await expect(page.getByRole('heading', { name: /Payments platform/ })).toBeVisible()
  await expect(page.getByText('valid')).toBeVisible({ timeout: 20_000 })
  await shot(page, 'space')

  // An account: runs cannot start until the space has one (SM-ACCT-003). The stub harness ignores the credential.
  await expect(page.getByText('No account is bound, so runs cannot start.')).toBeVisible()
  await page.getByRole('link', { name: 'Accounts' }).click()
  await page.getByRole('button', { name: 'Add account' }).click()
  await page.getByLabel('Name').fill('team-codex')
  await page.getByRole('textbox', { name: 'Kind' }).click()
  await page.getByRole('option', { name: /OpenAI API key/ }).click()
  await page.getByLabel('Credential', { exact: true }).fill('e2e-stub-openai-key')
  await page.getByRole('dialog').getByRole('button', { name: 'Add account' }).click()
  await expect(page.getByRole('cell', { name: 'team-codex', exact: true })).toBeVisible()
  await expect(page.getByText('e2e-stub-openai-key')).toHaveCount(0)
  await page.getByRole('link', { name: 'Spaces' }).click()
  await page.getByRole('link', { name: 'Payments platform' }).click()
  await page.getByRole('button', { name: 'Choose accounts' }).click()
  await page.getByLabel('Allow team-codex').check()
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('team-codex · Codex')).toBeVisible()

  // Launch a run and watch it live.
  await page.getByRole('button', { name: 'Launch run' }).click()
  await page.getByRole('textbox', { name: 'Harness' }).click()
  await page.getByRole('option', { name: /^Codex/ }).click()
  await expect(page.getByRole('textbox', { name: 'Account' })).toHaveAttribute('placeholder', 'Space default: team-codex')
  await page.getByLabel('Initial prompt').fill('Review the open pull requests and summarise the risky ones.')
  await shot(page, 'launch-run')
  await page.getByRole('button', { name: 'Launch', exact: true }).click()
  await expect(page).toHaveURL(/\/runs\/run_/)
  await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByText('running', { exact: true })).toBeVisible({ timeout: 20_000 })
  await page.waitForTimeout(4_000)
  await shot(page, 'run-live')

  // Stop it, then replay the recording.
  await page.getByRole('button', { name: 'Stop' }).click()
  await page.getByRole('menuitem', { name: 'Stop the program' }).click()
  await expect(page.getByText(/^(exited|failed)$/)).toBeVisible({ timeout: 30_000 })
  await page.getByText('Replay', { exact: true }).click()
  await expect(page.getByRole('button', { name: 'Play replay' })).toBeEnabled({ timeout: 20_000 })
  await page.waitForTimeout(500)
  await shot(page, 'run-replay')

  // Inbox: an operator item written in the composer, with an attached image and file, rendered safely and marked done.
  await page.getByRole('link', { name: 'Payments platform' }).click()
  await page.getByRole('navigation', { name: 'Space sections' }).getByRole('link', { name: /Inbox/ }).click()
  await page.getByRole('button', { name: 'New item' }).click()
  const editor = page.getByRole('textbox', { name: 'Item text' })
  await editor.click()
  await page.keyboard.type('Settlement retries fail on weekends')
  await page.keyboard.press('Enter')
  await page.keyboard.type('The batch job gives up after ')
  await page.keyboard.press('ControlOrMeta+b')
  await page.keyboard.type('three')
  await page.keyboard.press('ControlOrMeta+b')
  await page.keyboard.type(' attempts.')
  await page.locator('input[type=file]').setInputFiles([
    { name: 'retries.png', mimeType: 'image/png', buffer: PNG },
    { name: 'job.log', mimeType: 'text/plain', buffer: Buffer.from('2026-11-01 attempt 3 failed: timeout\n') },
  ])
  await expect(page.getByText('job.log ·')).toBeVisible()
  await expect(editor.locator('img')).toBeVisible()
  await shot(page, 'inbox-composer')
  await page.getByRole('button', { name: 'Add to inbox' }).click()
  await expect(page.getByRole('heading', { name: 'Settlement retries fail on weekends' })).toBeVisible()
  await expect(page.locator('.sm-markdown strong')).toHaveText('three')
  await expect(page.locator('.sm-markdown img')).toBeVisible()
  await expect(page.getByText('Attachments')).toBeVisible()
  await shot(page, 'inbox-item')
  await page.getByRole('button', { name: 'Mark done' }).click()
  await expect(page.getByText('Done', { exact: true }).first()).toBeVisible()

  // Events: a Pulse connection (devpulse), a GitHub issues subscription, a published go-pulse-github event that arrives
  // in the inbox, rendered as untrusted text.
  await page.getByRole('link', { name: 'Events' }).click()
  await page.getByRole('tab', { name: 'Connections' }).click()
  await page.getByRole('button', { name: 'Add connection' }).click()
  await page.getByLabel('Name').fill('dev-pulse')
  await page.getByLabel('URL').fill(readFileSync(join(env('SM_PULSE_DIR'), 'url'), 'utf8').trim())
  await page.getByLabel('Credentials (.creds)').fill(readFileSync(join(env('SM_PULSE_DIR'), 'spaceman.creds'), 'utf8'))
  await page.getByRole('button', { name: 'Add connection' }).last().click()
  await expect(page.getByRole('cell', { name: 'dev-pulse' })).toBeVisible()
  await page.getByRole('tab', { name: 'Templates' }).click()
  await page.getByRole('link', { name: 'GitHub issues' }).click()
  await page.getByRole('button', { name: 'Test' }).click()
  await expect(page.getByText('accepted', { exact: true })).toBeVisible()
  await shot(page, 'event-template')

  await page.getByRole('link', { name: 'Spaces' }).click()
  await page.getByRole('link', { name: 'Payments platform' }).click()
  await page.getByRole('navigation', { name: 'Space sections' }).getByRole('link', { name: 'Events' }).click()
  await page.getByRole('button', { name: 'Subscribe' }).click()
  await page.getByLabel('Name').fill('tool issues')
  await page.getByRole('textbox', { name: 'Connection' }).click()
  await page.getByRole('option', { name: 'dev-pulse' }).click()
  await page.getByRole('textbox', { name: 'Template' }).click()
  await page.getByRole('option', { name: 'GitHub issues · v1' }).click()
  await page.getByLabel('account_key').fill('example-org')
  await page.getByLabel('repository').fill('example-org/tool')
  await page.getByRole('button', { name: 'Subscribe' }).last().click()
  await expect(page.getByRole('heading', { name: 'tool issues' })).toBeVisible()
  await expect(page.getByText('running', { exact: true })).toBeVisible({ timeout: 30_000 })
  execFileSync(join(env('SM_BIN'), 'devpulse'), ['publish', '-dir', env('SM_PULSE_DIR'), '-subject', 'evt.github.example-org.webhook.issues.opened',
    '-type', 'github.webhook.issues.opened.v1', join(process.cwd(), '..', 'internal/events/template/testdata/pulse-github-issues-opened.json')])
  await expect(page.getByText('accepted', { exact: true })).toBeVisible({ timeout: 30_000 })
  await shot(page, 'event-subscription')
  await page.getByRole('link', { name: 'open item' }).click()
  await expect(page.getByRole('heading', { name: 'example-org/tool#42 opened: Bug <script>' })).toBeVisible()
  await expect(page.locator('.sm-markdown script')).toHaveCount(0)

  // Queue: the event becomes a task, a second task is added, the operator reorders with the keyboard and starts one.
  await page.getByRole('button', { name: 'Create task' }).click()
  await page.getByRole('button', { name: 'Add to queue' }).click()
  await expect(page.getByText('Done', { exact: true }).first()).toBeVisible()
  await page.getByRole('link', { name: 'Payments platform · Inbox' }).click()
  await page.getByRole('navigation', { name: 'Space sections' }).getByRole('link', { name: 'Queue' }).click()
  await page.getByRole('button', { name: 'New task' }).click()
  await page.getByLabel('Title').fill('Upgrade the Go toolchain')
  await page.getByRole('textbox', { name: 'Priority' }).click()
  await page.getByRole('option', { name: 'high' }).click()
  await page.getByRole('button', { name: 'Add to queue' }).click()
  const rows = page.getByRole('listitem')
  await expect(rows).toHaveCount(2)
  await expect(rows.first()).toContainText('Upgrade the Go toolchain')
  await rows.nth(1).focus()
  await page.keyboard.press('Alt+ArrowUp')
  await expect(rows.first()).toContainText('example-org/tool#42')
  await shot(page, 'queue')
  await rows.first().click()
  await page.getByRole('button', { name: 'Change state' }).click()
  await page.getByRole('menuitem', { name: 'running', exact: true }).click()
  await expect(page.getByRole('dialog').getByText('running', { exact: true })).toBeVisible()
  await page.keyboard.press('Escape')

  // Human actions: a blocking question on the task appears in the space tab and the global "Needs you" view (with the nav
  // badge), and is answered with its type-specific form; the task is unblocked.
  await page.getByRole('navigation', { name: 'Space sections' }).getByRole('link', { name: /Human actions/ }).click()
  await page.getByRole('button', { name: 'New action' }).click()
  const dlg = page.getByRole('dialog', { name: 'New human action' })
  await dlg.getByText('Question', { exact: true }).click()
  await dlg.getByLabel('Title').fill('Which region should the refunds service deploy to?')
  await dlg.getByLabel('Details').fill('The **EU** region has the new rate limiter; `us-east-1` is cheaper.')
  const offered = dlg.getByRole('textbox', { name: 'Offered answers' })
  await offered.fill('eu-west-1')
  await offered.press('Enter')
  await offered.fill('us-east-1')
  await offered.press('Enter')
  await dlg.getByRole('textbox', { name: 'Related task' }).click()
  await page.getByRole('option', { name: 'Upgrade the Go toolchain' }).click()
  await dlg.getByLabel('Block that task until someone responds').check()
  await dlg.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('listitem').filter({ hasText: 'Which region' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Needs you 1 pending' })).toBeVisible()
  await shot(page, 'human-space')
  await page.getByRole('link', { name: 'Needs you 1 pending' }).click()
  await expect(page.getByRole('heading', { name: 'Needs you' })).toBeVisible()
  await expect(page.getByRole('listitem').filter({ hasText: 'Payments platform' })).toBeVisible()
  await shot(page, 'human-global')
  await page.getByRole('listitem').filter({ hasText: 'Which region' }).click()
  await page.getByRole('radio', { name: 'eu-west-1' }).check()
  await shot(page, 'human-form-default')
  await page.getByRole('button', { name: 'Send answer' }).click()
  await expect(page.getByText('Response: answered')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByText('Nothing is waiting for you.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Needs you', exact: true })).toBeVisible()

  // Prompts: a role prompt is created as a draft and published as version 1.
  await page.getByRole('link', { name: 'Prompts' }).click()
  await page.getByRole('button', { name: 'New prompt' }).click()
  const np = page.getByRole('dialog', { name: 'New prompt' })
  await np.getByLabel('Name').fill('Payments reviewer')
  await np.getByLabel('Slug').fill('payments-reviewer')
  await np.getByLabel('First draft').fill('Review refunds for idempotency.')
  await np.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: 'Payments reviewer' })).toBeVisible()
  await page.getByRole('button', { name: 'Publish' }).click()
  await expect(page.getByText('Version 1', { exact: true })).toBeVisible()

  // Messages: an operator request to control is stored durably until a control run can see it.
  await page.getByRole('link', { name: 'Spaces' }).click()
  await page.getByRole('link', { name: 'Payments platform' }).click()
  await page.getByRole('navigation', { name: 'Space sections' }).getByRole('link', { name: /Messages/ }).click()
  await page.getByRole('button', { name: 'Send message' }).click()
  const compose = page.getByRole('dialog', { name: 'Send a message' })
  await compose.getByLabel('Subject').fill('Pause deploys until the bank window closes')
  await compose.getByRole('textbox', { name: 'Message' }).fill('The bank maintenance window ends at **06:00 UTC**.')
  await compose.getByLabel('Expect a reply (a request)').check()
  await compose.getByRole('button', { name: 'Send', exact: true }).click()
  const msg = page.getByRole('listitem', { name: /Pause deploys until the bank window closes/ })
  await expect(msg).toContainText('stored')
  await msg.click()
  await expect(page.getByText('Not yet seen by a recipient')).toBeVisible()
  await page.keyboard.press('Escape')

  // Audit shows what happened.
  await page.getByRole('link', { name: 'Audit log' }).click()
  await expect(page.getByText('run.launched').first()).toBeVisible()
  await shot(page, 'audit')

  // Password reset: an administrator issues a single-use token, the user sets a new password at /reset and signs in.
  await page.getByRole('link', { name: 'Users' }).click()
  await page.getByRole('button', { name: 'Actions for Alex Admin' }).click()
  await page.getByRole('menuitem', { name: 'Password reset token' }).click()
  const reset = (await page.getByTestId('secret').innerText()).trim()
  await page.getByRole('button', { name: 'Done' }).click()
  await page.goto('/reset')
  const newPassword = `e2e-${randomBytes(18).toString('base64url')}`
  await page.getByLabel('Reset token').fill(reset)
  await page.getByLabel('New password').fill(newPassword)
  await page.getByLabel('Confirm password').fill(newPassword)
  await page.getByRole('button', { name: 'Set password' }).click()
  await expect(page.getByText('Password changed')).toBeVisible()
  await page.getByRole('link', { name: 'Sign in' }).click()
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill(newPassword)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await nextTotpWindow()
  await page.getByRole('textbox', { name: 'Code' }).fill(totp(secret))
  await page.getByRole('button', { name: 'Verify' }).click()
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
})
