import '@fontsource-variable/inter'
import '@fontsource-variable/jetbrains-mono'
import '@mantine/core/styles.css'
import '@mantine/notifications/styles.css'
import '../theme/global.css'

import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { setWatchTransport } from '@/api/transport'
import { installChunkReload } from '@/lib/chunkReload'
import { queryClient } from '@/router'
import { cssVariablesResolver, theme } from '@/theme/theme'
import { ReopenScreen } from './ReopenScreen'
import { tgRouter } from './router'
import { startSession, useTgSession } from './session'
import { fetchSseTransport } from './sseTransport'
import { webApp } from './webapp'

setWatchTransport(fetchSseTransport)
installChunkReload()

function Gate() {
  const s = useTgSession()
  if (s.status === 'ended') return <ReopenScreen reason={s.reason} />
  if (s.status === 'exchanging') return null
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={tgRouter} />
    </QueryClientProvider>
  )
}

webApp()?.ready()
webApp()?.expand()
void startSession()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <MantineProvider theme={theme} cssVariablesResolver={cssVariablesResolver} defaultColorScheme="dark">
      <Notifications position="bottom-right" limit={4} />
      <Gate />
    </MantineProvider>
  </StrictMode>,
)
