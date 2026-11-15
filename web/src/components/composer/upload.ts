import { apiError, type Schemas } from '@/api/client'
import { API_PREFIX, csrfToken, endMiniAppSession, isMiniApp, miniAppFetchInit } from '@/api/session'

export type UploadedAttachment = Schemas['AttachmentOut']

/** Uploads one file to a space (multipart, as the generated client does not stream files). */
export async function uploadAttachment(spaceId: string, file: File): Promise<UploadedAttachment> {
  const form = new FormData()
  form.append('file', file, file.name)
  // The Mini App has no cookie and no CSRF token: its session is the Authorization header (ADR-0083 §3.4.2), exactly as
  // the API client's middleware sends it for every other call.
  const miniApp = isMiniApp()
  const url = `${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/attachments`
  const idem = { 'Idempotency-Key': crypto.randomUUID() }
  const res = await fetch(
    url,
    miniApp
      ? miniAppFetchInit({ method: 'POST', body: form, headers: idem })
      : { method: 'POST', body: form, headers: { ...idem, 'X-CSRF-Token': csrfToken() }, credentials: 'same-origin' },
  )
  if (miniApp && res.status === 401) await endMiniAppSession(res)
  const body: unknown = await res.json().catch(() => ({}))
  if (!res.ok) throw apiError(body, res.status)
  return body as UploadedAttachment
}

/** Human-readable size. */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`
  if (n < 1024 ** 3) return `${(n / 1024 / 1024).toFixed(1)} MB`
  if (n < 1024 ** 4) return `${(n / 1024 ** 3).toFixed(1)} GB`
  return `${(n / 1024 ** 4).toFixed(1)} TB`
}
