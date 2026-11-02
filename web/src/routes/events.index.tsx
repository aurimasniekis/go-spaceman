import { Badge, Button, Card, Group, SimpleGrid, Stack, Table, Tabs, Text } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useConnections, useMe, useTemplates } from '@/api/queries'
import type { EventConnection } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ConnectionModal } from '@/features/events/ConnectionModal'
import { TemplateCreateModal } from '@/features/events/TemplateEditor'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'

export const Route = createFileRoute('/events/')({ component: EventsPage })

function EventsPage() {
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const templates = useTemplates()
  const conns = useConnections()
  const [editing, setEditing] = useState<EventConnection | 'new' | null>(null)
  const [creating, setCreating] = useState(false)
  return (
    <>
      <PageHeader title="Events" description="Pulse connections and the catalog of templates spaces subscribe with." />
      <Tabs defaultValue="templates">
        <Tabs.List>
          <Tabs.Tab value="templates">Templates</Tabs.Tab>
          <Tabs.Tab value="connections">Connections</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="templates" pt="sm">
          <Group justify="space-between" mb="sm">
            <Text size="sm" c="dimmed">A template selects events by subject and payload and turns each into an inbox item. Published versions never change.</Text>
            {admin && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New template</Button>}
          </Group>
          <ErrorAlert error={templates.error} />
          <SimpleGrid cols={{ base: 1, md: 2 }}>
            {(templates.data ?? []).map((t) => {
              const latest = t.versions.find((v) => v.status === 'published')
              const draft = t.versions.find((v) => v.status === 'draft')
              return (
                <Card key={t.id}>
                  <Stack gap={6}>
                    <Group justify="space-between" wrap="nowrap">
                      <AnchorLink to="/events/templates/$templateId" params={{ templateId: t.id }} fw={600}>{t.name}</AnchorLink>
                      <Group gap={4}>
                        {t.builtin && <Badge variant="outline" color="owlPurple">built-in</Badge>}
                        {latest && <Badge variant="light">v{latest.version}</Badge>}
                        {draft && <Badge variant="outline" color="owlYellow">draft v{draft.version}</Badge>}
                      </Group>
                    </Group>
                    <Text size="xs" c="dimmed" className="mono">{t.slug}</Text>
                    <Text size="sm">{t.description}</Text>
                    {latest && <Text size="xs" className="mono" c="dimmed">{latest.definition.subject}</Text>}
                  </Stack>
                </Card>
              )
            })}
          </SimpleGrid>
        </Tabs.Panel>
        <Tabs.Panel value="connections" pt="sm">
          <Group justify="space-between" mb="sm">
            <Text size="sm" c="dimmed">How SpaceMan reaches Pulse. Each subscription needs a durable grant for this client in Pulse.</Text>
            {admin && <Button leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add connection</Button>}
          </Group>
          <ErrorAlert error={conns.error} />
          <Card p={0}>
            <Table>
              <Table.Thead>
                <Table.Tr><Table.Th>Name</Table.Th><Table.Th>URL</Table.Th><Table.Th>Pulse client</Table.Th><Table.Th>Stream</Table.Th><Table.Th>State</Table.Th><Table.Th>Updated</Table.Th>{admin && <Table.Th w={80} />}</Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {(conns.data ?? []).map((c) => (
                  <Table.Tr key={c.id}>
                    <Table.Td className="mono">{c.name}</Table.Td>
                    <Table.Td className="mono">{c.url}</Table.Td>
                    <Table.Td className="mono">{c.client_id}</Table.Td>
                    <Table.Td className="mono">{c.stream}</Table.Td>
                    <Table.Td><Badge color={c.enabled ? 'owlGreen' : 'gray'}>{c.enabled ? 'enabled' : 'disabled'}</Badge></Table.Td>
                    <Table.Td><Text size="xs" c="dimmed">{datetime(c.updated_at)}</Text></Table.Td>
                    {admin && <Table.Td><Button size="compact-xs" variant="subtle" onClick={() => setEditing(c)}>Edit</Button></Table.Td>}
                  </Table.Tr>
                ))}
                {conns.isSuccess && conns.data.length === 0 && <Table.Tr><Table.Td colSpan={7}><Text size="sm" c="dimmed" ta="center" p="md">No connections yet.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Card>
        </Tabs.Panel>
      </Tabs>
      {editing && <ConnectionModal conn={editing === 'new' ? undefined : editing} opened onClose={() => setEditing(null)} />}
      <TemplateCreateModal opened={creating} onClose={() => setCreating(false)} />
    </>
  )
}
