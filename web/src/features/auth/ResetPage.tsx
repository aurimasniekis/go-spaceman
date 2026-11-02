import { Alert, Anchor, Button, PasswordInput, Stack, Text } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check } from '@/api/client'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { AuthLayout } from './AuthLayout'

/** The new-password fields shared by the reset and recovery pages. */
export function NewPasswordForm({ token, submit, onDone }: { token?: string; submit: string; onDone: () => void }) {
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { token: token ?? '', password: '', confirm: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        check(await api.POST('/auth/reset', { body: { token: value.token.trim(), password: value.password } }))
        onDone()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
      <Stack gap="sm">
        {!token && (
          <form.Field name="token" validators={{ onBlur: required('Reset token') }}>
            {(f) => <PasswordInput label="Reset token" description="From your administrator" autoComplete="off" classNames={{ input: 'mono' }}
              value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
        )}
        <form.Field name="password" validators={{ onBlur: ({ value }) => (value.length < 12 ? 'Use at least 12 characters' : undefined) }}>
          {(f) => <PasswordInput label="New password" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
        </form.Field>
        <form.Field name="confirm" validators={{ onChangeListenTo: ['password'], onBlur: ({ value, fieldApi }) => (value !== fieldApi.form.getFieldValue('password') ? 'Passwords do not match' : undefined) }}>
          {(f) => <PasswordInput label="Confirm password" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
        </form.Field>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>{submit}</Button>}</form.Subscribe>
      </Stack>
    </form>
  )
}

/** Sets a new password with an administrator-issued reset token (single use; every session of the account ends). */
export function ResetPage({ token }: { token?: string }) {
  const [done, setDone] = useState(false)
  if (done) {
    return (
      <AuthLayout title="Password changed">
        <Alert color="owlGreen">Your password was changed and every session of your account was signed out.</Alert>
        <Button component={Link} to="/login">Sign in</Button>
      </AuthLayout>
    )
  }
  return (
    <AuthLayout title="Set a new password" subtitle="Reset tokens work once and expire.">
      <NewPasswordForm token={token} submit="Set password" onDone={() => setDone(true)} />
      <Anchor component={Link} to="/login" size="xs">Back to sign-in</Anchor>
    </AuthLayout>
  )
}
