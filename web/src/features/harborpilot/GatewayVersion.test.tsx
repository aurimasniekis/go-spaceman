import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import type { HarborPilotGateway } from '@/api/client'
import { GatewayVersion } from './GatewayVersion'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

const gw = (extra: Partial<HarborPilotGateway>) => ({ source: 'system', min_version: '0.1.3', recommended_version: '0.1.4', project_filter: true, ...extra }) as HarborPilotGateway
const show = (g?: HarborPilotGateway, loading = false) => render(<MantineProvider><GatewayVersion g={g} loading={loading} /></MantineProvider>)

describe('Harbor Pilot version', () => {
  it('0.1.4: the version and commit from GET /api/v1/system', () => {
    show(gw({ version: '0.1.4', commit: '60d8d0236d187fb89d1216b7d7c1c177f214df49' }))
    expect(screen.getByTestId('harbor-pilot-version')).toHaveTextContent('0.1.4 · commit 60d8d0236d18')
  })

  it('0.1.3: older than 0.1.4, with the recommendation', () => {
    show(gw({ source: 'legacy', project_filter: false }))
    const v = screen.getByTestId('harbor-pilot-version')
    expect(v).toHaveTextContent('older than 0.1.4')
    expect(v).toHaveTextContent('At least 0.1.3')
    expect(v).toHaveTextContent('0.1.4 or later is recommended')
  })

  it('unknown: says why', () => {
    show(gw({ source: 'unknown', project_filter: false, unavailable: 'the Harbor Pilot connection has no service-principal token' }))
    expect(screen.getByTestId('harbor-pilot-version')).toHaveTextContent('no service-principal token')
  })

  it('while loading', () => {
    show(undefined, true)
    expect(screen.getByText('Reading…')).toBeInTheDocument()
  })
})
