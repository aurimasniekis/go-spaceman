import { Button, Modal, PasswordInput, Stack, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { api, unwrap } from '@/api/client'
import { keys } from '@/api/queries'
import { setReauthHandler, setSession, type ReauthCode } from '@/api/session'
import { errorMessage } from '@/components/ErrorAlert'

/**
 * Step-up prompt (SM-AUTH-005): sensitive operations need a recent password (and TOTP for MFA-bound ones). The original
 * request is retried once after a successful re-authentication.
 */
export function ReauthModal() {
  const qc = useQueryClient()
  const [code, setCode] = useState<ReauthCode | null>(null)
  const [error, setError] = useState('')
  const resolve = useRef<((ok: boolean) => void) | null>(null)

  useEffect(
    () =>
      setReauthHandler(
        (c) =>
          new Promise<boolean>((res) => {
            resolve.current = res
            setError('')
            setCode(c)
          }),
      ),
    [],
  )

  const finish = (ok: boolean) => {
    resolve.current?.(ok)
    resolve.current = null
    setCode(null)
    form.reset()
  }

  const form = useForm({
    defaultValues: { password: '', code: '' },
    onSubmit: async ({ value }) => {
      try {
        const me = unwrap(await api.POST('/auth/reauth', { body: { password: value.password, code: value.code || undefined } }))
        setSession(me.csrf_token, me.limited)
        qc.setQueryData(keys.me, me)
        finish(true)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Modal opened={code !== null} onClose={() => finish(false)} title="Confirm it's you" size="sm">
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            {code === 'mfa_required' ? 'This action needs your password and an authenticator code.' : 'This action needs your password again.'}
          </Text>
          <form.Field name="password">
            {(f) => <PasswordInput label="Password" autoComplete="current-password" data-autofocus value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {code === 'mfa_required' && (
            <form.Field name="code">
              {(f) => <TextInput label="Authenticator code" inputMode="numeric" autoComplete="one-time-code" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          )}
          {error && <Text c="owlRed" size="sm">{error}</Text>}
          <form.Subscribe selector={(s) => s.isSubmitting}>
            {(busy) => <Button type="submit" loading={busy}>Continue</Button>}
          </form.Subscribe>
        </Stack>
      </form>
    </Modal>
  )
}
