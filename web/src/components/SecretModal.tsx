import { Alert, Button, Code, CopyButton, Group, Modal, Stack, Text } from '@mantine/core'
import type { ReactNode } from 'react'

/** Shows a secret exactly once (tokens, enrollment tokens, recovery codes); it is never fetched again. */
export function SecretModal({ title, secret, onClose, children }: { title: string; secret: string | null; onClose: () => void; children?: ReactNode }) {
  return (
    <Modal opened={secret !== null} onClose={onClose} title={title} size="lg" closeOnClickOutside={false}>
      <Stack gap="sm">
        <Alert color="owlYellow" variant="light">Copy it now. SpaceMan stores only a verifier and cannot show it again.</Alert>
        <Code block className="mono" style={{ wordBreak: 'break-all', whiteSpace: 'pre-wrap' }} data-testid="secret">{secret}</Code>
        {children}
        <Group justify="space-between">
          <CopyButton value={secret ?? ''}>
            {({ copied, copy }) => <Button variant="light" onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}
          </CopyButton>
          <Button onClick={onClose}>Done</Button>
        </Group>
        <Text size="xs" c="dimmed">Treat it like a password: anyone holding it acts with its grants.</Text>
      </Stack>
    </Modal>
  )
}
