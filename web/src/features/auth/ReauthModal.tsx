import { Button, Divider, Modal, PasswordInput, Stack, Text, TextInput } from '@mantine/core'
import { IconLogin2 } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { api, unwrap } from '@/api/client'
import { keys, useMe } from '@/api/queries'
import { setReauthHandler, setSession, type ReauthCode } from '@/api/session'
import { errorMessage } from '@/components/ErrorAlert'
import { ssoStartUrl } from '@/lib/sso'

/**
 * Step-up prompt (SM-AUTH-005): sensitive operations need a recent password (and TOTP for MFA-bound ones). The original
 * request is retried once after a successful re-authentication. An SSO session steps up at its identity provider (a
 * fresh login there, Hallpass HP-OIDC-013), or with the SpaceMan TOTP code where the provider allows that fallback.
 */
export function ReauthModal() {
  const qc = useQueryClient()
  const me = useMe().data
  const sso = me?.method === 'oidc' && !!me.provider
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

  // The pending request is dropped: the browser leaves for the provider and comes back to this page.
  const stepUpAtProvider = (slug: string) => {
    finish(false)
    window.location.assign(ssoStartUrl(slug, 'stepup', `${window.location.pathname}${window.location.search}`))
  }

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
        const me = unwrap(
          sso
            ? await api.POST('/auth/reauth/totp', { body: { code: value.code } })
            : await api.POST('/auth/reauth', { body: { password: value.password, code: value.code || undefined } }),
        )
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
          {sso ? (
            <>
              <Text size="sm" c="dimmed">This action needs a fresh sign-in at your identity provider. You come back to this page; then repeat the action.</Text>
              <Button leftSection={<IconLogin2 size={16} />} onClick={() => stepUpAtProvider(me?.provider ?? '')}>Sign in again with your provider</Button>
              {me?.user?.mfa_enrolled && <Divider label="or, where the provider allows it" labelPosition="center" />}
            </>
          ) : (
            <>
              <Text size="sm" c="dimmed">
                {code === 'mfa_required' ? 'This action needs your password and an authenticator code.' : 'This action needs your password again.'}
              </Text>
              <form.Field name="password">
                {(f) => <PasswordInput label="Password" autoComplete="current-password" data-autofocus value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
              </form.Field>
            </>
          )}
          {(code === 'mfa_required' || (sso && me?.user?.mfa_enrolled)) && (
            <form.Field name="code">
              {(f) => <TextInput label={sso ? 'SpaceMan authenticator code' : 'Authenticator code'} inputMode="numeric" autoComplete="one-time-code" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          )}
          {error && <Text c="owlRed" size="sm">{error}</Text>}
          {(!sso || me?.user?.mfa_enrolled) && (
            <form.Subscribe selector={(s) => s.isSubmitting}>
              {(busy) => <Button type="submit" variant={sso ? 'default' : 'filled'} loading={busy}>{sso ? 'Confirm with the code' : 'Continue'}</Button>}
            </form.Subscribe>
          )}
        </Stack>
      </form>
    </Modal>
  )
}
