import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { acceptingWrites, SystemView, uptime, type SystemStatus } from './SystemView'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

function status(extra: Partial<SystemStatus> = {}): SystemStatus {
  return {
    build: { version: 'v0.1.0', commit: '0123456789abcdef0123', date: '2026-11-08T10:00:00Z', go_version: 'go1.27.3', platform: 'linux/amd64' },
    started_at: '2026-11-08T11:00:00Z', uptime_seconds: 3720, boot_id: 'boot_01abc', public_url: 'https://spaceman.example.com', data_dir: '/var/lib/spaceman',
    schema: { current: 44, latest: 44 }, protocol: { major: 1, min_minor: 0, max_minor: 9 },
    storage: { readiness: 'ready', checks: [{ name: 'store', status: 'ok' }, { name: 'store.writable', status: 'ok' }], store_state: 'ok', disk_total_bytes: 100 * 2 ** 30,
      disk_available_bytes: 40 * 2 ** 30, disk_used_percent: 60, disk_reserve_bytes: 2 ** 30, below_reserve: false, problem: false, db_bytes: 3 * 2 ** 20, wal_bytes: 2 ** 20 },
    counts: { spaces: 3, nodes_active: 2, nodes_connected: 1, runs_running: 4, human_pending: 5, live_streams: 6 },
    ...extra,
  } as SystemStatus
}

const wrap = (s: SystemStatus) => render(<MantineProvider><SystemView s={s} /></MantineProvider>)

describe('System page', () => {
  it('shows health, versions, counts and where the instance runs', () => {
    wrap(status())
    const st = screen.getByTestId('system-status')
    expect(st).toHaveTextContent('ready')
    expect(st).toHaveTextContent('Accepting writes')
    expect(st).toHaveTextContent('v0.1.0')
    expect(st).toHaveTextContent('0123456789ab')
    expect(st).not.toHaveTextContent('0123456789abcdef')
    expect(st).toHaveTextContent('boot_01abc')
    expect(st).toHaveTextContent('up 1h 2m')
    expect(st).toHaveTextContent('1.0 – 1.9')
    expect(st).not.toHaveTextContent('binary has')
    const counts = screen.getByTestId('system-counts')
    expect(counts).toHaveTextContent('1 of 2')
    expect(counts).toHaveTextContent('Live streams6')
    const inst = screen.getByTestId('system-instance')
    expect(inst).toHaveTextContent('https://spaceman.example.com')
    expect(inst).toHaveTextContent('/var/lib/spaceman')
    expect(inst).toHaveTextContent('3.0 MiB + 1.0 MiB WAL')
    expect(inst).toHaveTextContent('40.0 GiB free of 100.0 GiB')
  })

  it('flags a store that refuses writes, a schema behind the binary and an unreadable disk', () => {
    const base = status()
    wrap(status({
      schema: { current: 43, latest: 44 },
      storage: { ...base.storage, readiness: 'failed', store_state: 'failed', store_reason: 'disk I/O error', checks: [{ name: 'store.writable', status: 'failed', error: 'not_writable' }],
        disk_error: 'unavailable', problem: true },
    }))
    const st = screen.getByTestId('system-status')
    expect(st).toHaveTextContent('Not accepting writes')
    expect(st).toHaveTextContent('disk I/O error')
    expect(st).toHaveTextContent('binary has 44')
    expect(screen.getByTestId('system-instance')).toHaveTextContent('free space unavailable')
  })

  it('formats uptime and reads the writable check', () => {
    expect(uptime(0)).toBe('0s')
    expect(uptime(65)).toBe('1m 5s')
    expect(uptime(3 * 3600 + 20 * 60)).toBe('3h 20m')
    expect(uptime(2 * 86400 + 4 * 3600 + 5)).toBe('2d 4h')
    expect(acceptingWrites(status().storage)).toBe(true)
    expect(acceptingWrites({ ...status().storage, checks: [] })).toBeUndefined()
  })
})
