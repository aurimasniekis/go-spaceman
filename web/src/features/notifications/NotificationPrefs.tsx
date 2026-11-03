import { Alert, Button, Card, Group, MultiSelect, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { IconBellRinging } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api, ifMatch, unwrap, type Schemas } from '@/api/client'
import { keys, useNotificationPrefs, useSpaces } from '@/api/queries'
import { ErrorAlert, errorMessage } from '@/components/ErrorAlert'
import { useAppForm } from '@/components/form/appForm'
import { notifyOk } from '@/lib/notify'

export type NotificationPrefs = Schemas['NotificationPrefs']

const desktopSupported = () => typeof window !== 'undefined' && 'Notification' in window

/**
 * Your notification preferences (SM-HUI-006): which human-action changes this web UI announces to you while it is open,
 * and whether it also shows desktop notifications. They are stored per user on the server; SpaceMan sends no e-mail
 * or chat notifications.
 */
export function NotificationPrefsCard() {
  const prefs = useNotificationPrefs()
  const spaces = useSpaces()
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [permission, setPermission] = useState(desktopSupported() ? Notification.permission : 'unsupported')
  const form = useAppForm({
    defaultValues: {
      enabled: true, desktop: false, new_actions: true, assigned_to_me: true, escalations: true, reminders: false, clarifications: true,
      min_priority: 'low', muted_spaces: [] as string[],
    } as NotificationPrefs,
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const next = unwrap(await api.PUT('/auth/me/notification-preferences', {
          params: { header: { 'If-Match': ifMatch(prefs.data?.version ?? 0) } }, body: value }))
        qc.setQueryData(keys.notificationPrefs, next)
        notifyOk('Notification preferences saved')
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  useEffect(() => {
    if (!prefs.data) return
    const d = prefs.data
    form.reset({ enabled: d.enabled, desktop: d.desktop, new_actions: d.new_actions, assigned_to_me: d.assigned_to_me, escalations: d.escalations,
      reminders: d.reminders, clarifications: d.clarifications, min_priority: d.min_priority, muted_spaces: d.muted_spaces ?? [] })
  }, [prefs.data?.version]) // the server copy replaces the form when it changes
  const ask = async () => setPermission(await Notification.requestPermission())
  return (
    <div>
      <Title order={5} mb="xs">Notifications</Title>
      <ErrorAlert error={prefs.error} />
      <Card maw={720}>
        <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
          <Stack gap="sm">
            <Text size="sm" c="dimmed">What this web UI announces to you while it is open. SpaceMan sends no e-mail or chat messages; outbound events can feed those.</Text>
            <form.AppField name="enabled">{(f) => <f.CheckboxField label="Announce human-action changes" />}</form.AppField>
            <form.Subscribe selector={(s) => s.values.enabled}>
              {(on) => (
                <Stack gap="sm" style={{ opacity: on ? 1 : 0.6 }}>
                  <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="xs">
                    <form.AppField name="new_actions">{(f) => <f.CheckboxField label="New actions in my spaces" disabled={!on} />}</form.AppField>
                    <form.AppField name="assigned_to_me">{(f) => <f.CheckboxField label="Actions assigned to me" disabled={!on} />}</form.AppField>
                    <form.AppField name="escalations">{(f) => <f.CheckboxField label="Escalations" disabled={!on} />}</form.AppField>
                    <form.AppField name="reminders">{(f) => <f.CheckboxField label="Reminders" disabled={!on} />}</form.AppField>
                    <form.AppField name="clarifications">{(f) => <f.CheckboxField label="Clarification requests and replies" disabled={!on} />}</form.AppField>
                    <form.AppField name="desktop">{(f) => <f.CheckboxField label="Also as desktop notifications" disabled={!on} />}</form.AppField>
                  </SimpleGrid>
                  <Group grow align="flex-start">
                    <form.AppField name="min_priority">
                      {(f) => <f.SelectField label="Lowest priority" description="Assignments to you are always announced" allowDeselect={false} disabled={!on}
                        data={[{ value: 'low', label: 'Low' }, { value: 'normal', label: 'Normal' }, { value: 'high', label: 'High' }, { value: 'urgent', label: 'Urgent' }]} />}
                    </form.AppField>
                    <form.Field name="muted_spaces">
                      {(f) => <MultiSelect label="Muted spaces" description="Never announced" searchable clearable disabled={!on} value={f.state.value}
                        onChange={(v) => f.handleChange(v)} data={(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))} />}
                    </form.Field>
                  </Group>
                </Stack>
              )}
            </form.Subscribe>
            <form.Subscribe selector={(s) => s.values.desktop && s.values.enabled}>
              {(desktop) => desktop && permission !== 'granted' ? (
                <Alert color="owlYellow" variant="light" icon={<IconBellRinging size={16} />}>
                  {permission === 'unsupported' ? 'This browser has no desktop notifications.'
                    : permission === 'denied' ? 'This browser blocks desktop notifications for SpaceMan; allow them in its site settings.'
                      : <Group justify="space-between">This browser has not allowed desktop notifications yet.<Button size="xs" variant="light" onClick={() => void ask()}>Allow</Button></Group>}
                </Alert>
              ) : null}
            </form.Subscribe>
            {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
            <Group justify="flex-end"><form.AppForm><form.SubmitButton>Save</form.SubmitButton></form.AppForm></Group>
          </Stack>
        </form>
      </Card>
    </div>
  )
}
