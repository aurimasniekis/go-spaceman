import { createFileRoute } from '@tanstack/react-router'
import { useCallback } from 'react'
import { SpaceTerminals } from '@/features/runs/SpaceTerminals'

type Search = { run?: string; view?: 'live' | 'replay' }

/** The space's Terminals tab: every agent as a tab, with the selected one's terminal (UX review §4.3). */
export const Route = createFileRoute('/spaces/$spaceId/terminals')({
  validateSearch: (s: Record<string, unknown>): Search => ({
    run: typeof s.run === 'string' && s.run ? s.run : undefined,
    view: s.view === 'live' || s.view === 'replay' ? s.view : undefined,
  }),
  component: TerminalsPage,
})

function TerminalsPage() {
  const { spaceId } = Route.useParams()
  const { run, view } = Route.useSearch()
  const navigate = Route.useNavigate()
  const onSelect = useCallback((id: string) => void navigate({ search: { run: id }, replace: true }), [navigate])
  const onMode = useCallback((v: 'live' | 'replay') => void navigate({ search: (p: Search) => ({ ...p, view: v }), replace: true }), [navigate])
  return <SpaceTerminals spaceId={spaceId} runId={run} mode={view} onSelect={onSelect} onMode={onMode} />
}
