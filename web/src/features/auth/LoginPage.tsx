import { Alert, Anchor, Button, Code, CopyButton, Divider, Group, PasswordInput, SimpleGrid, Stack, Text, TextInput } from '@mantine/core'
import { IconLogin2 } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { api, unwrap, type Me } from '@/api/client'
import { keys, useMe, useSignInProviders, useSystem } from '@/api/queries'
import { safeReturnTo, setSession } from '@/api/session'
import { errorMessage } from '@/components/ErrorAlert'
import { QRCode } from '@/components/QRCode'
import { fieldError, required } from '@/lib/form'
import { signOut } from '@/lib/signout'
import { clearSignedOut, markSignedOut, pendingProviderLogout, ssoErrorMessage, ssoStartUrl, wasSignedOut } from '@/lib/sso'
import { AuthLayout } from './AuthLayout'

type Step = 'password' | 'enroll' | 'codes' | 'verify'

function stepFor(me: Me | null | undefined): Step | 'done' {
  if (!me) return 'password'
  if (!me.limited) return 'done'
  return me.needs_enroll ? 'enroll' : 'verify'
}

/**
 * Sign-in (SM-AUTH-001/002): a button per enabled OIDC provider (Porch or any other), or password then TOTP. Accounts
 * that must use MFA and have none enroll here; enrollment never completes a sign-in by itself, so a fresh code is asked
 * for afterwards (Hallpass SR-03).
 *
 * Ported from Hallpass (ADR-0045): with a direct provider the page continues to it at once (`/login?password=1` always
 * shows every sign-in method); under SSO-only enforcement the password form is not offered; after a sign-out the page
 * stays put and offers the provider's RP-initiated logout when the server returned one.
 */
export function LoginPage({ returnTo, ssoError, password }: { returnTo?: string; ssoError?: string; password?: boolean }) {
  const qc = useQueryClient()
  const me = useMe()
  const system = useSystem()
  const providers = useSignInProviders().data ?? []
  const [step, setStep] = useState<Step>('password')
  const [recovery, setRecovery] = useState<string[]>([])
  const [error, setError] = useState('')
  const [signedOut] = useState(wasSignedOut)
  const [providerLogout, setProviderLogout] = useState(pendingProviderLogout)
  const ssoOnly = system.data?.local_login_disabled ?? false
  const direct = providers.find((p) => p.slug === system.data?.direct_provider)
  // Continue to the direct provider only on a plain /login visit by someone who is not signed in and did not just sign out.
  const continueDirect = direct !== undefined && !password && !ssoError && !signedOut && me.isFetched && !me.data && step === 'password'
  const start = (slug: string) => {
    clearSignedOut()
    window.location.assign(ssoStartUrl(slug, 'login', safeReturnTo(returnTo)))
  }

  useEffect(() => {
    if (continueDirect) start(direct.slug)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [continueDirect, direct?.slug])

  const signedIn = (m: Me) => {
    clearSignedOut()
    setSession(m.csrf_token, m.limited)
    qc.setQueryData(keys.me, m)
    const next = stepFor(m)
    if (next === 'done') window.location.assign(safeReturnTo(returnTo))
    else setStep(next)
  }

  // A session that is already limited (reload during MFA) resumes at its step.
  useEffect(() => {
    if (me.data && step === 'password') {
      const s = stepFor(me.data)
      if (s === 'done') window.location.assign(safeReturnTo(returnTo))
      else setStep(s)
    }
  }, [me.data, step, returnTo])

  const run = async (fn: () => Promise<void>) => {
    setError('')
    try {
      await fn()
    } catch (e) {
      setError(errorMessage(e))
    }
  }

  const pw = useForm({
    defaultValues: { email: '', password: '' },
    onSubmit: ({ value }) => run(async () => signedIn(unwrap(await api.POST('/auth/login', { body: value })))),
  })

  if (system.data && !system.data.has_admin) {
    return (
      <AuthLayout title="SpaceMan is not set up yet">
        <Text size="sm">No administrator exists. Create the first one with the root token printed by <Code>spaceman admin bootstrap</Code>.</Text>
        <Button component={Link} to="/setup">Set up SpaceMan</Button>
      </AuthLayout>
    )
  }

  if (step === 'enroll') return <EnrollStep onDone={(codes) => { setRecovery(codes); setStep('codes') }} />
  if (step === 'codes') {
    return (
      <AuthLayout title="Save your recovery codes" subtitle="Each code signs you in once if you lose your authenticator. They are shown only now.">
        <SimpleGrid cols={2} spacing={4}>
          {recovery.map((c) => <Code key={c} className="mono">{c}</Code>)}
        </SimpleGrid>
        <CopyButton value={recovery.join('\n')}>
          {({ copied, copy }) => <Button variant="light" onClick={copy}>{copied ? 'Copied' : 'Copy all'}</Button>}
        </CopyButton>
        <Button onClick={() => setStep('verify')}>I saved them — continue</Button>
      </AuthLayout>
    )
  }
  if (step === 'verify') {
    return (
      <AuthLayout title="Two-factor verification" subtitle="Enter the current code from your authenticator app, or a recovery code.">
        <CodeForm label="Code" submit="Verify" error={error} onSubmit={(code) => run(async () => signedIn(unwrap(await api.POST('/auth/mfa/verify', { body: { code } }))))} />
        <Anchor size="xs" onClick={() => void signOut()}>Use a different account</Anchor>
      </AuthLayout>
    )
  }

  if (continueDirect) {
    return (
      <AuthLayout title="Sign in">
        <Text size="sm">Continuing to {direct.display_name}…</Text>
        <Button variant="default" leftSection={<IconLogin2 size={16} />} onClick={() => start(direct.slug)}>Sign in with {direct.display_name}</Button>
        {!ssoOnly && <Anchor href="/login?password=1" size="xs">Use your password instead</Anchor>}
      </AuthLayout>
    )
  }

  return (
    <AuthLayout title="Sign in">
      {ssoError && <Alert color="owlRed" title="Single sign-on failed" role="alert">{ssoErrorMessage(ssoError)}</Alert>}
      {signedOut && !ssoError && <Alert color="owlBlue" variant="light">You are signed out.</Alert>}
      {providerLogout && (
        <Alert color="owlYellow" variant="light" title="Still signed in at your identity provider">
          <Stack gap="xs">
            <Text size="sm">Signing out of SpaceMan does not sign you out of the provider you signed in with.</Text>
            <Group gap="xs">
              <Button size="xs" component="a" href={providerLogout} onClick={() => markSignedOut(null)}>Sign out at the provider</Button>
              <Button size="xs" variant="default" onClick={() => { markSignedOut(null); setProviderLogout(null) }}>Not now</Button>
            </Group>
          </Stack>
        </Alert>
      )}
      {providers.length > 0 && (
        <Stack gap="xs">
          {providers.map((p) => (
            <Button key={p.slug} variant="default" leftSection={<IconLogin2 size={16} />} component="a" href={ssoStartUrl(p.slug, 'login', safeReturnTo(returnTo))}
              onClick={clearSignedOut}>
              Sign in with {p.display_name}
            </Button>
          ))}
          {!ssoOnly && <Divider label="or with your password" labelPosition="center" />}
        </Stack>
      )}
      {ssoOnly && <Alert color="owlYellow">Password sign-in is turned off on this server: sign in with single sign-on.</Alert>}
      {!ssoOnly && <form
        onSubmit={(e) => {
          e.preventDefault()
          void pw.handleSubmit()
        }}
      >
        <Stack gap="sm">
          <pw.Field name="email" validators={{ onBlur: required('Email') }}>
            {(f) => (
              <TextInput label="Email" type="email" autoComplete="username" autoFocus value={f.state.value} onBlur={f.handleBlur}
                onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
            )}
          </pw.Field>
          <pw.Field name="password" validators={{ onBlur: required('Password') }}>
            {(f) => (
              <PasswordInput label="Password" autoComplete="current-password" value={f.state.value} onBlur={f.handleBlur}
                onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
            )}
          </pw.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <pw.Subscribe selector={(s) => s.isSubmitting}>
            {(busy) => <Button type="submit" loading={busy}>Sign in</Button>}
          </pw.Subscribe>
        </Stack>
      </form>}
      <Group justify={ssoOnly ? 'flex-end' : 'space-between'}>
        {!ssoOnly && <Anchor component={Link} to="/reset" size="xs">I have a password reset token</Anchor>}
        <Anchor component={Link} to="/recover" size="xs">Recover access</Anchor>
      </Group>
    </AuthLayout>
  )
}

function CodeForm({ label, submit, error, onSubmit }: { label: string; submit: string; error: string; onSubmit: (code: string) => Promise<void> }) {
  const form = useForm({ defaultValues: { code: '' }, onSubmit: ({ value }) => onSubmit(value.code.trim()) })
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        void form.handleSubmit()
      }}
    >
      <Stack gap="sm">
        <form.Field name="code" validators={{ onSubmit: required(label) }}>
          {(f) => (
            <TextInput label={label} inputMode="numeric" autoComplete="one-time-code" autoFocus classNames={{ input: "mono" }} value={f.state.value}
              onChange={(e) => f.handleChange(e.target.value)} error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined} />
          )}
        </form.Field>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <form.Subscribe selector={(s) => s.isSubmitting}>
          {(busy) => <Button type="submit" loading={busy}>{submit}</Button>}
        </form.Subscribe>
      </Stack>
    </form>
  )
}

function EnrollStep({ onDone }: { onDone: (codes: string[]) => void }) {
  const qc = useQueryClient()
  const [enrollment, setEnrollment] = useState<{ secret: string; otpauth_uri: string } | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    api.POST('/auth/mfa/enroll').then((r) => setEnrollment(unwrap(r))).catch((e: unknown) => setError(errorMessage(e)))
  }, [])
  return (
    <AuthLayout title="Set up two-factor authentication" subtitle="Your grants require an authenticator app. Scan the code, then enter the 6-digit code it shows." width={420}>
      {enrollment && (
        <Group align="flex-start" wrap="nowrap">
          <QRCode value={enrollment.otpauth_uri} label="Authenticator QR code" size={150} />
          <Stack gap={4}>
            <Text size="xs" c="dimmed">Or enter this key manually:</Text>
            <Code className="mono" style={{ wordBreak: 'break-all' }}>{enrollment.secret}</Code>
          </Stack>
        </Group>
      )}
      <CodeForm
        label="Code"
        submit="Enable"
        error={error}
        onSubmit={async (code) => {
          setError('')
          try {
            const res = unwrap(await api.POST('/auth/mfa/enroll/confirm', { body: { code } }))
            setSession(res.me.csrf_token, res.me.limited)
            qc.setQueryData(keys.me, res.me)
            onDone(res.recovery_codes)
          } catch (e) {
            setError(errorMessage(e))
          }
        }}
      />
    </AuthLayout>
  )
}
