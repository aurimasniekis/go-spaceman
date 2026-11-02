import { Alert, Anchor, Button, Code, CopyButton, Group, PasswordInput, SimpleGrid, Stack, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { api, unwrap, type Me } from '@/api/client'
import { keys, useMe, useSystem } from '@/api/queries'
import { safeReturnTo, setSession } from '@/api/session'
import { errorMessage } from '@/components/ErrorAlert'
import { QRCode } from '@/components/QRCode'
import { fieldError, required } from '@/lib/form'
import { AuthLayout } from './AuthLayout'

type Step = 'password' | 'enroll' | 'codes' | 'verify'

function stepFor(me: Me | null | undefined): Step | 'done' {
  if (!me) return 'password'
  if (!me.limited) return 'done'
  return me.needs_enroll ? 'enroll' : 'verify'
}

/**
 * Sign-in (SM-AUTH-001/002): password, then TOTP. Accounts that must use MFA and have none enroll here; enrollment
 * never completes a sign-in by itself, so a fresh code is asked for afterwards (Hallpass SR-03).
 */
export function LoginPage({ returnTo }: { returnTo?: string }) {
  const qc = useQueryClient()
  const me = useMe()
  const system = useSystem()
  const [step, setStep] = useState<Step>('password')
  const [recovery, setRecovery] = useState<string[]>([])
  const [error, setError] = useState('')

  const signedIn = (m: Me) => {
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
        <Anchor size="xs" onClick={() => void api.POST('/auth/logout').then(() => window.location.assign('/login'))}>Use a different account</Anchor>
      </AuthLayout>
    )
  }

  return (
    <AuthLayout title="Sign in">
      {system.data?.local_login_disabled && <Alert color="owlYellow">Password sign-in is disabled on this server.</Alert>}
      <form
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
          {error && <Text c="red" size="sm" role="alert">{error}</Text>}
          <pw.Subscribe selector={(s) => s.isSubmitting}>
            {(busy) => <Button type="submit" loading={busy}>Sign in</Button>}
          </pw.Subscribe>
        </Stack>
      </form>
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
        {error && <Text c="red" size="sm" role="alert">{error}</Text>}
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
