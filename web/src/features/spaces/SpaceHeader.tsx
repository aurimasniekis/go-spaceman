import { Button, Group, Menu, Skeleton, Stack, Text, Title, Tooltip } from '@mantine/core'
import {
  IconArchive, IconArrowsExchange, IconChevronDown, IconDots, IconFileExport, IconInbox, IconPencil, IconPlayerPause, IconPlayerPlay, IconRestore,
  IconRocket, IconTrash,
} from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useMatch, useNavigate } from '@tanstack/react-router'
import { useState, type ReactNode } from 'react'
import { api, ifMatch, unwrap, type Space } from '@/api/client'
import { keys, useMe, useNodes, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { SpaceState } from '@/components/StatusBadge'
import { useCanCreateInboxItem, useNewInboxItem } from '@/features/inbox/useNewInboxItem'
import { ExportModal } from '@/features/spaces/ExportModal'
import { LaunchRunModal } from '@/features/spaces/LaunchRunModal'
import { RelocateModal } from '@/features/spaces/RelocateModal'
import { SpaceFormModal } from '@/features/spaces/SpaceFormModal'
import { SpaceLifecycleDialog, useSpaceLifecycleAccess, type Dialog } from '@/features/spaces/SpaceLifecycle'
import { atLeast, hasGlobal, roleIn } from '@/lib/access'
import { useNarrow } from '@/lib/media'
import { notifyError, notifyOk } from '@/lib/notify'

const PAUSE_MODES: { mode: 'hold' | 'interrupt' | 'emergency'; label: string; detail: string }[] = [
  { mode: 'hold', label: 'Hold', detail: 'No new work starts; running agents continue' },
  { mode: 'interrupt', label: 'Interrupt', detail: 'Interrupt running agents and stop them' },
  { mode: 'emergency', label: 'Emergency stop', detail: 'Stop every run now' },
]

/**
 * The space's header on every space page (UX review C6): its name, state and node, and the space-wide actions. Launch
 * run is the primary action; pausing has its own menu; everything else (edit, export, relocate, archive, restore,
 * purge) is in the ⋯ menu.
 */
export function SpaceHeader({ spaceId }: { spaceId: string }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const space = useSpace(spaceId)
  const nodes = useNodes()
  const me = useMe().data
  const role = roleIn(me, spaceId)
  const life = useSpaceLifecycleAccess(space.data)
  const canMove = hasGlobal(me, 'admin') // relocation is global administration (space:relocate)
  const [open, setOpen] = useState<'launch' | 'edit' | 'relocate' | 'export' | null>(null)
  const [dialog, setDialog] = useState<Dialog>(null)
  // "New inbox item" sits next to Launch run on the overview only: the Inbox page has its own "New item".
  const onOverview = useMatch({ from: '/spaces/$spaceId/', shouldThrow: false }) !== undefined
  const canAddItem = useCanCreateInboxItem(spaceId)
  const newItem = useNewInboxItem(spaceId)
  const narrow = useNarrow()
  if (!space.data) {
    return <Group mb="sm"><Skeleton h={28} w={240} /></Group>
  }
  const s = space.data
  const node = nodes.data?.find((n) => n.id === s.node_id)
  const setState = async (state: Space['state'], mode?: 'hold' | 'interrupt' | 'emergency') => {
    try {
      const next = unwrap(await api.POST('/spaces/{space_id}/state', { params: { path: { space_id: s.id }, header: { 'If-Match': ifMatch(s.version) } }, body: { state, mode } }))
      qc.setQueryData(keys.space(s.id), next)
      notifyOk(state === 'paused' ? `Space paused (${mode})` : state === 'active' ? 'Space resumed' : `Space ${state}`)
    } catch (e) {
      notifyError(e, 'Could not change the space state')
    }
  }
  const more: ReactNode[] = []
  if (atLeast(role, 'admin')) {
    more.push(<Menu.Item key="edit" leftSection={<IconPencil size={14} />} onClick={() => setOpen('edit')}>Edit…</Menu.Item>)
    more.push(<Menu.Item key="export" leftSection={<IconFileExport size={14} />} onClick={() => setOpen('export')}>Export…</Menu.Item>)
  }
  if (canMove && s.state !== 'archived' && !s.desired_node_id) {
    more.push(<Menu.Item key="relocate" leftSection={<IconArrowsExchange size={14} />} onClick={() => setOpen('relocate')}>Relocate…</Menu.Item>)
  }
  const lifecycle: ReactNode[] = []
  if (life.archive) lifecycle.push(<Menu.Item key="archive" leftSection={<IconArchive size={14} />} onClick={() => setDialog('archive')}>Archive…</Menu.Item>)
  if (life.restore) lifecycle.push(<Menu.Item key="restore" leftSection={<IconRestore size={14} />} onClick={() => setDialog('restore')}>Restore…</Menu.Item>)
  if (life.purge) lifecycle.push(<Menu.Item key="purge" color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => setDialog('purge')}>Purge…</Menu.Item>)
  return (
    <>
      {/* The actions wrap under the name when both don't fit (a phone), instead of squeezing the name to a few letters. */}
      <Group justify="space-between" align="flex-start" wrap="wrap" gap="xs" mb="xs" data-testid="space-header">
        <Stack gap={2} style={{ minWidth: 0, flex: '1 1 auto' }}>
          <Group gap="xs" wrap="nowrap">
            <Title order={3} style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{s.name}</Title>
            <SpaceState space={s} />
          </Group>
          <Text size="xs" c="dimmed">
            <span className="mono">{s.slug}</span>
            {' · node '}
            <AnchorLink to="/nodes" size="xs" c="dimmed">{node?.name ?? s.node_id}</AnchorLink>
            {node && ` · ${node.state !== 'active' ? node.state : node.connected ? 'connected' : 'offline'}`}
            {s.desired_node_id && s.desired_node_id !== s.node_id && ' · relocating'}
          </Text>
        </Stack>
        <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0, marginLeft: 'auto' }}>
          {atLeast(role, 'operator') && s.state === 'active' && (
            <Menu position="bottom-end">
              <Menu.Target>
                <Button variant="default" leftSection={<IconPlayerPause size={14} />} rightSection={<IconChevronDown size={12} />}>Pause</Button>
              </Menu.Target>
              <Menu.Dropdown>
                {PAUSE_MODES.map((p) => (
                  <Menu.Item key={p.mode} color={p.mode === 'emergency' ? 'owlRed' : undefined} onClick={() => void setState('paused', p.mode)}>
                    <Text size="sm">{p.label}</Text>
                    <Text size="xs" c="dimmed">{p.detail}</Text>
                  </Menu.Item>
                ))}
              </Menu.Dropdown>
            </Menu>
          )}
          {atLeast(role, 'operator') && s.state === 'paused' && <Button variant="default" leftSection={<IconPlayerPlay size={14} />} onClick={() => void setState('active')}>Resume</Button>}
          {onOverview && canAddItem && (
            // On a phone, the icon alone keeps the actions on one line next to Launch run.
            <Tooltip label="Add an item to this space's inbox for control to triage">
              {narrow ? (
                <Button variant="default" px="xs" aria-label="New inbox item" onClick={() => newItem.open()}><IconInbox size={16} /></Button>
              ) : (
                <Button variant="default" leftSection={<IconInbox size={14} />} onClick={() => newItem.open()}>New inbox item</Button>
              )}
            </Tooltip>
          )}
          {atLeast(role, 'operator') && (
            <Tooltip label={s.state !== 'active' ? 'The space is not active' : 'Start an agent in this space'}>
              <Button leftSection={<IconRocket size={14} />} disabled={s.state !== 'active'} onClick={() => setOpen('launch')}>Launch run</Button>
            </Tooltip>
          )}
          {(more.length > 0 || lifecycle.length > 0) && (
            <Menu position="bottom-end">
              <Menu.Target>
                <Button variant="default" px="xs" aria-label={`More actions for ${s.name}`}><IconDots size={16} /></Button>
              </Menu.Target>
              <Menu.Dropdown>
                {more}
                {more.length > 0 && lifecycle.length > 0 && <Menu.Divider />}
                {lifecycle}
              </Menu.Dropdown>
            </Menu>
          )}
        </Group>
      </Group>
      {onOverview && canAddItem && newItem.modal}
      <LaunchRunModal space={s} node={node} opened={open === 'launch'} onClose={() => setOpen(null)} />
      {open === 'edit' && <SpaceFormModal space={s} opened onClose={() => setOpen(null)} />}
      {open === 'relocate' && <RelocateModal space={s} opened onClose={() => setOpen(null)} />}
      {open === 'export' && <ExportModal spaceId={s.id} onClose={() => setOpen(null)} />}
      <SpaceLifecycleDialog space={s} dialog={dialog} onClose={() => setDialog(null)} onPurged={() => void navigate({ to: '/spaces' })} />
    </>
  )
}

/** A section's title row inside the space layout: the section name, a one-line purpose and its actions. */
export function SectionHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    // On a phone the actions move under the title (and wrap) instead of pushing the page sideways.
    <Group justify="space-between" align="flex-start" mb="sm" gap="md">
      <Stack gap={0} style={{ flex: '1 1 240px', minWidth: 0 }}>
        <Title order={4}>{title}</Title>
        {description && <Text size="sm" c="dimmed" lineClamp={1}>{description}</Text>}
      </Stack>
      {actions && <Group gap="xs">{actions}</Group>}
    </Group>
  )
}
