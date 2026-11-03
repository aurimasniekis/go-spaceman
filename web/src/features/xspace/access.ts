import type { XSpacePeer, XSpaceRequest } from '@/api/client'

/** Badge colors of request states. */
export const stateColor: Record<XSpaceRequest['state'], string> = {
  pending: 'owlBlue', accepted: 'owlBlue', completed: 'owlGreen', failed: 'owlRed', declined: 'gray', cancelled: 'gray', expired: 'owlYellow',
}

export const kindLabel: Record<XSpaceRequest['kind'], string> = { question: 'Question', delegation: 'Delegation', knowledge: 'Knowledge' }

export const disclosureLabel: Record<string, string> = {
  none: 'Outcome only',
  summary: 'Summary (2,000 characters)',
  detailed: 'Detailed (text and evidence)',
}

/** What a peer configuration accepts, as short labels (empty: nothing). */
export function accepts(p: Pick<XSpacePeer, 'inbound'>): string[] {
  const i = p.inbound
  const out: string[] = []
  if (i.discover) out.push('discovery')
  if (i.questions) out.push('questions')
  if (i.delegation) out.push('delegation')
  if (i.knowledge) out.push(i.kbs.length > 0 ? `knowledge (${i.kbs.join(', ')})` : 'knowledge (every KB)')
  return out
}

/** What a peer configuration lets this space's control send. */
export function sends(p: Pick<XSpacePeer, 'outbound'>): string[] {
  const o = p.outbound
  return [o.questions && 'questions', o.delegation && 'delegation', o.knowledge && 'knowledge'].filter((x): x is string => !!x)
}

/** The other space of a request as the viewing space sees it. */
export function peerOf(r: XSpaceRequest): XSpaceRequest['source'] {
  return r.side === 'incoming' ? r.source : r.destination
}

/** Whether an operator of the viewing space may withdraw (outgoing) or decline (incoming) a request. */
export function actions(r: XSpaceRequest): { cancel: boolean; decline: boolean } {
  const open = r.state === 'pending' || r.state === 'accepted'
  return { cancel: r.side === 'outgoing' && open, decline: r.side === 'incoming' && r.state === 'pending' }
}
