import { Center, Group, Paper, Stack, Text, Title } from '@mantine/core'
import type { ReactNode } from 'react'
import { Logo } from '@/components/Logo'

export function AuthLayout({ title, subtitle, children, width = 380 }: { title: string; subtitle?: ReactNode; children: ReactNode; width?: number }) {
  return (
    <Center mih="100vh" p="md">
      <Stack w={width} gap="md">
        <Group gap="xs" justify="center">
          <Logo size={26} />
          <Text fw={700} size="xl">SpaceMan</Text>
        </Group>
        <Paper withBorder p="lg" radius="md" bg="var(--sm-surface)">
          <Stack gap="md">
            <Stack gap={2}>
              <Title order={4}>{title}</Title>
              {subtitle && <Text size="sm" c="dimmed">{subtitle}</Text>}
            </Stack>
            {children}
          </Stack>
        </Paper>
      </Stack>
    </Center>
  )
}
