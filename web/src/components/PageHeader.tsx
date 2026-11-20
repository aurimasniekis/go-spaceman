import { Group, Stack, Text, Title } from '@mantine/core'
import type { ReactNode } from 'react'
import type { HubKey } from '@/lib/nav'
import { HubTabs } from './HubTabs'

/** `hub` adds the tabs of the settings hub this page belongs to under the header. */
export function PageHeader({ title, description, actions, hub }: { title: ReactNode; description?: ReactNode; actions?: ReactNode; hub?: HubKey }) {
  return (
    <>
      <Group justify="space-between" align="flex-start" mb="md" wrap="nowrap">
        <Stack gap={2}>
          <Title order={3}>{title}</Title>
          {description && <Text size="sm" c="dimmed">{description}</Text>}
        </Stack>
        {actions && <Group gap="xs" wrap="nowrap">{actions}</Group>}
      </Group>
      {hub && <HubTabs hub={hub} />}
    </>
  )
}
