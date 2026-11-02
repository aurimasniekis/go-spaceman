import { Badge, Tooltip } from '@mantine/core'
import type { Delivery, Subscription } from '@/api/client'

const stateColor: Record<Subscription['state'], string> = { running: 'owlGreen', stopped: 'gray', error: 'owlRed', disabled: 'gray' }

export function SubscriptionState({ sub }: { sub: Subscription }) {
  const badge = <Badge color={stateColor[sub.state]}>{sub.state}{sub.cursor_pending ? ' · applying cursor' : ''}</Badge>
  return sub.last_error ? <Tooltip label={sub.last_error} multiline w={360}>{badge}</Tooltip> : badge
}

const outcomeColor: Record<Delivery['outcome'], string> = { accepted: 'owlGreen', duplicate: 'gray', filtered: 'gray', quarantined: 'owlRed', replayed: 'owlPurple' }

export function OutcomeBadge({ outcome }: { outcome: Delivery['outcome'] }) {
  return <Badge variant="light" color={outcomeColor[outcome]}>{outcome}</Badge>
}
