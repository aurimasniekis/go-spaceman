import { Box } from '@mantine/core'
import { Outlet } from '@tanstack/react-router'
import { useWatch } from '@/api/watch'

/**
 * The Mini App's slim shell (plan §4.6): the existing pages in an Outlet, the change feed over the fetch transport, no
 * navbar and no sign-in gate (the session was exchanged before the router mounted). Back/Main buttons and theme: TG-9b-2.
 */
export function TgShell() {
  useWatch(true)
  return (
    <Box p="sm">
      <Outlet />
    </Box>
  )
}

