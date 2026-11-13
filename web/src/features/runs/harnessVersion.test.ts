import { describe, expect, it } from 'vitest'
import { dependencyText, runHarnessVersion } from './harnessVersion'

describe('run harness version', () => {
  it('shows what ran, with the request and fallback only when they add something', () => {
    expect(runHarnessVersion({})).toBe('')
    expect(runHarnessVersion({ harness_version: '2.1.317', harness_requested: '2.1.317' })).toBe('2.1.317')
    expect(runHarnessVersion({ harness_version: '2.1.322', harness_requested: 'latest' })).toBe('2.1.322 (requested latest)')
    expect(runHarnessVersion({ harness_version: '2.1.320', harness_requested: 'latest', harness_fallback: 'previous_target' }))
      .toBe('2.1.320 (requested latest; fallback: previous target)')
  })

  it('renders old and new manifest dependencies', () => {
    expect(dependencyText('claude-code', '2.1.317')).toBe('claude-code 2.1.317')
    expect(dependencyText('node_protocol', 1)).toBe('node_protocol 1')
    expect(dependencyText('claude-code', { requested: 'latest', node_default: '2.1.322' })).toBe('claude-code requested latest, node default 2.1.322')
    expect(dependencyText('claude-code', { requested: '', node_default: '2.1.322' })).toBe('claude-code node default 2.1.322')
  })
})
