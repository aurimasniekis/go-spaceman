import { Anchor, Card, Group, Image, SimpleGrid, Stack, Text } from '@mantine/core'
import { IconDownload, IconPaperclip } from '@tabler/icons-react'
import { useEffect, useRef } from 'react'
import type { Attachment } from '@/api/client'
import { useAttachmentsHost } from '@/components/attachmentsHost'
import { formatBytes } from '@/components/composer/upload'
import { attachmentURL } from '@/lib/markdown'

/**
 * An item's attachments (SM-ATT-001): raster images as previews, other files as downloads. Content is served by the
 * attachment route with the space's attachment permission and a sandboxing CSP (SM-ATT-004).
 */
export function AttachmentList({ spaceId, list, title = 'Attachments', cols = { base: 2, sm: 4 } }: {
  spaceId: string; list: Attachment[]; title?: string; cols?: { base: number; sm: number }
}) {
  // The Mini App (no cookie) loads previews as blobs behind a visibility gate and downloads through signed links.
  const host = useAttachmentsHost()
  const ref = useRef<HTMLDivElement>(null)
  const empty = list.length === 0
  useEffect(() => (host && ref.current && !empty ? host.watch(ref.current) : undefined), [host, empty, list])
  if (empty) return null
  const urlOf = (id: string) => (host ? host.resolver(spaceId)(id) : attachmentURL(spaceId, id))
  return (
    <Stack gap={6} ref={ref}>
      <Text size="sm" fw={600}>{title}</Text>
      <SimpleGrid cols={cols} spacing="xs">
        {list.map((a) => (
          <Card key={a.id} p="xs">
            {a.inline ? <Image src={urlOf(a.id)} alt={a.name} h={90} fit="contain" radius="sm" /> : <IconPaperclip size={24} aria-hidden />}
            <Group justify="space-between" wrap="nowrap" mt={4}>
              <Text size="xs" truncate title={a.name}>{a.name}</Text>
              <Anchor href={urlOf(a.id)} download={a.name} aria-label={`Download ${a.name}`}><IconDownload size={14} /></Anchor>
            </Group>
            <Text size="xs" c="dimmed">{formatBytes(a.size)} · {a.mime}</Text>
          </Card>
        ))}
      </SimpleGrid>
    </Stack>
  )
}
