import { Alert, Button, Card, Group, List, Select, Stack, Switch, Text, Title } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type OidcProvider, type SignInSettings } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { notifyOk } from '@/lib/notify'

export const signInSettingsKey = ['oidc-sign-in-settings'] as const

/**
 * Installation-wide sign-in settings (ported from Hallpass, ADR-0045): SSO-only enforcement, which turns password sign-in
 * off once a provider passed a test login and an administrator can sign in with SSO, and the direct provider the
 * sign-in page continues to.
 */
export function SignInSettingsCard({ providers }: { providers: OidcProvider[] }) {
  const settings = useQuery({ queryKey: signInSettingsKey, queryFn: async ({ signal }) => unwrap(await api.GET('/oidc/settings', { signal })) })
  return (
    <Card>
      <Stack gap="sm">
        <Title order={5}>Sign-in settings</Title>
        <ErrorAlert error={settings.error} />
        {settings.data && <SettingsForm key={JSON.stringify(settings.data)} current={settings.data} providers={providers} />}
      </Stack>
    </Card>
  )
}

function SettingsForm({ current, providers }: { current: SignInSettings; providers: OidcProvider[] }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { local_login_disabled: current.local_login_disabled, direct_provider: current.direct_provider },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const next = unwrap(await api.PATCH('/oidc/settings', { body: value }))
        qc.setQueryData(signInSettingsKey, next)
        void qc.invalidateQueries({ queryKey: keys.system })
        notifyOk('Sign-in settings saved')
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const enabled = providers.filter((p) => p.enabled)
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        void form.handleSubmit()
      }}
    >
      <Stack gap="sm">
        <form.Field name="local_login_disabled">
          {(f) => (
            <Switch label="Sign in with SSO only" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)}
              description="Password sign-in is refused. Break-glass recovery (spaceman admin recover) still works." />
          )}
        </form.Field>
        {!current.sso_only_ready && !current.local_login_disabled && (
          <Alert variant="light" color="owlYellow" title="Not ready for SSO only">
            <List size="sm">
              <List.Item>{current.tested_providers > 0 ? '✓' : '✗'} An enabled provider with a successful test login ({current.tested_providers})</List.Item>
              <List.Item>{current.sso_admins > 0 ? '✓' : '✗'} An enabled global administrator with a linked identity on an enabled provider ({current.sso_admins})</List.Item>
            </List>
          </Alert>
        )}
        <form.Field name="direct_provider">
          {(f) => (
            <Select label="Direct sign-in" description="The sign-in page continues straight to this provider; /login?password=1 still shows every method."
              placeholder="None: offer every sign-in method" clearable value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')}
              data={enabled.map((p) => ({ value: p.slug, label: p.display_name }))} />
          )}
        </form.Field>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <form.Subscribe selector={(s) => [s.isDirty, s.isSubmitting] as const}>
            {([dirty, busy]) => <Button type="submit" disabled={!dirty} loading={busy}>Save</Button>}
          </form.Subscribe>
        </Group>
      </Stack>
    </form>
  )
}
