import { Text } from '@mantine/core'
import type { HarborPilotGateway } from '@/api/client'
import { gatewayVersionLabel } from './format'

/** The gateway's version (read with the service-principal token), and a hint when it is older than recommended. */
export function GatewayVersion({ g, loading }: { g?: HarborPilotGateway; loading?: boolean }) {
  if (!g) return <Text size="sm" c="dimmed">{loading ? 'Reading…' : 'unknown'}</Text>
  return (
    <div data-testid="harbor-pilot-version">
      <Text size="sm">
        <span className="mono">{gatewayVersionLabel(g)}</span>
        {g.source === 'system' && g.commit && <Text span size="xs" c="dimmed"> · commit <span className="mono">{g.commit.slice(0, 12)}</span></Text>}
      </Text>
      {g.source === 'legacy' && (
        <Text size="xs" c="dimmed">At least {g.min_version} (the connection check showed it). {g.recommended_version} or later is recommended: SpaceMan then reads only the budgets covering each space.</Text>
      )}
      {g.source === 'unknown' && <Text size="xs" c="dimmed">{g.unavailable || 'It could not be read.'}</Text>}
      {g.source === 'system' && !g.project_filter && <Text size="xs" c="dimmed">Budget status is read in full.</Text>}
    </div>
  )
}
