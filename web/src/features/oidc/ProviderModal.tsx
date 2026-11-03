import { Alert, Button, Group, Modal, PasswordInput, SegmentedControl, Select, SimpleGrid, Stack, Switch, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { IconLock } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, ifMatch, unwrap, type OidcProvider, type OidcProviderInput } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError } from '@/lib/form'

const SLUG_RE = /^[a-z][a-z0-9-]{0,31}$/

interface Values {
  display_name: string
  slug: string
  issuer: string
  client_id: string
  client_secret: string
  remove_secret: boolean
  scopes: string[]
  groups_claim: string
  provisioning: 'preprovisioned' | 'jit'
  email_domains: string[]
  groups: string[]
  tenants: string[]
  hosted_domains: string[]
  stepup_fallback: 'none' | 'local_totp'
  acr_values: string[]
  ca_bundle_pem: string
  allow_private_network: boolean
  allow_cross_origin_endpoints: boolean
  end_session_enabled: boolean
  enabled: boolean
}

function initial(p?: OidcProvider): Values {
  return {
    display_name: p?.display_name ?? '',
    slug: p?.slug ?? '',
    issuer: p?.issuer ?? '',
    client_id: p?.client_id ?? '',
    client_secret: '',
    remove_secret: false,
    scopes: p?.scopes ?? ['openid', 'profile', 'email', 'groups'],
    groups_claim: p?.groups_claim ?? 'porch.groups',
    provisioning: p?.provisioning ?? 'preprovisioned',
    email_domains: p?.jit.email_domains ?? [],
    groups: p?.jit.groups ?? [],
    tenants: p?.jit.tenants ?? [],
    hosted_domains: p?.jit.hosted_domains ?? [],
    stepup_fallback: p?.stepup_fallback ?? 'none',
    acr_values: p?.acr_values ?? [],
    ca_bundle_pem: p?.ca_bundle_pem ?? '',
    allow_private_network: p?.allow_private_network ?? false,
    allow_cross_origin_endpoints: p?.allow_cross_origin_endpoints ?? false,
    end_session_enabled: p?.end_session_enabled ?? false,
    enabled: p?.enabled ?? false,
  }
}

function body(v: Values, editing: boolean): OidcProviderInput {
  const secret = v.remove_secret ? '' : v.client_secret || undefined
  return {
    display_name: v.display_name,
    slug: v.slug,
    issuer: v.issuer,
    client_id: v.client_id,
    // Write-only: omitted keeps the stored secret, "" removes it.
    client_secret: editing ? secret : v.client_secret || undefined,
    scopes: v.scopes,
    groups_claim: v.groups_claim,
    provisioning: v.provisioning,
    jit: { email_domains: v.email_domains, groups: v.groups, tenants: v.tenants, hosted_domains: v.hosted_domains },
    stepup_fallback: v.stepup_fallback,
    acr_values: v.acr_values,
    ca_bundle_pem: v.ca_bundle_pem,
    allow_private_network: v.allow_private_network,
    allow_cross_origin_endpoints: v.allow_cross_origin_endpoints,
    end_session_enabled: v.end_session_enabled,
    enabled: v.enabled,
  }
}

/**
 * Adds or edits an OIDC sign-in provider (SM-AUTH-001/002). The client secret is write-only: it is never filled from the
 * server; on an edit an empty value keeps the stored one. The defaults fit Porch (groups scope, porch.groups).
 */
export function ProviderModal({ provider, redirectUri, onClose }: { provider?: OidcProvider; redirectUri: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const editing = !!provider
  const form = useForm({
    defaultValues: initial(provider),
    onSubmit: async ({ value }) => {
      setError('')
      try {
        if (provider) {
          unwrap(await api.PATCH('/oidc/providers/{provider_id}', {
            params: { path: { provider_id: provider.id }, header: { 'If-Match': ifMatch(provider.version) } }, body: body(value, true),
          }))
        } else {
          unwrap(await api.POST('/oidc/providers', { params: { header: { 'Idempotency-Key': key } }, body: body(value, false) }))
          setKey(idempotencyKey())
        }
        void qc.invalidateQueries({ queryKey: keys.oidcProviders })
        void qc.invalidateQueries({ queryKey: keys.signInProviders })
        void qc.invalidateQueries({ queryKey: ['oidc-sign-in-settings'] })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={editing ? `Edit ${provider.display_name}` : 'Add a sign-in provider'} size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            Register SpaceMan at the provider as a confidential client with the redirect URI <code>{redirectUri}</code> and PKCE (S256). For Porch, create an
            application in the tenant, request the <code>groups</code> scope and enable group disclosure.
          </Text>
          <SimpleGrid cols={2}>
            <form.Field name="display_name" validators={{ onChange: ({ value }) => (!value.trim() ? 'Required' : undefined) }}>
              {(f) => <TextInput label="Name on the sign-in page" placeholder="Porch" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="slug" validators={{ onChange: ({ value }) => (!SLUG_RE.test(value) ? 'Lowercase letters, digits and dashes; starts with a letter' : undefined) }}>
              {(f) => <TextInput label="Slug" description="In sign-in URLs and audit records" placeholder="porch" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="issuer" validators={{ onChange: ({ value }) => (!/^https?:\/\//.test(value) ? 'An https URL' : undefined) }}>
            {(f) => <TextInput label="Issuer" description="Discovery is read from <issuer>/.well-known/openid-configuration; its issuer must match exactly" placeholder="https://id.example.com" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <SimpleGrid cols={2}>
            <form.Field name="client_id" validators={{ onChange: ({ value }) => (!value.trim() ? 'Required' : undefined) }}>
              {(f) => <TextInput label="Client ID" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="client_secret">
              {(f) => (
                <PasswordInput label={editing ? 'Replace the client secret' : 'Client secret'} leftSection={<IconLock size={14} />} autoComplete="off"
                  description={editing ? (provider.has_client_secret ? 'A secret is stored. Leave empty to keep it.' : 'No secret is stored.') : 'Sealed when saved; never shown again.'}
                  value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />
              )}
            </form.Field>
          </SimpleGrid>
          {editing && provider.has_client_secret && (
            <form.Field name="remove_secret">
              {(f) => <Switch label="Remove the stored client secret (public client)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          )}
          <SimpleGrid cols={2}>
            <form.Field name="scopes" validators={{ onChange: ({ value }) => (!value.includes('openid') ? 'Must include openid' : undefined) }}>
              {(f) => <TagsInput label="Scopes" value={f.state.value} onChange={f.handleChange} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="groups_claim">
              {(f) => <TextInput label="Groups claim" description="A dotted path reads a nested claim (Porch: porch.groups)" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="provisioning">
            {(f) => (
              <Stack gap={4}>
                <Text size="sm" fw={500} id="provisioning-label">Who can sign in</Text>
                <SegmentedControl aria-labelledby="provisioning-label" value={f.state.value} onChange={(v) => f.handleChange(v as Values['provisioning'])}
                  data={[{ value: 'preprovisioned', label: 'Linked users only' }, { value: 'jit', label: 'Create users at first sign-in (JIT)' }]} />
              </Stack>
            )}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.provisioning}>
            {(mode) => mode === 'jit' && (
              <Stack gap="xs">
                <Text size="xs" c="dimmed">At least one rule is required and every rule you set must match. New users get no grants of their own; mappings give them grants.</Text>
                <SimpleGrid cols={2}>
                  <form.Field name="groups">{(f) => <TagsInput label="Required groups (any of)" value={f.state.value} onChange={f.handleChange} />}</form.Field>
                  <form.Field name="email_domains">{(f) => <TagsInput label="Verified email domains" value={f.state.value} onChange={f.handleChange} />}</form.Field>
                  <form.Field name="tenants">{(f) => <TagsInput label="Tenants (tid)" value={f.state.value} onChange={f.handleChange} />}</form.Field>
                  <form.Field name="hosted_domains">{(f) => <TagsInput label="Hosted domains (hd)" value={f.state.value} onChange={f.handleChange} />}</form.Field>
                </SimpleGrid>
              </Stack>
            )}
          </form.Subscribe>
          <SimpleGrid cols={2}>
            <form.Field name="stepup_fallback">
              {(f) => (
                <Select label="Step-up without a fresh login proof" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Values['stepup_fallback'])}
                  description="When the provider sends no auth_time"
                  data={[{ value: 'none', label: 'Refuse' }, { value: 'local_totp', label: 'Ask for the SpaceMan TOTP code' }]} />
              )}
            </form.Field>
            <form.Field name="acr_values">
              {(f) => <TagsInput label="Required acr values" description="Optional; step-up must return one of them" value={f.state.value} onChange={f.handleChange} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="ca_bundle_pem">
            {(f) => <Textarea label="Extra CA certificates (PEM)" description="Only adds trust; TLS verification is always on" autosize minRows={2} maxRows={6} classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <Group gap="xl">
            <form.Field name="allow_private_network">
              {(f) => <Switch label="Allow a private-network provider" description="Loopback, private and link-local addresses are refused otherwise" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
            <form.Field name="allow_cross_origin_endpoints">
              {(f) => <Switch label="Allow endpoints on another site" description="Token and key endpoints outside the issuer's domain" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          </Group>
          <form.Field name="end_session_enabled">
            {(f) => <Switch label="Sign out at the provider too" description="RP-initiated logout: after signing out of an SSO session, offer the provider's end_session_endpoint (when it advertises one)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <form.Field name="enabled">
            {(f) => <Switch label="Enabled" description="Shown on the sign-in page. Run a test login first." checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <Alert variant="light" color="owlBlue">
            SpaceMan identifies people by the provider's issuer and subject, never by email. Access and refresh tokens are discarded after sign-in.
          </Alert>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([can, busy]) => <Button type="submit" disabled={!can} loading={busy}>{editing ? 'Save' : 'Add provider'}</Button>}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
