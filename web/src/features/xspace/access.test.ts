import { describe, expect, it } from 'vitest'
import type { XSpacePeer, XSpaceRequest } from '@/api/client'
import { accepts, actions, peerOf, sends } from './access'

const inbound: XSpacePeer['inbound'] = { discover: false, questions: false, delegation: false, knowledge: false, kbs: [], disclosure: 'none', max_per_hour: 20, max_pending: 5 }
const outbound: XSpacePeer['outbound'] = { questions: false, delegation: false, knowledge: false }

describe('cross-space access summaries', () => {
  it('lists each grant separately and nothing by default', () => {
    expect(accepts({ inbound })).toEqual([])
    expect(sends({ outbound })).toEqual([])
    expect(accepts({ inbound: { ...inbound, discover: true, knowledge: true, kbs: ['payments'] } })).toEqual(['discovery', 'knowledge (payments)'])
    expect(accepts({ inbound: { ...inbound, knowledge: true } })).toEqual(['knowledge (every KB)'])
    expect(sends({ outbound: { ...outbound, delegation: true } })).toEqual(['delegation'])
  })

  it('offers withdrawing outgoing and declining pending incoming requests only', () => {
    const base = { source: { id: 'spc_a' }, destination: { id: 'spc_b' } } as XSpaceRequest
    expect(actions({ ...base, side: 'outgoing', state: 'accepted' })).toEqual({ cancel: true, decline: false })
    expect(actions({ ...base, side: 'incoming', state: 'pending' })).toEqual({ cancel: false, decline: true })
    expect(actions({ ...base, side: 'incoming', state: 'accepted' })).toEqual({ cancel: false, decline: false })
    expect(actions({ ...base, side: 'outgoing', state: 'completed' })).toEqual({ cancel: false, decline: false })
    expect(peerOf({ ...base, side: 'incoming' }).id).toBe('spc_a')
    expect(peerOf({ ...base, side: 'outgoing' }).id).toBe('spc_b')
  })
})
