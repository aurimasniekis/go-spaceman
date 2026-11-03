import { notifications } from '@mantine/notifications'
import { errorMessage } from '@/components/ErrorAlert'

/** Toasts' close buttons are named for screen readers (Mantine leaves them unnamed). */
export const closeButtonProps = { 'aria-label': 'Dismiss notification' }

export const notifyOk = (message: string) => notifications.show({ color: 'owlGreen', message, closeButtonProps })
export const notifyError = (e: unknown, title = 'Failed') => notifications.show({ color: 'owlRed', title, message: errorMessage(e), closeButtonProps })
