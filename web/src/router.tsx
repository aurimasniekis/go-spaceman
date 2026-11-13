import { QueryClient } from '@tanstack/react-query'
import { createRouter } from '@tanstack/react-router'
import { ApiError } from './api/client'
import { RouteError } from './components/RouteError'
import { routeTree } from './routeTree.gen'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The change feed (useWatch) invalidates what changes, so cached data is reused on mount and navigation instead
      // of refetched; data the feed does not cover is refetched on a new mount after a minute.
      staleTime: 60_000,
      // Authorization and validation failures are answers, not transient errors.
      retry: (n, e) => !(e instanceof ApiError && e.status >= 400 && e.status < 500) && n < 1,
      refetchOnWindowFocus: false,
    },
  },
})

export const router = createRouter({ routeTree, context: { queryClient }, defaultPreload: 'intent', defaultErrorComponent: RouteError })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
