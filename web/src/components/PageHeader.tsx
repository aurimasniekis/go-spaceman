import { Group, Stack, Text, Title } from '@mantine/core'
import type { ReactNode } from 'react'

export function PageHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <Group justify="space-between" align="flex-start" mb="md" wrap="nowrap">
      <Stack gap={2}>
        <Title order={3}>{title}</Title>
        {description && <Text size="sm" c="dimmed">{description}</Text>}
      </Stack>
      {actions && <Group gap="xs" wrap="nowrap">{actions}</Group>}
    </Group>
  )
}
