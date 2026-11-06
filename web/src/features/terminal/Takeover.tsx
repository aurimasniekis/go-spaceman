import { Alert, Button, Group, List, Modal, Stack, Text } from '@mantine/core'
import { IconHandStop, IconKeyboard } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, unwrap, type Me, type Run } from '@/api/client'
import { keys } from '@/api/queries'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

/** How often the holder's page renews its takeover (the server ends it 30 s after the last renewal). */
const RENEW_MS = 10_000
/** Keystrokes are sent in small batches, in order; a batch stays far below the server's 4096-byte limit. */
const FLUSH_MS = 25
const MAX_CHARS = 1000

/** The principal reference the server stores as a takeover's holder (user:<id>, or root). */
export function holderRef(me: Me | null | undefined): string {
  if (!me) return ''
  return me.root ? 'root' : me.user ? `user:${me.user.id}` : ''
}

type Path = { space_id: string; run_id: string }

/**
 * Releases scheduled by an unmount, by takeover ID. React's development double mount unmounts and mounts again at once;
 * the short delay lets the second mount cancel the release instead of ending the takeover it just started.
 */
const pendingRelease = new Map<string, ReturnType<typeof setTimeout>>()

/**
 * The holder's side of a terminal takeover (SM-UI-004): renews it while the page is open, sends typed keys in order,
 * and releases it when the page goes away. Returns the key handler for the terminal, or null when you do not hold it.
 */
export function useTakeoverInput(spaceId: string, runId: string, run: Run, me: Me | null | undefined): ((data: string) => void) | null {
  const qc = useQueryClient()
  const t = run.takeover
  const mine = !!t && t.holder === holderRef(me)
  const id = mine ? t.id : ''
  const queue = useRef('')
  const sending = useRef(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const flush = useCallback(async () => {
    timer.current = null
    if (sending.current || !id) return
    sending.current = true
    try {
      while (queue.current) {
        const data = queue.current.slice(0, MAX_CHARS)
        queue.current = queue.current.slice(MAX_CHARS)
        const res = await api.POST('/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/input', {
          params: { path: { space_id: spaceId, run_id: runId, takeover_id: id } },
          body: { data },
        })
        if (res.error) {
          queue.current = ''
          notifyError(res.error, 'Keys were not delivered')
          void qc.invalidateQueries({ queryKey: keys.run(spaceId, runId) })
        }
      }
    } finally {
      sending.current = false
    }
  }, [id, spaceId, runId, qc])

  useEffect(() => {
    if (!id) return
    const path: Path & { takeover_id: string } = { space_id: spaceId, run_id: runId, takeover_id: id }
    const scheduled = pendingRelease.get(id)
    if (scheduled) {
      clearTimeout(scheduled)
      pendingRelease.delete(id)
    }
    const renew = setInterval(() => {
      void api.POST('/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/renew', { params: { path } }).then((res) => {
        if (res.error) void qc.invalidateQueries({ queryKey: keys.run(spaceId, runId) })
      })
    }, RENEW_MS)
    return () => {
      clearInterval(renew)
      if (timer.current) clearTimeout(timer.current)
      queue.current = ''
      // Leaving the page releases the takeover; a closed tab lets it expire instead.
      pendingRelease.set(id, setTimeout(() => {
        pendingRelease.delete(id)
        void api.POST('/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/end', { params: { path } }).then(() =>
          qc.invalidateQueries({ queryKey: keys.run(spaceId, runId) }),
        )
      }, 300))
    }
  }, [id, spaceId, runId, qc])

  const onData = useCallback(
    (data: string) => {
      queue.current += data
      if (!timer.current) timer.current = setTimeout(() => void flush(), FLUSH_MS)
    },
    [flush],
  )
  return id ? onData : null
}

/**
 * Who holds the terminal, for everyone watching, with the controls the viewer may use: take over (terminal:takeover,
 * admins by default), release (the holder) or end someone else's takeover.
 */
export function TakeoverPanel({ spaceId, runId, run, me, canTakeOver, ended, part }: {
  spaceId: string
  runId: string
  run: Run
  me: Me | null | undefined
  canTakeOver: boolean
  ended: boolean
  /** button: only the "Take over" button (for a header row); banner: only the one-line takeover banner. Default both. */
  part?: 'button' | 'banner'
}) {
  const qc = useQueryClient()
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const t = run.takeover
  const mine = !!t && t.holder === holderRef(me)
  const path: Path = { space_id: spaceId, run_id: runId }
  const refresh = () => qc.invalidateQueries({ queryKey: keys.run(spaceId, runId) })

  const start = async () => {
    setBusy(true)
    try {
      unwrap(await api.POST('/spaces/{space_id}/runs/{run_id}/takeover', { params: { path } }))
      setConfirm(false) // the banner says it; no toast over the terminal (UX review)
    } catch (e) {
      notifyError(e, 'Could not take over the terminal')
    } finally {
      setBusy(false)
      void refresh()
    }
  }
  const end = async () => {
    if (!t) return
    setBusy(true)
    try {
      unwrap(await api.POST('/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/end', { params: { path: { ...path, takeover_id: t.id } } }))
      notifyOk(mine ? 'Takeover released; automated input resumes' : 'Takeover ended')
    } catch (e) {
      notifyError(e, 'Could not end the takeover')
    } finally {
      setBusy(false)
      void refresh()
    }
  }

  if (t) {
    if (part === 'button') return null
    return (
      <Alert color={mine ? 'owlYellow' : 'owlPurple'} variant="light" icon={<IconKeyboard size={18} />} role="status"
        title={mine ? 'You have taken over this terminal' : `${t.holder_label || t.holder} has taken over this terminal`}>
        <Group justify="space-between" align="flex-end" wrap="wrap" gap="xs">
          <Stack gap={2}>
            <Text size="sm">
              {mine
                ? 'Your keys go to the program. Click the terminal to type.'
                : 'Their keys go to the program; you are watching.'}{' '}
              Automated input is paused: Message the agent and the notices about messages and human responses wait until the takeover ends.
            </Text>
            <Text size="xs" c="dimmed">Since {datetime(t.started_at)}. It ends when released, when the page holding it closes, or when the run ends.</Text>
          </Stack>
          {(mine || canTakeOver) && (
            <Button size="xs" color={mine ? undefined : 'owlRed'} variant={mine ? 'default' : 'light'} loading={busy} onClick={() => void end()}>
              {mine ? 'Release' : 'End takeover'}
            </Button>
          )}
        </Group>
      </Alert>
    )
  }
  if (!canTakeOver || ended || run.desired !== 'running' || part === 'banner') return null
  const button = <Button size="xs" variant="default" leftSection={<IconHandStop size={14} />} onClick={() => setConfirm(true)}>Take over</Button>
  return (
    <>
      {part === 'button' ? button : <Group justify="flex-end">{button}</Group>}
      <Modal opened={confirm} onClose={() => setConfirm(false)} title="Take over this terminal?">
        <Stack gap="sm">
          <Text size="sm">You will type directly into the agent&apos;s program. While you hold the terminal:</Text>
          <List size="sm" spacing={4}>
            <List.Item>everyone watching sees that you hold it;</List.Item>
            <List.Item>Message the agent and SpaceMan&apos;s notices about messages and human responses are held back until you release it;</List.Item>
            <List.Item>the start and end are recorded in the audit log (your keystrokes are not stored).</List.Item>
          </List>
          <Text size="xs" c="dimmed">It ends when you release it, leave this page, or the run ends.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirm(false)}>Cancel</Button>
            <Button color="owlYellow" loading={busy} onClick={() => void start()}>Take over</Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}
