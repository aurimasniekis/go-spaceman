import { Alert, Button, Checkbox, Group, Radio, Stack, Text, Textarea } from '@mantine/core'
import { IconCheck, IconInfoCircle, IconShieldCheck, IconX } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type HumanAction } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import type { ApprovalReq, MessageReq, QuestionReq, TodoReq } from './badges'
import { useActionApproval } from './EnforcedApproval'
import { SchemaForm } from './SchemaForm'

type Outcome = string

/**
 * The type-specific response form (SM-HUI-002). Each type has its own action: a question is answered, an approval is
 * approved or denied, a to-do is completed and a message is acknowledged. The response is sent with If-Match, so a stale
 * form never overwrites another person's response.
 */
export function RespondForm({ action, canDecide, onDone }: { action: HumanAction; canDecide: boolean; onDone?: (a: HumanAction) => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [data, setData] = useState<Record<string, unknown>>({})
  const enforced = useActionApproval(action)
  const req = (action.request ?? {}) as QuestionReq & ApprovalReq & TodoReq & MessageReq
  const options = req.options ?? []
  const freeText = options.length === 0 || !!req.allow_text
  const steps = req.steps ?? []
  const form = useForm({
    defaultValues: { choice: '', answer: '', note: '', done: steps.map(() => false) },
    onSubmitMeta: { outcome: 'answered' as Outcome },
    onSubmit: async ({ value, meta }) => {
      setError('')
      const outcome = meta.outcome
      const answer = action.type === 'question' ? (value.answer.trim() || value.choice) : undefined
      try {
        const next = unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/respond', {
          params: { path: { space_id: action.space_id, action_id: action.id }, header: { 'If-Match': ifMatch(action.version) } },
          body: { outcome, answer, note: value.note || undefined, data: action.type === 'custom' ? data : undefined },
        }))
        void qc.invalidateQueries({ queryKey: keys.human(action.space_id) })
        void qc.invalidateQueries({ queryKey: keys.humanPending })
        onDone?.(next)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const submit = (outcome: Outcome) => void form.handleSubmit({ outcome })

  return (
    <form onSubmit={(e) => { e.preventDefault(); submit(action.type === 'question' ? 'answered' : action.type === 'todo' ? 'completed' : action.type === 'custom' ? (action.type_spec?.outcomes[0] ?? 'done') : 'acknowledged') }}>
      <Stack gap="sm">
        {action.type === 'question' && (
          <>
            {options.length > 0 && (
              <form.Field name="choice">
                {(f) => (
                  <Radio.Group label="Answer" value={f.state.value} onChange={(v) => f.handleChange(v)}>
                    <Stack gap={6} mt={6}>{options.map((o) => <Radio key={o} value={o} label={o} />)}</Stack>
                  </Radio.Group>
                )}
              </form.Field>
            )}
            {freeText && (
              <form.Field name="answer">
                {(f) => <Textarea label={options.length > 0 ? 'Or write an answer' : 'Answer'} description="Markdown" autosize minRows={3} maxRows={12}
                  value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
              </form.Field>
            )}
            <form.Subscribe selector={(s) => s.values.answer.trim() !== '' || s.values.choice !== ''}>
              {(ok) => <Group justify="flex-end"><Button type="submit" disabled={!ok} leftSection={<IconCheck size={14} />}>Send answer</Button></Group>}
            </form.Subscribe>
          </>
        )}

        {action.type === 'approval' && (
          <>
            {enforced.data ? (
              <Alert color="owlBlue" variant="light" icon={<IconShieldCheck size={16} />} title="Enforced approval">
                SpaceMan runs this operation only after approval: for this run, with exactly the arguments shown below, at most {enforced.data.uses_allowed} time(s).
              </Alert>
            ) : enforced.isSuccess && (
              <Alert color="owlYellow" variant="light" icon={<IconInfoCircle size={16} />} title="Advisory decision">
                This records your decision and notifies the requester. No policy gates this request, so SpaceMan does not enforce it on any operation.
              </Alert>
            )}
            {req.action && <Text size="sm"><b>Requested:</b> {req.action}</Text>}
            {req.details && <MarkdownView source={req.details} spaceId={action.space_id} />}
            <form.Field name="note">
              {(f) => <Textarea label="Note" description="Required to deny" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            {canDecide ? (
              <form.Subscribe selector={(s) => s.values.note.trim() !== ''}>
                {(hasNote) => (
                  <Group justify="flex-end">
                    <Button color="owlRed" variant="light" leftSection={<IconX size={14} />} disabled={!hasNote} onClick={() => submit('denied')}>Deny</Button>
                    <Button color="owlGreen" leftSection={<IconCheck size={14} />} onClick={() => submit('approved')}>Approve</Button>
                  </Group>
                )}
              </form.Subscribe>
            ) : <Text size="sm" c="dimmed">Deciding approvals needs the approval:decide permission in this space.</Text>}
          </>
        )}

        {action.type === 'todo' && (
          <>
            {steps.length > 0 && (
              <form.Field name="done">
                {(f) => (
                  <Stack gap={6}>
                    <Text size="sm" fw={500}>Steps</Text>
                    {steps.map((s, i) => <Checkbox key={i} label={s} checked={f.state.value[i]} onChange={(e) => f.handleChange(f.state.value.map((v, j) => (j === i ? e.currentTarget.checked : v)))} />)}
                  </Stack>
                )}
              </form.Field>
            )}
            <form.Field name="note">
              {(f) => <Textarea label="Note" description="Optional: what you did" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <form.Subscribe selector={(s) => s.values.done.every(Boolean)}>
              {(all) => <Group justify="flex-end"><Button type="submit" disabled={!all} leftSection={<IconCheck size={14} />}>Mark completed</Button></Group>}
            </form.Subscribe>
          </>
        )}

        {action.type === 'custom' && action.type_spec && (
          <>
            <SchemaForm label="Response" schema={action.type_spec.response_schema} ui={action.type_spec.ui} value={data} onChange={setData} />
            <form.Field name="note">
              {(f) => <Textarea label="Note" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <Text size="xs" c="dimmed">A custom type records an answer; it never grants anything, whatever its outcomes are called.</Text>
            <Group justify="flex-end">
              {action.type_spec.outcomes.map((o, i) => (
                <Button key={o} variant={i === 0 ? 'filled' : 'default'} onClick={() => submit(o)}>{o.replace(/_/g, ' ')}</Button>
              ))}
            </Group>
          </>
        )}
        {action.type === 'message' && (
          <Group justify="flex-end"><Button type="submit" leftSection={<IconCheck size={14} />}>Acknowledge</Button></Group>
        )}
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
      </Stack>
    </form>
  )
}
