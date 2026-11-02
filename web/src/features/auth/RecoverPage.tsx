import { Alert, Anchor, Button, PasswordInput, Stack, Text } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Schemas } from '@/api/client'
import { keys } from '@/api/queries'
import { setSession } from '@/api/session'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { AuthLayout } from './AuthLayout'
import { NewPasswordForm } from './ResetPage'

type Recovered = Schemas['RecoveryOut']

/**
 * Break-glass recovery (Hallpass HP-AUTH-016): a token from `spaceman admin recover` on the server signs the account in.
 * The account's other sessions, authenticator and recovery codes are removed, so the next steps are a new password (when
 * the account has one) and a new authenticator at the next sign-in. Administrators see an alert until they acknowledge it.
 */
export function RecoverPage() {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [res, setRes] = useState<Recovered | null>(null)
  const [passwordSet, setPasswordSet] = useState(false)
  const form = useForm({
    defaultValues: { token: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const out = unwrap(await api.POST('/auth/recover', { body: { token: value.token.trim() } }))
        setSession(out.me.csrf_token, out.me.limited)
        qc.setQueryData(keys.me, out.me)
        setRes(out)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  if (res && res.password_reset && !passwordSet) {
    return (
      <AuthLayout title="Choose a new password" subtitle={`Recovering ${res.me.user?.email ?? res.me.user?.display_name ?? 'your account'}.`}>
        {res.mfa_reset && <Alert color="owlYellow">Your authenticator and recovery codes were removed. You will set up a new authenticator when you sign in.</Alert>}
        <NewPasswordForm token={res.password_reset.token} submit="Set password and continue" onDone={() => setPasswordSet(true)} />
      </AuthLayout>
    )
  }
  if (res) {
    return (
      <AuthLayout title="Account recovered">
        <Text size="sm">{passwordSet ? 'Your password was set. Sign in with it to continue.' : 'Continue to sign-in to finish setting up your account.'}</Text>
        <Button component={Link} to="/login">Sign in</Button>
      </AuthLayout>
    )
  }
  return (
    <AuthLayout title="Recover access" subtitle="Use a recovery token issued on the server with `spaceman admin recover`.">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="token" validators={{ onBlur: required('Recovery token') }}>
            {(f) => <PasswordInput label="Recovery token" autoComplete="off" classNames={{ input: 'mono' }} value={f.state.value}
              onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Recover</Button>}</form.Subscribe>
        </Stack>
      </form>
      <Text size="xs" c="dimmed">This signs out the account everywhere and removes its authenticator. The use is audited, and administrators see an alert.</Text>
      <Anchor component={Link} to="/login" size="xs">Back to sign-in</Anchor>
    </AuthLayout>
  )
}
