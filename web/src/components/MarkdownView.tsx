import { Box } from '@mantine/core'
import { useMemo } from 'react'
import { renderMarkdown } from '@/lib/markdown'
import './composer/composer.css'

/** Displays untrusted Markdown, sanitized, with the space's attachments resolved (SM-ATT-003, SM-UI-005). */
export function MarkdownView({ source, spaceId }: { source: string; spaceId: string }) {
  const html = useMemo(() => renderMarkdown(source, spaceId), [source, spaceId])
  // The HTML is produced by marked and sanitized by DOMPurify (scripts, handlers, styles and remote images removed).
  return <Box className="sm-markdown" fz="md" dangerouslySetInnerHTML={{ __html: html }} />
}
