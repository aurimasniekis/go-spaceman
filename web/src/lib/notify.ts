import { notifications } from '@mantine/notifications'
import { errorMessage } from '@/components/ErrorAlert'

export const notifyOk = (message: string) => notifications.show({ color: 'owlGreen', message })
export const notifyError = (e: unknown, title = 'Failed') => notifications.show({ color: 'owlRed', title, message: errorMessage(e) })
