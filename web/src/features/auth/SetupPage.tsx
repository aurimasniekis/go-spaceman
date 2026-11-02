import { Alert, Anchor, Button, Code, PasswordInput, Stack, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap } from '@/api/client'
import { keys, useSystem } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { AuthLayout } from './AuthLayout'

/**
 * First-run setup (SM-AUTH-010): the root token from `spaceman admin bootstrap` creates the first administrator. The
 * token is sent once as a bearer credential and is never stored by the browser.
 */
export function SetupPage() {
  const qc = useQueryClient()
  const system = useSystem()
  const [error, setError] = useState('')
  const [done, setDone] = useState(false)
  const [key] = useState(idempotencyKey)

  const form = useForm({
    defaultValues: { root: '', display_name: '', email: '', password: '', confirm: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(
          await api.POST('/users', {
            headers: { Authorization: `Bearer ${value.root.trim()}`, 'Idempotency-Key': key },
            body: { display_name: value.display_name, email: value.email, password: value.password, mfa_required: true, grants: [{ role: 'admin', domain: '*' }] },
          }),
        )
        setDone(true)
        void qc.invalidateQueries({ queryKey: keys.system })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  if (done) {
    return (
      <AuthLayout title="Administrator created">
        <Text size="sm">Sign in with the new account. You will set up an authenticator app first.</Text>
        <Text size="sm" c="dimmed">Keep the root token offline for recovery, or revoke it under Settings once you have a second administrator.</Text>
        <Button component={Link} to="/login">Sign in</Button>
      </AuthLayout>
    )
  }
  if (system.data?.has_admin) {
    return (
      <AuthLayout title="Already set up">
        <Text size="sm">An administrator exists. <Anchor component={Link} to="/login">Sign in</Anchor> instead.</Text>
      </AuthLayout>
    )
  }

  return (
    <AuthLayout title="Set up SpaceMan" subtitle="Create the first administrator." width={420}>
      {system.data && !system.data.bootstrapped && (
        <Alert color="owlYellow">No root token exists yet. Run <Code>spaceman admin bootstrap</Code> on the server first.</Alert>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="sm">
          <form.Field name="root" validators={{ onBlur: required('Root token'), onSubmit: ({ value }) => (!value.trim().startsWith('spaceman_rt_') ? 'Root tokens start with spaceman_rt_' : undefined) }}>
            {(f) => (
              <PasswordInput label="Root token" description="Printed once by `spaceman admin bootstrap`" autoComplete="off" classNames={{ input: "mono" }} value={f.state.value}
                onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError({ ...f.state.meta, isTouched: true })} />
            )}
          </form.Field>
          <form.Field name="display_name" validators={{ onBlur: required('Name') }}>
            {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="email" validators={{ onBlur: required('Email') }}>
            {(f) => <TextInput label="Email" type="email" autoComplete="username" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="password" validators={{ onBlur: ({ value }) => (value.length < 12 ? 'Use at least 12 characters' : undefined) }}>
            {(f) => <PasswordInput label="Password" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="confirm" validators={{ onChangeListenTo: ['password'], onBlur: ({ value, fieldApi }) => (value !== fieldApi.form.getFieldValue('password') ? 'Passwords do not match' : undefined) }}>
            {(f) => <PasswordInput label="Confirm password" autoComplete="new-password" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <form.Subscribe selector={(s) => s.isSubmitting}>
            {(busy) => <Button type="submit" loading={busy}>Create administrator</Button>}
          </form.Subscribe>
        </Stack>
      </form>
    </AuthLayout>
  )
}
