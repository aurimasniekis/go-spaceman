import { Alert, Button, Group, Menu, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconArchive, IconDots, IconRestore, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type Space } from '@/api/client'
import { keys, useMe, useRuns } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { atLeast, hasGlobal, roleIn } from '@/lib/access'
import { notifyOk } from '@/lib/notify'

export type Dialog = 'archive' | 'restore' | 'purge' | null

const ENDED = ['exited', 'failed', 'lost']

/** What the signed-in principal may do to a space's lifecycle. The API enforces the same (space:write; purge: global space:purge with step-up). */
export function useSpaceLifecycleAccess(space: Space | undefined) {
  const me = useMe().data
  const operator = !!space && atLeast(roleIn(me, space.id), 'operator')
  return {
    archive: operator && space?.state !== 'archived',
    restore: operator && space?.state === 'archived',
    purge: !!space && space.state === 'archived' && hasGlobal(me, 'admin'),
  }
}

/**
 * Archive, restore and purge a space (SM-SPACE-002), each behind a confirmation. Archiving keeps everything and needs
 * the space's runs stopped; restoring makes it active again; purging permanently deletes an archived space's records
 * (never upstream repositories) and needs the slug typed, plus a fresh sign-in with MFA (the API asks for it; the
 * step-up prompt appears and the request is retried).
 */
export function SpaceLifecycleMenu({ space, onPurged }: { space: Space; onPurged?: () => void }) {
  const can = useSpaceLifecycleAccess(space)
  const [dialog, setDialog] = useState<Dialog>(null)
  if (!can.archive && !can.restore && !can.purge) return null
  return (
    <>
      <Menu position="bottom-end">
        <Menu.Target>
          <Button variant="default" px="xs" aria-label={`More actions for ${space.name}`}><IconDots size={16} /></Button>
        </Menu.Target>
        <Menu.Dropdown>
          {can.archive && <Menu.Item leftSection={<IconArchive size={14} />} onClick={() => setDialog('archive')}>Archive…</Menu.Item>}
          {can.restore && <Menu.Item leftSection={<IconRestore size={14} />} onClick={() => setDialog('restore')}>Restore…</Menu.Item>}
          {can.purge && <Menu.Item color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => setDialog('purge')}>Purge…</Menu.Item>}
        </Menu.Dropdown>
      </Menu>
      <SpaceLifecycleDialog space={space} dialog={dialog} onClose={() => setDialog(null)} onPurged={onPurged} />
    </>
  )
}

/** The confirmation dialogs on their own (for pages that offer the actions as buttons). */
export function SpaceLifecycleDialog({ space, dialog, onClose, onPurged }: { space: Space; dialog: Dialog; onClose: () => void; onPurged?: () => void }) {
  if (dialog === 'purge') return <PurgeDialog space={space} onClose={onClose} onPurged={onPurged} />
  if (dialog === 'archive' || dialog === 'restore') return <StateDialog space={space} to={dialog} onClose={onClose} />
  return null
}

function StateDialog({ space, to, onClose }: { space: Space; to: 'archive' | 'restore'; onClose: () => void }) {
  const qc = useQueryClient()
  const runs = useRuns(space.id).data ?? []
  const active = runs.filter((r) => !ENDED.includes(r.observed)).length
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async () => {
    setError('')
    setBusy(true)
    try {
      const next = unwrap(await api.POST('/spaces/{space_id}/state', {
        params: { path: { space_id: space.id }, header: { 'If-Match': ifMatch(space.version) } }, body: { state: to === 'archive' ? 'archived' : 'active' },
      }))
      qc.setQueryData(keys.space(space.id), next)
      void qc.invalidateQueries({ queryKey: keys.spaces })
      notifyOk(to === 'archive' ? `${space.name} archived` : `${space.name} restored`)
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened onClose={onClose} title={to === 'archive' ? `Archive ${space.name}?` : `Restore ${space.name}?`}>
      <Stack gap="sm">
        {to === 'archive' ? (
          <>
            <Text size="sm">An archived space starts no work and keeps all its records. You can restore it, or purge it to delete its records for good.</Text>
            {active > 0 && (
              <Alert color="owlYellow" title={`${active} run${active > 1 ? 's are' : ' is'} still active`}>
                Archiving needs every run stopped. Pause the space with Interrupt or Emergency stop first.
              </Alert>
            )}
          </>
        ) : (
          <Text size="sm">The space becomes active again, and agents can be started in it.</Text>
        )}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void submit()} leftSection={to === 'archive' ? <IconArchive size={14} /> : <IconRestore size={14} />}>
            {to === 'archive' ? 'Archive' : 'Restore'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function PurgeDialog({ space, onClose, onPurged }: { space: Space; onClose: () => void; onPurged?: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { slug: '' },
    onSubmit: async () => {
      setError('')
      try {
        check(await api.DELETE('/spaces/{space_id}', { params: { path: { space_id: space.id } } }))
        qc.removeQueries({ queryKey: keys.space(space.id) })
        void qc.invalidateQueries({ queryKey: keys.spaces })
        notifyOk(`${space.name} purged`)
        onClose()
        if (onPurged) onPurged()
        else void navigate({ to: '/spaces' })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={`Purge ${space.name}?`}>
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Alert color="owlRed" title="This cannot be undone">
            Purging permanently deletes the space and the records SpaceMan keeps for it, including its runs, tasks, inbox, human actions,
            messages, attachments and grants. Upstream repositories are never touched, and the node keeps the space's folder.
          </Alert>
          <form.Field name="slug" validators={{ onChange: ({ value }) => (value === space.slug ? undefined : 'Type the slug exactly') }}>
            {(f) => (
              <TextInput label={<>Type <span className="mono">{space.slug}</span> to confirm</>} autoComplete="off" spellCheck={false} data-autofocus
                value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)}
                error={f.state.meta.isTouched && f.state.value !== '' && f.state.value !== space.slug ? 'Type the slug exactly' : undefined} />
            )}
          </form.Field>
          <Text size="xs" c="dimmed">Purging needs a recent sign-in with an authenticator code; SpaceMan asks for one if yours is too old.</Text>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => [s.isSubmitting, s.values.slug === space.slug] as const}>
              {([busy, ok]) => <Button type="submit" color="owlRed" loading={busy} disabled={!ok} leftSection={<IconTrash size={14} />}>Purge</Button>}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
