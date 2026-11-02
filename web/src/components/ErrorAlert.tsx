import { Alert } from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'
import { ApiError } from '@/api/client'

export function errorMessage(e: unknown): string {
  if (e instanceof ApiError) {
    const extra = Object.entries(e.fields).map(([f, m]) => `${f}: ${m}`).join('; ')
    return extra ? `${e.message} (${extra})` : e.message
  }
  return e instanceof Error ? e.message : String(e)
}

export function ErrorAlert({ error, title = 'Request failed' }: { error: unknown; title?: string }) {
  if (!error) return null
  const forbidden = error instanceof ApiError && error.status === 403
  return (
    <Alert color={forbidden ? 'owlYellow' : 'owlRed'} title={forbidden ? 'Not permitted' : title} icon={<IconAlertTriangle size={16} />} my="sm">
      {forbidden ? 'Your grants do not include this. Ask an administrator for the role you need.' : errorMessage(error)}
    </Alert>
  )
}
