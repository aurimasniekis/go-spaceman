import { describe, expect, it } from 'vitest'
import { parseStartParam } from './deeplink'

describe('parseStartParam', () => {
  it('maps the known kinds to a fixed route', () => {
    expect(parseStartParam('space-spc_abc')).toEqual({ to: '/spaces/$spaceId', params: { spaceId: 'spc_abc' } })
    expect(parseStartParam('inbox-spc_abc-inb_x1')).toEqual({ to: '/spaces/$spaceId/inbox/$itemId', params: { spaceId: 'spc_abc', itemId: 'inb_x1' } })
    expect(parseStartParam('run-spc_abc-run_x1')).toEqual({ to: '/spaces/$spaceId/runs/$runId', params: { spaceId: 'spc_abc', runId: 'run_x1' } })
    expect(parseStartParam('task-spc_abc-tsk_x1')).toEqual({ to: '/spaces/$spaceId/queue', params: { spaceId: 'spc_abc' }, search: { task: 'tsk_x1' } })
    expect(parseStartParam('human-spc_abc-hum_x1')).toEqual({ to: '/spaces/$spaceId/human', params: { spaceId: 'spc_abc' }, search: { action: 'hum_x1' } })
    expect(parseStartParam('attention')).toEqual({ to: '/attention' })
    expect(parseStartParam('needs')).toEqual({ to: '/human' })
  })
  it('ignores everything else', () => {
    for (const bad of [undefined, '', 'x', 'space', 'space-../etc', 'space-spc_a-extra', 'inbox-spc_a', 'run-spc_a-x', 'attention-spc_a', 'needs-spc_a', 'needs-spc_a-hum_x1', 'admin-spc_a-inb_x', 'a-b-c-d', 'space-spc_a/b', 'space-spc_a-', 'x'.repeat(300)]) {
      expect(parseStartParam(bad as string | undefined)).toBeNull()
    }
  })
})
