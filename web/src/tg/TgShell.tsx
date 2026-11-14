import { Box } from '@mantine/core'
import { Outlet, useRouter } from '@tanstack/react-router'
import { useEffect, useMemo } from 'react'
import { useWatch } from '@/api/watch'
import { AttachmentsHostProvider } from '@/components/attachmentsHost'
import { useBackButton } from './buttons'
import { parseStartParam, startParam } from './deeplink'
import { useTelegramTheme } from './theme'
import { createAttachmentsHost } from './watchAttachments'

/**
 * The Mini App's slim shell (plan §4.6): the existing pages in an Outlet, the change feed over the fetch transport, no
 * navbar and no sign-in gate (the session was exchanged before the router mounted). Telegram's theme, BackButton and
 * start-parameter deep link are wired here; attachments load as blobs through the AttachmentsHost.
 */
export function TgShell() {
  useWatch(true)
  useTelegramTheme()
  useBackButton()
  useDeepLink()
  const host = useMemo(() => createAttachmentsHost(), [])
  useEffect(() => () => host.blobs.dispose(), [host])
  return (
    <AttachmentsHostProvider value={host}>
      <Box p="sm">
        <Outlet />
      </Box>
    </AttachmentsHostProvider>
  )
}

/** Opens the page the start parameter names, once, when the app was opened on its start page. */
function useDeepLink() {
  const router = useRouter()
  useEffect(() => {
    const link = parseStartParam(startParam())
    if (!link || router.state.location.pathname !== '/') return
    // `to` and `params` come from the fixed table in deeplink.ts, never from the parameter's text.
    void router.navigate({ to: link.to, params: link.params, search: link.search } as never)
  }, [router])
}
