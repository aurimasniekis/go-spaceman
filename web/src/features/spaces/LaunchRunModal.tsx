import { Button, Group, Modal, Select, SimpleGrid, Stack, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type LaunchInput, type Node, type Space } from '@/api/client'
import { keys, useSpaceAccounts, useSpaceRoles } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError } from '@/lib/form'

const ROLE_RE = /^[a-z][a-z0-9-]{0,47}$/
// The "another role" choice; the colon keeps it apart from every valid role name (a space role may be called custom).
const CUSTOM_ROLE = ':custom'

/** The space-role picker's options: each configured role by name, then the choice to name another role. */
export function spaceRoleOptions(roles: { name: string }[]): { value: string; label: string }[] {
  return [...roles.map((r) => ({ value: r.name, label: r.name })), { value: CUSTOM_ROLE, label: 'Another role (choose kind and harness)' }]
}

type Kind = NonNullable<LaunchInput['kind']>
type Harness = NonNullable<LaunchInput['harness']>

const KINDS: { value: Kind; label: string }[] = [
  { value: 'control', label: 'Control — coordinates the space' },
  { value: 'worker', label: 'Worker — implements one task' },
  { value: 'diagnostic', label: 'Diagnostic — one-off investigation' },
]

/** Launches one run on the space's pinned node (SM-RUN-001). The idempotency key makes a retried submit safe. */
export function LaunchRunModal({ space, node, opened, onClose }: { space: Space; node?: Node; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const available = (node?.capabilities.harnesses ?? []).filter((h) => h.available).map((h) => h.kind)
  const bound = useSpaceAccounts(space.id).data ?? []
  const roles = useSpaceRoles(space.id).data ?? []

  const form = useForm({
    defaultValues: { spaceRole: '', harness: (available[0] ?? 'claude-code') as Harness, kind: 'control' as Kind, role: 'control', model: '', prompt: '', system: '', workspace: '', account: '', repo: '', branch: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const run = unwrap(
          await api.POST('/spaces/{space_id}/runs', {
            params: { path: { space_id: space.id }, header: { 'Idempotency-Key': key } },
            body: { ...value, spaceRole: undefined, account: undefined, repo: undefined, branch: undefined, account_ref: value.account || undefined, model: value.model || undefined,
              // A configured role brings its own kind and harness (its template and profile); a free role names them.
              ...(value.spaceRole ? { role: value.spaceRole, kind: undefined, harness: undefined } : {}),
              worktree: value.repo ? { repo: value.repo, branch: value.branch || `spaceman/${value.role}` } : undefined, prompt: value.prompt || undefined, system: value.system || undefined, workspace: value.workspace || undefined },
          }),
        )
        void qc.invalidateQueries({ queryKey: keys.runs(space.id) })
        setKey(idempotencyKey())
        form.reset()
        onClose()
        void navigate({ to: '/spaces/$spaceId/runs/$runId', params: { spaceId: space.id, runId: run.id } })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Modal opened={opened} onClose={onClose} title={`Launch a run in ${space.name}`} size="lg">
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="sm">
          {node && !node.connected && <Text c="owlYellow" size="sm">The pinned node {node.name} is offline; the launch waits until it reconnects.</Text>}
          {roles.length > 0 && (
            <form.Field name="spaceRole">
              {(f) => <Select label="Space role" description="A configured role brings its template, profile and prompt" allowDeselect={false}
                value={f.state.value || CUSTOM_ROLE} onChange={(v) => f.handleChange(v === CUSTOM_ROLE || !v ? '' : v)}
                data={spaceRoleOptions(roles)} />}
            </form.Field>
          )}
          <form.Subscribe selector={(st) => st.values.spaceRole}>
            {(spaceRole) => (
              <SimpleGrid cols={2}>
                {!spaceRole && (
                  <>
                    <form.Field name="harness">
                      {(f) => (
                        <Select label="Harness" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Harness)}
                          data={[
                            { value: 'claude-code', label: `Claude Code${available.includes('claude-code') ? '' : ' (not detected)'}` },
                            { value: 'codex', label: `Codex${available.includes('codex') ? '' : ' (not detected)'}` },
                          ]} />
                      )}
                    </form.Field>
                    <form.Field name="kind">
                      {(f) => <Select label="Run kind" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Kind)} data={KINDS} />}
                    </form.Field>
                    <form.Field name="role" validators={{ onChange: ({ value }) => (!ROLE_RE.test(value) ? 'Lowercase letters, digits and dashes' : undefined) }}>
                      {(f) => <TextInput label="Role" description="e.g. control, implementor-backend" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
                    </form.Field>
                  </>
                )}
                <form.Field name="model">
                  {(f) => <TextInput label="Model" description={spaceRole ? "The role's model when empty" : 'Harness default when empty'} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
                </form.Field>
              </SimpleGrid>
            )}
          </form.Subscribe>
          <form.Subscribe selector={(st) => st.values.harness}>
            {(harness) => {
              const usable = bound.filter((b) => b.harness === harness && b.enabled)
              const dflt = usable.find((b) => b.default) ?? (usable.length === 1 ? usable[0] : undefined)
              return (
                <form.Field name="account">
                  {(f) => (
                    <Select label="Account" clearable value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')}
                      placeholder={dflt ? `Space default: ${dflt.name}` : usable.length ? 'Choose an account' : 'No account bound'}
                      description={usable.length === 0 ? 'No enabled account for this harness is bound to the space; the launch will be refused.' : undefined}
                      data={usable.map((b) => ({ value: b.account_id, label: b.name ?? b.account_id }))} />
                  )}
                </form.Field>
              )
            }}
          </form.Subscribe>
          <form.Field name="prompt">
            {(f) => <Textarea label="Initial prompt" autosize minRows={3} maxRows={10} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="system">
            {(f) => <Textarea label="Additional system prompt" description="Appended after the space's standing instructions" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
<form.Subscribe selector={(st) => st.values.repo}>
            {(repo) => (
              <SimpleGrid cols={2}>
                <form.Field name="repo">
                  {(f) => <TextInput label="Git worktree of" description="A repository folder inside the space (optional)" placeholder="repos/payments-api" classNames={{ input: 'mono' }}
                    value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
                </form.Field>
                {repo ? (
                  <form.Field name="branch">
                    {(f) => <TextInput label="Branch" description="Created from HEAD when new" placeholder="spaceman/<role>" classNames={{ input: 'mono' }}
                      value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
                  </form.Field>
                ) : (
                  <form.Field name="workspace">
                    {(f) => <TextInput label="Workspace" description="Directory inside the space's workdir (empty: the workdir itself)" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
                  </form.Field>
                )}
              </SimpleGrid>
            )}
          </form.Subscribe>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([can, busy]) => <Button type="submit" disabled={!can} loading={busy}>Launch</Button>}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
