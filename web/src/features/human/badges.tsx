import { Badge } from '@mantine/core'
import { IconChecklist, IconHelpCircle, IconMessage, IconShieldCheck } from '@tabler/icons-react'
import type { HumanAction, HumanType } from '@/api/client'

export const typeLabel: Record<HumanType, string> = { question: 'Question', approval: 'Approval', todo: 'To-do', message: 'Message' }
export const typeIcon = { question: IconHelpCircle, approval: IconShieldCheck, todo: IconChecklist, message: IconMessage } as const
const typeColor: Record<HumanType, string> = { question: 'owlBlue', approval: 'owlRed', todo: 'owlPurple', message: 'gray' }

const statusColor: Record<HumanAction['status'], string> = { open: 'owlBlue', in_progress: 'owlPurple', resolved: 'owlGreen', cancelled: 'gray', expired: 'owlYellow' }
export const statusLabel: Record<HumanAction['status'], string> = { open: 'Open', in_progress: 'In progress', resolved: 'Resolved', cancelled: 'Cancelled', expired: 'Expired' }
const outcomeColor: Record<string, string> = { answered: 'owlGreen', approved: 'owlGreen', denied: 'owlRed', completed: 'owlGreen', acknowledged: 'owlGreen' }

export function TypeBadge({ type }: { type: HumanType }) {
  const Icon = typeIcon[type]
  return <Badge color={typeColor[type]} variant="light" leftSection={<Icon size={12} />}>{typeLabel[type]}</Badge>
}

/** Lifecycle and outcome are separate (SM-HUM-005): a resolved approval can be approved or denied. */
export function HumanStatusBadge({ action }: { action: Pick<HumanAction, 'status' | 'outcome'> }) {
  if (action.status === 'resolved' && action.outcome) return <Badge color={outcomeColor[action.outcome] ?? 'gray'} variant="light">{action.outcome}</Badge>
  return <Badge color={statusColor[action.status]} variant="light">{statusLabel[action.status]}</Badge>
}

export function PriorityBadge({ priority }: { priority: HumanAction['priority'] }) {
  if (priority === 'normal') return null
  return <Badge color={priority === 'urgent' ? 'owlRed' : priority === 'high' ? 'owlYellow' : 'gray'} variant={priority === 'urgent' ? 'filled' : 'light'}>{priority}</Badge>
}

export function requesterLabel(a: HumanAction): string {
  if (a.requester_kind === 'system') return 'SpaceMan'
  if (a.requester_kind === 'operator') return a.requester_label || 'An operator'
  return [a.requester_label || a.requester_role || 'An agent', a.requester_run].filter(Boolean).join(' · ')
}

export const isPending = (a: Pick<HumanAction, 'status'>) => a.status === 'open' || a.status === 'in_progress'

export interface QuestionReq { options?: string[]; allow_text?: boolean }
export interface ApprovalReq { action?: string; details?: string }
export interface TodoReq { steps?: string[] }
export interface MessageReq { requires_ack?: boolean }
export interface Resp { answer?: string; note?: string }
