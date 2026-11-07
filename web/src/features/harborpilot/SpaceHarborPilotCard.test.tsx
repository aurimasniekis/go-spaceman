import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me, type SpaceHarborPilot } from '@/api/client'
import { keys } from '@/api/queries'
import { SpaceHarborPilotCard } from './SpaceHarborPilotCard'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})

const SPACE = 'spc_1'

function settings(extra: Partial<SpaceHarborPilot> = {}): SpaceHarborPilot {
  return { space_id: SPACE, stored: true, enabled: true, project_id: 'prj_web', harnesses: ['claude-code', 'gemini-cli'], ttl_seconds: 86400,
    on_budget_status_unavailable: 'refuse', allowed_model_aliases: ['coding/default'], connected: true, warnings: [], version: 2, ...extra } as SpaceHarborPilot
}

function me(role: 'viewer' | 'operator' | 'admin', domain: string = SPACE): Me {
  return { root: false, method: 'password', user: { grants: [{ role, domain }] } } as unknown as Me
}

function wrap(s: SpaceHarborPilot, m: Me) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.spaceHarborPilot(SPACE), s)
  qc.setQueryData(keys.me, m)
  return render(<MantineProvider><QueryClientProvider client={qc}><SpaceHarborPilotCard spaceId={SPACE} /></QueryClientProvider></MantineProvider>)
}

describe('space Harbor Pilot card', () => {
  it('shows an enabled space: project, harnesses (any kind), lifetime, budget fallback, models and warnings', () => {
    wrap(settings({ warnings: ['The parent token expires soon.'] }), me('operator'))
    expect(screen.getByText('Enabled')).toBeInTheDocument()
    expect(screen.getByText('Claude Code')).toBeInTheDocument()
    expect(screen.getByText('gemini-cli')).toBeInTheDocument()
    expect(screen.getByText('prj_web')).toBeInTheDocument()
    expect(screen.getByTestId('space-harbor-pilot')).toHaveTextContent('run tokens last 24 h')
    expect(screen.getByTestId('space-harbor-pilot')).toHaveTextContent('launches are refused when budget status is unavailable')
    expect(screen.getByTestId('space-harbor-pilot')).toHaveTextContent('models coding/default')
    expect(screen.getByText('The parent token expires soon.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Settings' })).toBeInTheDocument()
  })

  it('offers no settings to viewers and says when the installation is not connected', () => {
    wrap(settings({ stored: false, enabled: false, connected: false, version: 0 }), me('viewer'))
    expect(screen.getByTestId('space-harbor-pilot')).toHaveTextContent('Not enabled: runs use the space')
    expect(screen.getByTestId('space-harbor-pilot')).toHaveTextContent('Harbor Pilot is not connected yet')
    expect(screen.queryByRole('button', { name: 'Settings' })).toBeNull()
  })

  it('lets only a global administrator set the project (it decides who pays); others see it read-only', () => {
    for (const [m, readOnly] of [[me('operator'), true], [me('admin'), true], [me('admin', '*'), false]] as const) {
      const { unmount } = wrap(settings(), m)
      fireEvent.click(screen.getByRole('button', { name: 'Settings' }))
      const input = screen.getByTestId('hp-project')
      expect(input).toHaveValue('prj_web')
      expect(input.hasAttribute('readonly')).toBe(readOnly)
      if (readOnly) expect(screen.getByText(/Only an administrator sets or changes the project/)).toBeInTheDocument()
      unmount()
    }
  })
})
