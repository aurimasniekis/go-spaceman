import { MantineProvider } from '@mantine/core'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import type { Node } from '@/api/client'
import { capabilitiesStale, NodeHarnesses } from './NodeHarnesses'

const NOW = Date.parse('2026-11-13T12:00:00Z')

beforeEach(() => {
  // jsdom has no matchMedia (Mantine's color scheme reads it).
  window.matchMedia = ((q: string) => ({
    matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
})

function node(p: Partial<Node>, harnesses: Node['capabilities']['harnesses']): Node {
  return {
    id: 'nod_1', name: 'n1', description: '', state: 'active', labels: {}, protocol_minor: 11, capacity: 4, connected: true, version: 1,
    created_at: '2026-11-01T00:00:00Z', capabilities_updated_at: '2026-11-13T11:50:00Z',
    capabilities: { os: 'linux', arch: 'amd64', features: [], runner: { kind: 'herdr-core', version: '0.4.0', core_id: 'c' }, harnesses },
    ...p,
  } as Node
}

function show(n: Node) {
  return render(<MantineProvider><NodeHarnesses node={n} now={NOW} /></MantineProvider>)
}

describe('node harnesses', () => {
  it('shows a managed harness: channel, resolved targets with times, cached versions and the cache size', () => {
    show(node({}, [{
      kind: 'claude-code', available: true, version: '2.1.322', path: '/data/harness/claude-code/2.1.322/claude', capabilities: ['resume'],
      managed: true, channel: 'latest', cached: ['2.1.322', '2.1.320'], cache_bytes: 3 * 1024 * 1024 * 1024 / 2,
      resolved: { latest: { version: '2.1.322', resolved_at: '2026-11-13T11:00:00Z' }, stable: { version: '2.1.300' } },
    }]))
    const card = within(screen.getByLabelText('Harness claude-code'))
    expect(card.getByText('managed by the node')).toBeTruthy()
    expect(card.getByText('latest')).toBeTruthy()
    expect(card.getAllByText('2.1.322').length).toBeGreaterThanOrEqual(3) // new runs, resolved latest, cached
    expect(card.getByText('1h ago')).toBeTruthy()
    expect(card.getByText('2.1.300')).toBeTruthy()
    expect(card.getByText('2.1.320')).toBeTruthy()
    expect(card.getByText('1.5 GiB')).toBeTruthy()
    expect(screen.getByText('10m ago')).toBeTruthy()
    expect(screen.queryByText('stale')).toBeNull()
  })

  it('shows an unmanaged harness as detected, and a stale report', () => {
    show(node({ capabilities_updated_at: '2026-11-13T09:00:00Z' }, [
      { kind: 'claude-code', available: true, version: '2.1.5', path: '/usr/local/bin/claude', capabilities: [] },
      { kind: 'codex', available: false, error: 'not installed', capabilities: [] },
    ]))
    const cc = within(screen.getByLabelText('Harness claude-code'))
    expect(cc.getByText('detected')).toBeTruthy()
    expect(cc.getByText('2.1.5')).toBeTruthy()
    expect(cc.getByText('/usr/local/bin/claude')).toBeTruthy()
    expect(cc.queryByText('Cache size')).toBeNull()
    expect(within(screen.getByLabelText('Harness codex')).getByText('not installed')).toBeTruthy()
    expect(screen.getByText('3h ago')).toBeTruthy()
    expect(screen.getByText('stale')).toBeTruthy()
  })

  it('a managed node with nothing cached yet says so', () => {
    show(node({}, [{ kind: 'claude-code', available: false, error: 'managed: no version cached yet', capabilities: [], managed: true, channel: 'stable' }]))
    const card = within(screen.getByLabelText('Harness claude-code'))
    expect(card.getByText('nothing cached yet')).toBeTruthy()
    expect(card.getAllByText('not resolved')).toHaveLength(2)
    expect(card.getByText('none')).toBeTruthy()
  })

  it('is stale only after an hour, and never for a node that never reported', () => {
    expect(capabilitiesStale({ capabilities_updated_at: '2026-11-13T11:00:01Z' }, NOW)).toBe(false)
    expect(capabilitiesStale({ capabilities_updated_at: '2026-11-13T10:59:59Z' }, NOW)).toBe(true)
    expect(capabilitiesStale({}, NOW)).toBe(false)
    expect(capabilitiesStale({ capabilities_updated_at: '0001-01-01T00:00:00Z' }, NOW)).toBe(false)
  })
})
