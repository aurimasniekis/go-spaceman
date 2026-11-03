import { Badge, Tooltip } from '@mantine/core'
import type { Delivery, Subscription } from '@/api/client'

const stateColor: Record<Subscription['state'], string> = { running: 'owlGreen', stopped: 'gray', error: 'owlRed', disabled: 'gray' }

export function SubscriptionState({ sub }: { sub: Subscription }) {
  const badge = <Badge color={stateColor[sub.state]}>{sub.state}{sub.cursor_pending ? ' · applying cursor' : ''}</Badge>
  return sub.last_error ? <Tooltip label={sub.last_error} multiline w={360}>{badge}</Tooltip> : badge
}

/** What a core NATS subscription gives up; shown wherever one is created or inspected (SM-EVT-011, ADR-0042). */
export const coreExplanation =
  'Core NATS is non-durable: there is no replay, no redelivery and no cursor, and messages published while SpaceMan is disconnected (or that arrive faster than it stores them) are lost.'

/** Marks a core NATS subscription as non-durable; durable subscriptions get no badge. */
export function DeliveryBadge({ sub }: { sub: Pick<Subscription, 'delivery'> }) {
  if (sub.delivery !== 'core') return null
  return <Tooltip label={coreExplanation} multiline w={360}><Badge color="owlYellow" variant="light">core NATS · non-durable</Badge></Tooltip>
}

const outcomeColor: Record<Delivery['outcome'], string> = { accepted: 'owlGreen', duplicate: 'gray', filtered: 'gray', quarantined: 'owlRed', replayed: 'owlPurple' }

export function OutcomeBadge({ outcome }: { outcome: Delivery['outcome'] }) {
  return <Badge variant="light" color={outcomeColor[outcome]}>{outcome}</Badge>
}
