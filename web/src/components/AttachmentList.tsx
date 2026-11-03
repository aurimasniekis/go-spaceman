import { Anchor, Card, Group, Image, SimpleGrid, Stack, Text } from '@mantine/core'
import { IconDownload, IconPaperclip } from '@tabler/icons-react'
import type { Attachment } from '@/api/client'
import { formatBytes } from '@/components/composer/upload'
import { attachmentURL } from '@/lib/markdown'

/**
 * An item's attachments (SM-ATT-001): raster images as previews, other files as downloads. Content is served by the
 * attachment route with the space's attachment permission and a sandboxing CSP (SM-ATT-004).
 */
export function AttachmentList({ spaceId, list, title = 'Attachments', cols = { base: 2, sm: 4 } }: {
  spaceId: string; list: Attachment[]; title?: string; cols?: { base: number; sm: number }
}) {
  if (list.length === 0) return null
  return (
    <Stack gap={6}>
      <Text size="sm" fw={600}>{title}</Text>
      <SimpleGrid cols={cols} spacing="xs">
        {list.map((a) => (
          <Card key={a.id} p="xs">
            {a.inline ? <Image src={attachmentURL(spaceId, a.id)} alt={a.name} h={90} fit="contain" radius="sm" /> : <IconPaperclip size={24} aria-hidden />}
            <Group justify="space-between" wrap="nowrap" mt={4}>
              <Text size="xs" truncate title={a.name}>{a.name}</Text>
              <Anchor href={attachmentURL(spaceId, a.id)} download={a.name} aria-label={`Download ${a.name}`}><IconDownload size={14} /></Anchor>
            </Group>
            <Text size="xs" c="dimmed">{formatBytes(a.size)} · {a.mime}</Text>
          </Card>
        ))}
      </SimpleGrid>
    </Stack>
  )
}
