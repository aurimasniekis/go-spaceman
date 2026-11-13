import { Button, Checkbox, Drawer, Group, Modal, Text } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { api, unwrap } from '@/api/client'
import { useState } from 'react'
import { useHumanSearch, useMe } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { ActionDetail } from '@/features/human/ActionDetail'
import { ActionFilters, useActionFilters } from '@/features/human/ActionFilters'
import { ActionList } from '@/features/human/ActionList'
import { isPending } from '@/features/human/badges'
import { NewActionModal } from '@/features/human/NewActionModal'
import { PoliciesPanel } from '@/features/human/PoliciesPanel'
import { SpaceTypesModal } from '@/features/human/SpaceTypesModal'
import { TypePermissionsModal } from '@/features/human/TypePermissionsModal'
import { atLeast, roleIn } from '@/lib/access'
import { useView } from '@/lib/viewPrefs'

export const Route = createFileRoute('/spaces/$spaceId/human')({
  // ?action=<id> opens that action (links from task blockers, notifications and the attention view, SM-HUI-006).
  validateSearch: (s: Record<string, unknown>): { action?: string } => (typeof s.action === 'string' ? { action: s.action } : {}),
  component: SpaceHumanPage,
})

function SpaceHumanPage() {
  const { spaceId } = Route.useParams()
  const filters = useActionFilters(`human.space.${spaceId}`)
  const [view, setView] = useView('human.space.final', { showFinal: false })
  const showFinal = view.showFinal || filters.f.status !== ''
  const list = useHumanSearch(spaceId, { ...filters.query, pending: !showFinal })
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  const linked = useQuery({
    queryKey: ['human-action', spaceId, search.action ?? '', 'linked'],
    enabled: !!search.action,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/human-actions/{action_id}', { params: { path: { space_id: spaceId, action_id: search.action! } }, signal })),
  })
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [creating, setCreating] = useState(false)
  const [types, setTypes] = useState(false)
  const [policies, setPolicies] = useState(false)
  const [perms, setPerms] = useState(false)
  const items = list.data?.items ?? []
  return (
    <>
      <SectionHeader title="Human actions" description="Questions, approvals and tasks for people in this space"
        actions={(
          <>
            <Button variant="default" onClick={() => setPolicies(true)}>Policies</Button>
            {canWrite && <Button variant="default" onClick={() => setTypes(true)}>Types</Button>}
            <Button variant="default" onClick={() => setPerms(true)}>Permissions</Button>
            {canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New action</Button>}
          </>
        )} />
      <ErrorAlert error={list.error ?? linked.error} />
      <ActionFilters filters={filters} />
      <Group justify="space-between" mb={6}>
        <Text size="xs" c="dimmed" aria-live="polite">{items.filter(isPending).length} pending{filters.active ? ' matching' : ''}{showFinal ? `, ${items.filter((a) => !isPending(a)).length} final` : ''}</Text>
        <Checkbox size="xs" label="Show resolved, withdrawn and expired" checked={showFinal} disabled={filters.f.status !== ''}
          onChange={(e) => setView({ showFinal: e.currentTarget.checked })} />
      </Group>
      <ActionList actions={items} empty={filters.active ? 'Nothing matches the filters.' : 'No pending human actions in this space.'} />
      <Drawer opened={!!search.action && !!linked.data} onClose={() => void navigate({ search: {} })} position="right" size="lg" title="Human action">
        {linked.data && <ActionDetail action={linked.data} />}
      </Drawer>
      <NewActionModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)} />
      {types && <SpaceTypesModal spaceId={spaceId} onClose={() => setTypes(false)} />}
      {perms && <TypePermissionsModal spaceId={spaceId} canAdmin={atLeast(roleIn(me, spaceId), 'admin')} onClose={() => setPerms(false)} />}
      <Modal opened={policies} onClose={() => setPolicies(false)} title="Reminders, escalation and routing in this space" size="85rem">
        <Text size="sm" c="dimmed" mb="sm">The policy in force for each type, and where it comes from. A space policy replaces the installation default for its types.</Text>
        <PoliciesPanel spaceId={spaceId} canWrite={canWrite} />
      </Modal>
    </>
  )
}
