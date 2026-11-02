import { Alert, Anchor, Badge, Breadcrumbs, Button, Card, Group, Select, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { IconPencil, IconUpload } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap } from '@/api/client'
import { keys, useMe, useTemplate } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { DefinitionView } from '@/features/events/DefinitionView'
import { PreviewPanel } from '@/features/events/PreviewPanel'
import { DraftModal } from '@/features/events/TemplateEditor'
import { hasGlobal } from '@/lib/access'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/events/templates/$templateId')({ component: TemplatePage })

function TemplatePage() {
  const { templateId } = Route.useParams()
  const qc = useQueryClient()
  const t = useTemplate(templateId)
  const me = useMe().data
  const admin = hasGlobal(me, 'admin')
  const [versionId, setVersionId] = useState<string | null>(null)
  const [editing, setEditing] = useState(false)
  if (t.error) return <ErrorAlert error={t.error} />
  if (!t.data) return null
  const tpl = t.data
  const v = tpl.versions.find((x) => x.id === versionId) ?? tpl.versions[0]
  const draft = tpl.versions.find((x) => x.status === 'draft')

  const publish = async () => {
    if (!draft) return
    try {
      const res = unwrap(await api.POST('/event-template-versions/{version_id}/publish', { params: { path: { version_id: draft.id } } }))
      void qc.invalidateQueries({ queryKey: keys.template(templateId) })
      void qc.invalidateQueries({ queryKey: keys.templates })
      notifyOk(res.held_subscriptions.length ? `Published v${res.version.version}; ${res.held_subscriptions.length} following subscription(s) stay on their version (subject changed)` : `Published v${res.version.version}`)
    } catch (e) {
      notifyError(e, 'Could not publish')
    }
  }

  return (
    <Stack gap="md">
      <Breadcrumbs><Anchor component={Link} to="/events" size="sm">Events</Anchor><Text size="sm">{tpl.name}</Text></Breadcrumbs>
      <Group justify="space-between" align="flex-start">
        <Stack gap={2}>
          <Group gap="xs"><Title order={3}>{tpl.name}</Title>{tpl.builtin && <Badge variant="outline" color="owlPurple">built-in</Badge>}</Group>
          <Text size="sm" c="dimmed">{tpl.description}</Text>
        </Stack>
        {admin && (
          <Group gap="xs">
            <Button variant="default" leftSection={<IconPencil size={14} />} onClick={() => setEditing(true)}>{draft ? 'Edit draft' : 'New draft'}</Button>
            {draft && <Button leftSection={<IconUpload size={14} />} onClick={() => void publish()}>Publish v{draft.version}</Button>}
          </Group>
        )}
      </Group>
      <Group gap="xs">
        <Select aria-label="Version" w={260} allowDeselect={false} value={v.id} onChange={setVersionId}
          data={tpl.versions.map((x) => ({ value: x.id, label: `v${x.version} · ${x.status}${x.published_at ? ` · ${datetime(x.published_at)}` : ''}` }))} />
        {v.notes && <Text size="xs" c="dimmed">{v.notes}</Text>}
      </Group>
      {v.status === 'draft' && <Alert color="owlYellow" variant="light">Drafts can be tested here but not subscribed to until published.</Alert>}
      <SimpleGrid cols={{ base: 1, lg: 2 }}>
        <Card><Stack gap="xs"><Text fw={600}>Definition</Text><DefinitionView def={v.definition} /></Stack></Card>
        <Card><Stack gap="xs"><Text fw={600}>Test with a sample event</Text><PreviewPanel key={v.id} def={v.definition} slug={tpl.slug} /></Stack></Card>
      </SimpleGrid>
      {editing && <DraftModal templateId={tpl.id} initial={(draft ?? v).definition} opened onClose={() => setEditing(false)} />}
    </Stack>
  )
}
