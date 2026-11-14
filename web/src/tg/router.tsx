import { createRouter } from '@tanstack/react-router'
import { queryClient, routerOptions } from '@/router'

/** The Mini App's router: the web's route tree and error handling, in the slim shell (context.miniApp). */
export const tgRouter = createRouter({ ...routerOptions, context: { queryClient, miniApp: true } })
