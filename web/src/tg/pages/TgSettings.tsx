import { Alert, Badge, Button, Card, Checkbox, Group, NativeSelect, Stack, Switch, Text, TextInput } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { ApiError, api, ifMatch, unwrap, type Schemas } from '@/api/client'
import { useMe, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { datetime } from '@/lib/format'
import { notifyError } from '@/lib/notify'
import { useQuery } from '@tanstack/react-query'

type Prefs = Schemas['LinkPrefs']
type Effective = Prefs['effective']
type Channel = 'push' | 'digest' | 'off'

const PREFS_KEY = ['tg', 'link-prefs']
const CHANNELS = [{ value: 'push', label: 'Push' }, { value: 'digest', label: 'Digest' }, { value: 'off', label: 'Off' }]
const CHANNEL_FIELDS = [
  ['inbox_items', 'New inbox items'], ['failed_runs', 'Failed or lost runs'], ['tasks_in_review', 'Tasks entering review'], ['reminders', 'Reminders of pending actions'],
] as const
const FLAG_FIELDS = [
  ['new_actions', 'New human actions'], ['assigned_to_me', 'Actions assigned to me'], ['clarifications', 'Clarification requests and replies'],
  ['escalations', 'Escalations'], ['operator_messages', 'Agent messages to operators'], ['digest', 'Daily digest'],
] as const

interface Form { eff: Effective; timezone: string }

/** The overrides to save: only what differs from the defaults, so everything else keeps following them. */
export function overridesFrom(p: Prefs, form: Form): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  const d = p.defaults as unknown as Record<string, unknown>
  for (const [k, v] of Object.entries(form.eff)) {
    if (k === 'muted_spaces') continue
    if (v !== d[k]) out[k] = v
  }
  if (form.eff.muted_spaces.length > 0) out.muted_spaces = form.eff.muted_spaces
  return out
}

/**
 * Your Telegram settings in the Mini App: who this session acts as, and this link's own notification preferences, which you
 * can edit here (GET and PUT /telegram/link/prefs, TG-10-4). Saving always sends If-Match with the version that was read: a
 * stale save (412) reloads the form and says so, and a save without it (428) cannot happen, because Save stays off until the
 * preferences have loaded. Linking, unlinking and the global Telegram settings stay in the web (ADR-0083).
 */
export function TgSettings() {
  const me = useMe().data
  const grants = me?.user?.grants ?? []
  return (
    <Stack gap="md">
      <AnchorLink to="/" size="xs">‹ Overview</AnchorLink>
      <PageHeader title="Telegram settings" description="This Telegram link and what it can do" />
      <Card>
        <Stack gap={6}>
          <Group justify="space-between"><Text size="sm" c="dimmed">Signed in as</Text><Text size="sm">{me?.user?.display_name ?? '—'}</Text></Group>
          <Group justify="space-between"><Text size="sm" c="dimmed">Session ends</Text><Text size="sm">{me?.expires_at ? datetime(me.expires_at) : '—'}</Text></Group>
        </Stack>
      </Card>
      <Stack gap="xs">
        <Text fw={600}>Your access</Text>
        {grants.length === 0 && <Text size="sm" c="dimmed">No grants.</Text>}
        {grants.map((g) => (
          <Group key={`${g.domain}:${g.role}`} justify="space-between" wrap="nowrap">
            <Text size="sm" truncate>{g.domain === '*' ? 'All spaces' : g.domain}</Text>
            <Badge variant="light">{g.role}</Badge>
          </Group>
        ))}
        <Text size="xs" c="dimmed">A link may be capped below your role. The server decides what each action may do.</Text>
      </Stack>
      <PrefsForm />
      <Alert variant="light" title="Linking">
        Link or unlink Telegram accounts under Account → Telegram in the SpaceMan web app.
      </Alert>
    </Stack>
  )
}

function PrefsForm() {
  const qc = useQueryClient()
  const prefs = useQuery({ queryKey: PREFS_KEY, queryFn: async ({ signal }) => unwrap(await api.GET('/telegram/link/prefs', { signal })) })
  const spaces = useSpaces().data ?? []
  const [form, setForm] = useState<Form | null>(null)
  const [saving, setSaving] = useState(false)
  const [conflict, setConflict] = useState(false)
  const data = prefs.data
  // Seed the form when the preferences load or reload (after a save or a conflict); edits in between stay.
  useEffect(() => {
    if (data) setForm({ eff: { ...data.effective, muted_spaces: [...data.effective.muted_spaces] }, timezone: data.timezone })
  }, [data])

  const set = <K extends keyof Effective>(k: K, v: Effective[K]) => setForm((f) => (f ? { ...f, eff: { ...f.eff, [k]: v } } : f))

  const save = async () => {
    if (!data || !form) return // no version read yet: never save without If-Match
    setSaving(true)
    setConflict(false)
    try {
      const body = { ...overridesFrom(data, form), timezone: form.timezone.trim() } as Schemas['LinkPrefsInput']
      const saved = unwrap(await api.PUT('/telegram/link/prefs', { params: { header: { 'If-Match': ifMatch(data.version) } }, body }))
      qc.setQueryData(PREFS_KEY, saved)
      notifications.show({ color: 'green', title: 'Saved', message: 'Your Telegram notification settings were saved.' })
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        setConflict(true)
        await qc.invalidateQueries({ queryKey: PREFS_KEY })
      } else notifyError(e, 'Could not save')
    } finally {
      setSaving(false)
    }
  }

  if (prefs.isError && !data) return <ErrorAlert error={prefs.error} />
  if (!data || !form) return <Text size="sm" c="dimmed">Loading notification settings…</Text>
  const muted = new Set(form.eff.muted_spaces)
  const toggleMuted = (id: string, on: boolean) => {
    const next = new Set(muted)
    if (on) next.add(id)
    else next.delete(id)
    set('muted_spaces', [...next])
  }
  const known = new Set(spaces.map((s) => s.id))
  return (
    <Stack gap="sm" component="form" onSubmit={(e: React.FormEvent) => { e.preventDefault(); void save() }} aria-label="Notification settings">
      <Text fw={600}>Notifications for this Telegram link</Text>
      {conflict && (
        <Alert color="yellow" variant="light" title="Changed elsewhere" role="alert">
          These settings changed since you opened them, so the form was reloaded with the current values. Review them and save again.
        </Alert>
      )}
      <ErrorAlert error={prefs.error} />
      <Card><Stack gap="xs">
        {CHANNEL_FIELDS.map(([k, label]) => (
          <NativeSelect key={k} label={label} data={CHANNELS} value={form.eff[k]} onChange={(e) => set(k, e.currentTarget.value as Channel)} />
        ))}
        <NativeSelect label="Lowest priority of new actions" data={['low', 'normal', 'high', 'urgent']} value={form.eff.min_priority}
          onChange={(e) => set('min_priority', e.currentTarget.value as Effective['min_priority'])} />
        {FLAG_FIELDS.map(([k, label]) => (
          <Switch key={k} label={label} checked={form.eff[k]} onChange={(e) => set(k, e.currentTarget.checked)} />
        ))}
        <TextInput label="Digest time" type="time" value={form.eff.digest_time} onChange={(e) => set('digest_time', e.currentTarget.value)} />
      </Stack></Card>
      <Card><Stack gap="xs">
        <Switch label="Quiet hours (send silently)" checked={form.eff.quiet_hours} onChange={(e) => set('quiet_hours', e.currentTarget.checked)} />
        <Group grow gap="xs" align="flex-start">
          <TextInput label="From" type="time" value={form.eff.quiet_hours_start} onChange={(e) => set('quiet_hours_start', e.currentTarget.value)} />
          <TextInput label="Until" type="time" value={form.eff.quiet_hours_end} onChange={(e) => set('quiet_hours_end', e.currentTarget.value)} />
        </Group>
        <TextInput label="Time zone" description="Per user, not per link: it applies to all your links and the web. Empty turns the digest and quiet hours off."
          placeholder="Europe/Vilnius" value={form.timezone} onChange={(e) => setForm({ ...form, timezone: e.currentTarget.value })} />
      </Stack></Card>
      <Card><Stack gap="xs">
        <Text size="sm" fw={600} id="muted-spaces">Muted spaces</Text>
        <Text size="xs" c="dimmed">Nothing from a muted space is sent to this link.</Text>
        {spaces.length === 0 && <Text size="sm" c="dimmed">No spaces.</Text>}
        <Stack gap={6} role="group" aria-labelledby="muted-spaces">
          {spaces.map((s) => <Checkbox key={s.id} label={s.name} checked={muted.has(s.id)} onChange={(e) => toggleMuted(s.id, e.currentTarget.checked)} />)}
          {form.eff.muted_spaces.filter((id) => !known.has(id)).map((id) => <Checkbox key={id} label={id} checked onChange={(e) => toggleMuted(id, e.currentTarget.checked)} />)}
        </Stack>
      </Stack></Card>
      <Group justify="space-between" wrap="nowrap">
        <Text size="sm">Topics mode</Text>
        <Text size="sm" c="dimmed">{data.topics.available ? 'Available (set in the chat)' : 'Not available yet'}</Text>
      </Group>
      <Button type="submit" loading={saving} disabled={saving}>Save</Button>
    </Stack>
  )
}
