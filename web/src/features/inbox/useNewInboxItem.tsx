import { Anchor, Text } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useNavigate, useRouter } from '@tanstack/react-router'
import { useCallback, useState, type ReactNode } from 'react'
import type { InboxItem } from '@/api/client'
import { useMe } from '@/api/queries'
import { atLeast, roleIn } from '@/lib/access'
import { closeButtonProps } from '@/lib/notify'
import { NewItemModal, type NewItemPrefill } from './NewItemModal'

/** Whether the signed-in user may add items to a space's inbox: the same check as the Inbox page's "New item". */
export function useCanCreateInboxItem(spaceId: string): boolean {
  return atLeast(roleIn(useMe().data, spaceId), 'operator')
}

/**
 * Opens the "New inbox item" dialog from anywhere in a space (the overview's header action, and later the terminal
 * page): `open()` shows it, `open({ body })` shows it prefilled. Render `modal` once. After an item is added, a toast
 * confirms it with a link to the item, and the user stays where they were.
 *
 * A plain `open()` keeps an unsent draft from an earlier opening; a prefilled one replaces its text.
 */
export function useNewInboxItem(spaceId: string): { open: (prefill?: NewItemPrefill) => void; modal: ReactNode } {
  const navigate = useNavigate()
  const router = useRouter()
  const [opened, setOpened] = useState(false)
  const [prefill, setPrefill] = useState<NewItemPrefill>()
  const open = useCallback((value?: NewItemPrefill) => {
    // A new object each time, so opening twice with the same text still replaces an edited draft.
    if (value) setPrefill({ ...value })
    setOpened(true)
  }, [])
  const onCreated = (it: InboxItem) => {
    const to = { to: '/spaces/$spaceId/inbox/$itemId', params: { spaceId, itemId: it.id } } as const
    // The toast container sits outside the router, so the link navigates through the router captured here.
    notifications.show({
      color: 'owlGreen',
      title: 'Added to the inbox',
      closeButtonProps,
      message: (
        <Text size="sm" component="span">
          <Anchor size="sm" href={router.buildLocation(to).href} data-testid="new-inbox-item-link"
            onClick={(e) => {
              if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
              e.preventDefault()
              void navigate(to)
            }}>
            {it.title}
          </Anchor>
        </Text>
      ),
    })
  }
  const modal = (
    <NewItemModal spaceId={spaceId} prefill={prefill} opened={opened} onClose={() => setOpened(false)} onCreated={onCreated} />
  )
  return { open, modal }
}
