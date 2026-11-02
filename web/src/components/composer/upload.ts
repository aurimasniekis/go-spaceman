import { apiError, type Schemas } from '@/api/client'
import { API_PREFIX, csrfToken } from '@/api/session'

export type UploadedAttachment = Schemas['AttachmentOut']

/** Uploads one file to a space (multipart, as the generated client does not stream files). */
export async function uploadAttachment(spaceId: string, file: File): Promise<UploadedAttachment> {
  const form = new FormData()
  form.append('file', file, file.name)
  const res = await fetch(`${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/attachments`, {
    method: 'POST',
    body: form,
    headers: { 'X-CSRF-Token': csrfToken(), 'Idempotency-Key': crypto.randomUUID() },
    credentials: 'same-origin',
  })
  const body: unknown = await res.json().catch(() => ({}))
  if (!res.ok) throw apiError(body, res.status)
  return body as UploadedAttachment
}

/** Human-readable size. */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
