import { Button, Group, Stack, Text, Tooltip } from '@mantine/core'
import { IconMessageQuestion } from '@tabler/icons-react'
import { useState } from 'react'
import { api, unwrap, type HumanAction } from '@/api/client'
import { Composer } from '@/components/composer/Composer'
import { errorMessage } from '@/components/ErrorAlert'
import { useAppForm } from '@/components/form/appForm'
import { isPending } from './badges'

/**
 * Writing on a human action (SM-HUM-006): a comment, or, while the action is pending, a clarification request that
 * asks the requester for more information. Both are discussion and never a response. The text is Markdown from the
 * TipTap composer, so files can be pasted, dropped or attached (SM-ATT-001/002).
 */
export function CommentBox({ action, onDone }: { action: HumanAction; onDone: (a: HumanAction) => void }) {
  const [key, setKey] = useState(0)
  const [error, setError] = useState('')
  const path = { space_id: action.space_id, action_id: action.id }
  const form = useAppForm({
    defaultValues: { body: '', attachmentIds: [] as string[] },
    onSubmitMeta: { clarify: false },
    onSubmit: async ({ value, meta }) => {
      setError('')
      const body = { body: value.body, attachment_ids: value.attachmentIds.length ? value.attachmentIds : undefined }
      try {
        const next = meta.clarify
          ? unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/clarify', { params: { path }, body }))
          : unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/comments', { params: { path }, body }))
        form.reset()
        setKey((k) => k + 1)
        onDone(next)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const pending = isPending(action)
  const agent = action.requester_kind === 'agent'
  return (
    <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit({ clarify: false }) }}>
      <Stack gap={6}>
        <Composer key={key} spaceId={action.space_id} ariaLabel="Comment" minHeight={70} placeholder="Add a comment (Markdown; paste, drop or attach files)"
          onChange={(v) => { form.setFieldValue('body', v.markdown); form.setFieldValue('attachmentIds', v.attachmentIds) }}
          onSubmit={() => void form.handleSubmit({ clarify: false })} />
        <form.Subscribe selector={(s) => [s.values.body.trim() !== '', s.isSubmitting] as const}>
          {([has, busy]) => (
            <Group justify="flex-end" gap="xs">
              {pending && (
                <Tooltip multiline w={280} label={agent
                  ? 'The requesting agent is notified (like a response) and answers with a reply. The action stays pending.'
                  : 'Marks this comment as a question for the requester. The action stays pending.'}>
                  <Button variant="default" leftSection={<IconMessageQuestion size={14} />} disabled={!has} loading={busy}
                    onClick={() => void form.handleSubmit({ clarify: true })}>Ask the requester</Button>
                </Tooltip>
              )}
              <Button type="submit" variant="default" disabled={!has} loading={busy}>Comment</Button>
            </Group>
          )}
        </form.Subscribe>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
      </Stack>
    </form>
  )
}
