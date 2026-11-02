import createClient from 'openapi-fetch'
import type { components, paths } from './schema.gen'
import { API_PREFIX, sessionMiddleware } from './session'

/** Typed API client; types are generated from api/openapi.json (make api). */
export const api = createClient<paths>({ baseUrl: API_PREFIX })
api.use(sessionMiddleware)

export type Schemas = components['schemas']
export type Me = Schemas['Me']
export type SystemInfo = Schemas['SystemInfo']
export type User = Schemas['AuthUser']
export type Grant = Schemas['AuthzGrant']
export type GrantInput = Schemas['AuthGrantInput']
export type Role = GrantInput['role']
export type APIToken = Schemas['AuthAPIToken']
export type SessionInfo = Schemas['AuthSessionInfo']
export type Node = Schemas['FleetNode']
export type Space = Schemas['FleetSpace']
export type Run = Schemas['FleetRun']
export type LaunchInput = Schemas['FleetLaunchInput']
export type RecordingRecord = Schemas['RecordingRecord']
export type AuditRecord = Schemas['AuditRecord']
export type InboxItem = Schemas['InboxItem']
export type Attachment = Schemas['AttachmentsAttachment']

/** Problem details (RFC 9457) with SpaceMan's stable `code`. */
export interface Problem {
  title?: string
  status?: number
  detail?: string
  code?: string
  errors?: { field?: string; message?: string }[]
}

/** An API error with its HTTP status, stable code and field errors. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string = '',
    readonly fields: Record<string, string> = {},
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export function apiError(problem: unknown, status: number): ApiError {
  const p = (problem ?? {}) as Problem
  const msg = p.detail || p.title || `HTTP ${status}`
  const fields: Record<string, string> = {}
  for (const e of p.errors ?? []) if (e.field && e.message) fields[e.field] = e.message
  return new ApiError(msg, status, p.code ?? '', fields)
}

/** Unwraps an openapi-fetch result or throws an ApiError. */
export function unwrap<T>(res: { data?: T; error?: unknown; response: Response }): T {
  if (res.error !== undefined || res.data === undefined) throw apiError(res.error, res.response.status)
  return res.data
}

/** Throws an ApiError for a failed result without a body (204 endpoints). */
export function check(res: { error?: unknown; response: Response }): void {
  if (res.error !== undefined || !res.response.ok) throw apiError(res.error, res.response.status)
}

/** A fresh idempotency key for one create attempt (retries of the same attempt reuse it). */
export function idempotencyKey(): string {
  return crypto.randomUUID()
}

/** The If-Match value for a resource version. */
export function ifMatch(version: number): string {
  return `"${version}"`
}

export type EventConnection = Schemas['EventsConnection']
export type EventTemplate = Schemas['EventsTemplate']
export type TemplateVersion = Schemas['EventsTemplateVersion']
export type Definition = Schemas['TemplateDefinition']
export type Subscription = Schemas['EventsSubscription']
export type Delivery = Schemas['EventsDelivery']
export type QuarantineEntry = Schemas['EventsQuarantineEntry']
export type Cursor = Schemas['EventsCursor']
export type Task = Schemas['QueueTask']
export type QueuePolicy = Schemas['QueuePolicy']
export type Selection = Schemas['QueueSelection']
export type HumanAction = Schemas['HumanAction']
export type Message = Schemas['MessagesMessage']
export type Prompt = Schemas['CatalogPrompt']
export type PromptVersion = Schemas['CatalogPromptVersion']
export type Profile = Schemas['CatalogProfile']
export type RoleTemplate = Schemas['CatalogRoleTemplate']
export type SpaceRole = Schemas['CatalogSpaceRole']
export type RolePreview = Schemas['Preview']
export type Account = Schemas['AccountsAccount']
export type AccountKind = Schemas['AccountsKind']
export type AccountBinding = Schemas['AccountsBinding']
export type HumanType = HumanAction['type']
