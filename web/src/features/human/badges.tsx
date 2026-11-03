import { Badge } from '@mantine/core'
import { IconBell, IconChecklist, IconForms, IconHelpCircle, IconMessage, IconShieldCheck, IconTrendingUp } from '@tabler/icons-react'
import type { HumanAction, HumanNotification, HumanType } from '@/api/client'

export const typeLabel: Record<HumanType, string> = { question: 'Question', approval: 'Approval', todo: 'To-do', message: 'Message', custom: 'Custom' }
export const typeIcon = { question: IconHelpCircle, approval: IconShieldCheck, todo: IconChecklist, message: IconMessage, custom: IconForms } as const
const typeColor: Record<HumanType, string> = { question: 'owlBlue', approval: 'owlRed', todo: 'owlPurple', message: 'gray', custom: 'owlGreen' }

const statusColor: Record<HumanAction['status'], string> = { open: 'owlBlue', in_progress: 'owlPurple', resolved: 'owlGreen', cancelled: 'gray', expired: 'owlYellow' }
export const statusLabel: Record<HumanAction['status'], string> = { open: 'Open', in_progress: 'In progress', resolved: 'Resolved', cancelled: 'Cancelled', expired: 'Expired' }
const outcomeColor: Record<string, string> = { answered: 'owlGreen', approved: 'owlGreen', denied: 'owlRed', completed: 'owlGreen', acknowledged: 'owlGreen', declined: 'owlRed', rejected: 'owlRed' }

export function TypeBadge({ type, slug }: { type: HumanType; slug?: string }) {
  const Icon = typeIcon[type]
  return <Badge color={typeColor[type]} variant="light" leftSection={<Icon size={12} />}>{type === 'custom' && slug ? slug : typeLabel[type]}</Badge>
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

/** Reminded and escalated markers (SM-HUM-011): reminders re-surface an action, escalation raised it. */
export function FollowUpBadges({ action }: { action: Pick<HumanAction, 'reminder_count' | 'escalated_at' | 'escalated_from' | 'priority'> }) {
  return (
    <>
      {action.reminder_count > 0 && (
        <Badge variant="outline" color="owlYellow" leftSection={<IconBell size={12} />}>
          reminded{action.reminder_count > 1 ? ` ×${action.reminder_count}` : ''}
        </Badge>
      )}
      {action.escalated_at && (
        <Badge variant="outline" color="owlRed" leftSection={<IconTrendingUp size={12} />}>escalated</Badge>
      )}
    </>
  )
}

export const routeReasonLabel: Record<string, string> = {
  requester_ended: 'the requester\'s run had ended',
  run_ended: 'the run it was routed to ended',
  hold_expired: 'no run of the requester\'s agent instance started in time',
  operator: 'a person routed it',
  too_many_routes: 'it was routed too many times',
}

/** Where a response notification is now, in words. */
export function notificationTarget(n: HumanNotification): string {
  switch (n.target_kind) {
    case 'run': return `run ${n.target_run}${n.target_role ? ` (${n.target_role})` : ''}`
    case 'instance': return `the next run of agent instance ${n.target_instance}`
    case 'control': return 'the space\'s control'
    default: return 'nobody: undeliverable'
  }
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
export interface Resp { answer?: string; note?: string; data?: unknown }
