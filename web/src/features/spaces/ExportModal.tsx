import { Alert, Button, Group, Loader, Modal, SimpleGrid, Stack, Text } from '@mantine/core'
import { IconAlertTriangle, IconDownload } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { api, unwrap } from '@/api/client'
import { ErrorAlert } from '@/components/ErrorAlert'
import { bytes } from '@/lib/format'

/** The URL of a space's export archive (a raw download route; the browser session authenticates it). */
export const exportURL = (spaceId: string) => `/api/v1/spaces/${encodeURIComponent(spaceId)}/export`

/**
 * Space export (ADR-0045): what the archive would hold, the space's export limit, the plaintext warning and the
 * download. Space admins only (space:export); the API enforces it.
 */
export function ExportModal({ spaceId, onClose }: { spaceId: string; onClose: () => void }) {
  const summary = useQuery({
    queryKey: ['spaces', spaceId, 'export-summary'],
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/export/summary', { params: { path: { space_id: spaceId } }, signal })),
  })
  const s = summary.data
  const count = (label: string, n: number) => (
    <div key={label}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text fw={600}>{n.toLocaleString('en-US')}</Text>
    </div>
  )
  return (
    <Modal opened onClose={onClose} title="Export this space" size="lg">
      <Stack gap="sm">
        <Text size="sm">
          One ZIP archive of the space's inbox items, tasks, human actions with their responses and comments, messages and attachments, as JSON
          Lines plus the attachment files, with a manifest of checksums. Recordings and transcripts are not included.
        </Text>
        <ErrorAlert error={summary.error} />
        {summary.isLoading && <Loader size="sm" />}
        {s && (
          <>
            <Alert color="owlYellow" variant="light" icon={<IconAlertTriangle size={16} />} title="The archive is not encrypted">
              <Text size="sm">{s.warning}</Text>
            </Alert>
            <SimpleGrid cols={{ base: 2, sm: 3 }}>
              {count('Inbox items', s.counts.inbox_items)}
              {count('Tasks', s.counts.tasks)}
              {count('Human actions', s.counts.human_actions)}
              {count('Comments', s.counts.human_comments)}
              {count('Messages', s.counts.messages)}
              {count('Attachments', s.counts.attachments)}
            </SimpleGrid>
            <Text size="sm">
              About <b>{bytes(s.estimated_bytes)}</b> uncompressed; this space's export limit is {bytes(s.max_bytes)}.
            </Text>
            {!s.within_limit && (
              <Text size="sm" c="owlRed" role="alert">
                The export is larger than the space's export limit (export_max_bytes in its limits) and would be refused.
              </Text>
            )}
          </>
        )}
        <Text size="xs" c="dimmed">Every export is audited. The archive is streamed from one consistent snapshot; central keeps no copy.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Close</Button>
          <Button component="a" href={exportURL(spaceId)} download leftSection={<IconDownload size={14} />} disabled={!s?.within_limit}
            onClick={(e: React.MouseEvent) => {
              if (!s?.within_limit) e.preventDefault()
            }}>
            Download archive
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
