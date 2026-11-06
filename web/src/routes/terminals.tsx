import { createFileRoute } from '@tanstack/react-router'
import { useCallback } from 'react'
import { Workspace } from '@/features/runs/Workspace'

type Search = { space?: string; run?: string; view?: 'live' | 'replay' }

/** Every space's agents in one place: a space and agent list beside the selected space's terminals (UX review §4.4). */
export const Route = createFileRoute('/terminals')({
  validateSearch: (s: Record<string, unknown>): Search => ({
    space: typeof s.space === 'string' && s.space ? s.space : undefined,
    run: typeof s.run === 'string' && s.run ? s.run : undefined,
    view: s.view === 'live' || s.view === 'replay' ? s.view : undefined,
  }),
  component: TerminalsWorkspace,
})

function TerminalsWorkspace() {
  const { space, run, view } = Route.useSearch()
  const navigate = Route.useNavigate()
  const onSelect = useCallback((s: string, r?: string) => void navigate({ search: { space: s, run: r }, replace: true }), [navigate])
  const onMode = useCallback((v: 'live' | 'replay') => void navigate({ search: (p: Search) => ({ ...p, view: v }), replace: true }), [navigate])
  return <Workspace spaceId={space} runId={run} mode={view} onSelect={onSelect} onMode={onMode} />
}
