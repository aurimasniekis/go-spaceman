import { Alert, Button, Group, Modal, PasswordInput, Select, Stack, Switch, Text, Textarea, TextInput } from '@mantine/core'
import { IconLock } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, ifMatch, unwrap, type Account } from '@/api/client'
import { keys, useAccountKinds } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError } from '@/lib/form'

const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,62}$/

/** A Codex ChatGPT login (ADR-0053) is the auth.json `codex login` writes; the server checks it the same way. */
function loginError(value: string): string | undefined {
  try {
    const f = JSON.parse(value) as { auth_mode?: unknown; tokens?: unknown }
    if (f.auth_mode !== 'chatgpt' || !f.tokens || typeof f.tokens !== 'object') return 'Paste the auth.json of a ChatGPT login (auth_mode "chatgpt", with its tokens)'
    return undefined
  } catch {
    return 'Not valid JSON: paste the whole auth.json file'
  }
}

/**
 * Adds or edits a harness account (SM-ACCT-001). The secret field is write-only: it is never filled from the server,
 * and on an edit an empty value keeps the stored credential while a value rotates it (later launches use it).
 */
export function AccountModal({ account, opened, onClose }: { account?: Account; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const kinds = useAccountKinds().data ?? []
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const editing = !!account
  const form = useForm({
    defaultValues: { name: account?.name ?? '', kind: account?.kind ?? 'claude_oauth_token', description: account?.description ?? '', secret: '', enabled: account?.enabled ?? true },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        if (account) {
          unwrap(await api.PUT('/accounts/{account_id}', {
            params: { path: { account_id: account.id }, header: { 'If-Match': ifMatch(account.version) } },
            body: { name: value.name, description: value.description, enabled: value.enabled, secret: value.secret || undefined, metadata: account.metadata },
          }))
        } else {
          unwrap(await api.POST('/accounts', {
            params: { header: { 'Idempotency-Key': key } },
            body: { name: value.name, kind: value.kind as Account['kind'], description: value.description, enabled: value.enabled, secret: value.secret },
          }))
          setKey(idempotencyKey())
        }
        void qc.invalidateQueries({ queryKey: keys.accounts })
        form.reset()
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title={editing ? `Edit ${account.name}` : 'Add an account'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="name" validators={{ onChange: ({ value }) => (!NAME_RE.test(value) ? 'Lowercase letters, digits, dots, dashes or underscores' : undefined) }}>
            {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="kind">
            {(f) => <Select label="Kind" allowDeselect={false} disabled={editing} value={f.state.value} onChange={(v) => v && f.handleChange(v as Account['kind'])}
              description={editing ? 'The kind of an account never changes' : undefined}
              data={kinds.map((k) => ({ value: k.name, label: `${k.label} — ${k.harness === 'codex' ? 'Codex' : 'Claude Code'}` }))} />}
          </form.Field>
          <form.Subscribe selector={(st) => st.values.kind}>
            {(kind) => {
              const isFile = kinds.find((k) => k.name === kind)?.delivery === 'file'
              return (
                <form.Field name="secret" validators={{
                  onChangeListenTo: ['kind'],
                  onChange: ({ value }) => (!editing && !value ? 'Required' : !value ? undefined : isFile ? loginError(value) : /\s/.test(value) ? 'One line without spaces' : undefined),
                }}>
                  {(f) => isFile ? (
                    <Textarea label={editing ? 'Replace the login' : 'Login (auth.json)'} autosize minRows={4} maxRows={10} autoComplete="off" spellCheck={false}
                      classNames={{ input: 'mono' }}
                      description={(editing ? 'Leave empty to keep the stored login. ' : '') +
                        'Paste the auth.json that `codex login` wrote (in ~/.codex or $CODEX_HOME). Sealed when saved; it can never be shown again.'}
                      value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
                  ) : (
                    <PasswordInput label={editing ? 'Rotate the credential' : 'Credential'} leftSection={<IconLock size={14} />} autoComplete="off"
                      description={editing ? 'Leave empty to keep the stored credential. A new value applies to later launches.' : 'Sealed when saved; it can never be shown again.'}
                      value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
                  )}
                </form.Field>
              )
            }}
          </form.Subscribe>
          <form.Field name="description">
            {(f) => <Textarea label="Description" autosize minRows={2} maxRows={5} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="enabled">
            {(f) => <Switch label="Enabled" description="Disabled accounts are never chosen for a launch" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <form.Subscribe selector={(st) => kinds.find((k) => k.name === st.values.kind)?.delivery === 'file'}>
            {(isFile) => isFile ? (
              <Alert variant="light" color="owlYellow">
                Codex refreshes this login itself, so the node keeps it in a file (0600, owned by the node agent; people with a shell on the node can read it) that every Codex instance there shares.
                A refresh token works once: use one login on one node, or the copies stop working after the first refresh.
              </Alert>
            ) : (
              <Alert variant="light" color="owlBlue">
                A run receives the credential only when it starts, through a file that is removed once read. People with a shell on the node can still read
                a running harness's credential.
              </Alert>
            )}
          </form.Subscribe>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([can, busy]) => <Button type="submit" disabled={!can} loading={busy}>{editing ? 'Save' : 'Add account'}</Button>}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
