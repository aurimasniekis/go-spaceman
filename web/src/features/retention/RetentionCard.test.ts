import { describe, expect, it } from 'vitest'
import { deletedSummary } from './RetentionCard'
import { exportURL } from '../spaces/ExportModal'

describe('retention', () => {
  it('summarizes what a pass deleted', () => {
    expect(deletedSummary({})).toBe('nothing')
    expect(deletedSummary(undefined)).toBe('nothing')
    expect(deletedSummary({ recordings: 3, attachments: 0, audit_events: 12 })).toBe('3 recordings, 12 audit events')
  })
  it('builds the export download URL', () => {
    expect(exportURL('spc_01')).toBe('/api/v1/spaces/spc_01/export')
  })
})
