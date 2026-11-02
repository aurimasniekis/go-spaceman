import { execFileSync, spawn } from 'node:child_process'
import { openSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import type { APIRequestContext, Page } from '@playwright/test'

import { totp } from './totp'

/** A value from the env file printed by `scripts/dev-stack.sh up`. */
export const env = (k: string): string => {
  const v = process.env[k]
  if (!v) throw new Error(`${k} is not set: source the env file printed by scripts/dev-stack.sh up`)
  return v
}

/** The agent's environment: its own herdr-core core, nothing inherited from a surrounding hc terminal or SpaceMan setup. */
export function agentEnv(): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = {}
  for (const [k, v] of Object.entries(process.env)) if (!k.startsWith('HC_') && !k.startsWith('XDG_') && !k.startsWith('SPACEMAN_')) out[k] = v
  return { ...out, HC_SESSION: env('SM_HC_SESSION'), HC_SOCKET: env('SM_HC_SOCKET'), XDG_STATE_HOME: `${env('SM_HC_HOME')}/state`, XDG_CONFIG_HOME: `${env('SM_HC_HOME')}/config` }
}

/** Enrolls the stack's node agent with a one-time token and starts it (stopped by `dev-stack.sh down` via agent.pid). */
export function startAgent(token: string) {
  const agent = join(env('SM_BIN'), 'spaceman-agent')
  execFileSync(agent, ['-c', env('SM_AGENT_CONFIG'), 'enroll', '--token', token], { env: agentEnv() })
  const log = openSync(join(env('SM_DIR'), 'agent.log'), 'a')
  const child = spawn(agent, ['-c', env('SM_AGENT_CONFIG'), 'run'], { env: agentEnv(), stdio: ['ignore', log, log], detached: true })
  child.unref()
  writeFileSync(join(env('SM_DIR'), 'agent.pid'), String(child.pid))
}

/** Waits for the next 30-second TOTP step (a code is accepted once per step). */
export async function nextTotpWindow() {
  const ms = 30_000 - (Date.now() % 30_000) + 750
  await new Promise((r) => setTimeout(r, ms))
}

/**
 * A JSON API client on a request context. Cookie sessions send X-CSRF-Token and the page Origin on mutations, like the
 * UI does; `bearer` uses a token instead.
 */
export class Api {
  csrf = ''
  constructor(
    private readonly req: APIRequestContext,
    private readonly origin: string,
    private readonly bearer = '',
  ) {}

  async call<T = Record<string, unknown>>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
    const h: Record<string, string> = { Origin: this.origin, ...headers }
    if (this.bearer) h.Authorization = `Bearer ${this.bearer}`
    else if (method !== 'GET' && this.csrf) h['X-CSRF-Token'] = this.csrf
    const res = await this.req.fetch(`/api/v1${path}`, { method, data: body, headers: h })
    const text = await res.text()
    if (!res.ok()) throw new Error(`${method} ${path}: ${res.status()} ${text}`)
    const out = (text ? JSON.parse(text) : {}) as T & { csrf_token?: string; me?: { csrf_token?: string } }
    const token = out.csrf_token ?? out.me?.csrf_token
    if (token) this.csrf = token
    return out
  }

  /**
   * Signs in a user whose account requires TOTP: password, enrollment (first time), and verification with a code from
   * a later step. Returns the TOTP secret.
   */
  async signIn(email: string, password: string, secret?: string): Promise<string> {
    const me = await this.call<{ needs_enroll: boolean; limited: boolean }>('POST', '/auth/login', { email, password })
    if (!me.limited) return secret ?? ''
    if (me.needs_enroll) {
      const en = await this.call<{ secret: string }>('POST', '/auth/mfa/enroll')
      secret = en.secret
      await this.call('POST', '/auth/mfa/enroll/confirm', { code: totp(secret) })
      await nextTotpWindow()
    }
    await this.call('POST', '/auth/mfa/verify', { code: totp(secret!) })
    return secret!
  }
}

/** Mantine transitions run in JavaScript, so let them settle before a capture. */
export async function settle(page: Page, ms = 350) {
  await page.waitForTimeout(ms)
}
