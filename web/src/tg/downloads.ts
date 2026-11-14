import { notifications } from '@mantine/notifications'
import { api } from '@/api/client'
import type { AttachmentRef } from './attachments'
import { webApp } from './webapp'

/**
 * Downloads in the Mini App (ADR-0083 §3.4.3): the session cannot ride in a URL, so a click mints a signed link on the
 * Mini App host (60 s, 3 uses) and hands it to Telegram's downloadFile. The server refuses a mint with 429 when the
 * caller or the host has too many live links; that is a "try again shortly", not an error screen. The link is a
 * capability: it is never logged or shown.
 */
export async function downloadAttachment(ref: AttachmentRef, fileName?: string): Promise<void> {
  const res = await api.POST('/telegram/download-links', { body: { space_id: ref.spaceId, attachment_id: ref.id } }).catch(() => null)
  if (!res) {
    notifications.show({ color: 'red', message: 'Could not reach SpaceMan. Try again.' })
    return
  }
  if (res.response.status === 429) {
    const wait = Number(res.response.headers.get('Retry-After'))
    notifications.show({ color: 'yellow', message: `Too many downloads at once. Try again${wait > 0 ? ` in ${Math.min(wait, 60)} s` : ' shortly'}.` })
    return
  }
  if (!res.data) {
    // A 401 already ended the session (the middleware); anything else is a refusal or a missing file.
    if (res.response.status !== 401) notifications.show({ color: 'red', message: 'This file cannot be downloaded.' })
    return
  }
  const url = res.data.url
  const app = webApp()
  if (app?.downloadFile) app.downloadFile({ url, file_name: fileName })
  else if (app?.openLink) app.openLink(url)
  else window.open(url, '_blank', 'noopener,noreferrer')
}
