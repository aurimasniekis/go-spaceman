import { Alert, Button, Stack, Text } from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'
import { Component, type ReactNode } from 'react'
import { errorMessage } from './ErrorAlert'

/** The alert shown in place of a part of a page that failed to render, with a way to try again. */
export function LocalErrorAlert({ error, title, reset }: { error: unknown; title: string; reset?: () => void }) {
  return (
    <Alert color="owlRed" title={title} icon={<IconAlertTriangle size={16} />} my="sm" role="alert">
      <Stack gap="xs" align="flex-start">
        <Text size="sm">{errorMessage(error)}</Text>
        {reset && <Button size="xs" variant="default" onClick={reset}>Try again</Button>}
      </Stack>
    </Alert>
  )
}

/**
 * Contains a rendering failure to one part of a page: the rest of the route keeps working and the failed part shows an
 * alert with a retry instead of the whole page going blank.
 */
export class LocalErrorBoundary extends Component<{ title: string; children: ReactNode }, { error: unknown; failed: boolean }> {
  state = { error: undefined as unknown, failed: false }

  static getDerivedStateFromError(error: unknown) {
    return { error, failed: true }
  }

  render() {
    if (this.state.failed) {
      return <LocalErrorAlert error={this.state.error} title={this.props.title} reset={() => this.setState({ error: undefined, failed: false })} />
    }
    return this.props.children
  }
}
