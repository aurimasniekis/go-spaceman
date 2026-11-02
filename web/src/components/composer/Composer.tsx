import { ActionIcon, Badge, Box, Divider, Group, Loader, Stack, Text, Tooltip } from '@mantine/core'
import {
  IconBold, IconCode, IconH2, IconItalic, IconLink, IconList, IconListCheck, IconListNumbers, IconPaperclip, IconQuote, IconSourceCode,
  IconStrikethrough, IconTable, IconX,
} from '@tabler/icons-react'
import FileHandler from '@tiptap/extension-file-handler'
import Image from '@tiptap/extension-image'
import { TaskItem, TaskList } from '@tiptap/extension-list'
import { TableKit } from '@tiptap/extension-table'
import { Placeholder } from '@tiptap/extensions'
import { Markdown } from '@tiptap/markdown'
import { EditorContent, useEditor, useEditorState, type Editor } from '@tiptap/react'
import StarterKit from '@tiptap/starter-kit'
import { useEffect, useRef, useState } from 'react'
import { errorMessage } from '@/components/ErrorAlert'
import { referenceAttachments, referencedAttachments, resolveAttachments } from '@/lib/markdown'
import { formatBytes, uploadAttachment, type UploadedAttachment } from './upload'
import './composer.css'

export interface ComposerValue {
  /** Markdown with spaceman://attachment/<id> references (what the API stores). */
  markdown: string
  /** Every attachment the body references or that is attached as a file. */
  attachmentIds: string[]
}

interface Props {
  spaceId: string
  initialMarkdown?: string
  placeholder?: string
  onChange: (v: ComposerValue) => void
  /** Ctrl/Cmd+Enter */
  onSubmit?: () => void
  minHeight?: number
  ariaLabel?: string
}

function Tool({ label, icon: Icon, active, onClick, disabled }: { label: string; icon: typeof IconBold; active?: boolean; onClick: () => void; disabled?: boolean }) {
  return (
    <Tooltip label={label}>
      <ActionIcon aria-label={label} aria-pressed={active} variant={active ? 'light' : 'subtle'} color={active ? undefined : 'gray'} onClick={onClick} disabled={disabled}>
        <Icon size={15} />
      </ActionIcon>
    </Tooltip>
  )
}

function Toolbar({ editor, onAttach }: { editor: Editor; onAttach: () => void }) {
  const s = useEditorState({
    editor,
    selector: ({ editor: e }) => ({
      bold: e.isActive('bold'), italic: e.isActive('italic'), strike: e.isActive('strike'), code: e.isActive('code'), h2: e.isActive('heading', { level: 2 }),
      bullet: e.isActive('bulletList'), ordered: e.isActive('orderedList'), task: e.isActive('taskList'), quote: e.isActive('blockquote'),
      codeBlock: e.isActive('codeBlock'), link: e.isActive('link'),
    }),
  })
  const chain = () => editor.chain().focus()
  const setLink = () => {
    const prev = editor.getAttributes('link').href as string | undefined
    const url = window.prompt('Link URL', prev ?? 'https://')
    if (url === null) return
    if (url === '') chain().extendMarkRange('link').unsetLink().run()
    else chain().extendMarkRange('link').setLink({ href: url }).run()
  }
  return (
    <Group gap={2} px={6} py={4} className="sm-composer-toolbar" role="toolbar" aria-label="Formatting">
      <Tool label="Bold (⌘B)" icon={IconBold} active={s.bold} onClick={() => chain().toggleBold().run()} />
      <Tool label="Italic (⌘I)" icon={IconItalic} active={s.italic} onClick={() => chain().toggleItalic().run()} />
      <Tool label="Strikethrough" icon={IconStrikethrough} active={s.strike} onClick={() => chain().toggleStrike().run()} />
      <Tool label="Inline code" icon={IconCode} active={s.code} onClick={() => chain().toggleCode().run()} />
      <Divider orientation="vertical" mx={2} />
      <Tool label="Heading" icon={IconH2} active={s.h2} onClick={() => chain().toggleHeading({ level: 2 }).run()} />
      <Tool label="Bulleted list" icon={IconList} active={s.bullet} onClick={() => chain().toggleBulletList().run()} />
      <Tool label="Numbered list" icon={IconListNumbers} active={s.ordered} onClick={() => chain().toggleOrderedList().run()} />
      <Tool label="Task list" icon={IconListCheck} active={s.task} onClick={() => chain().toggleTaskList().run()} />
      <Tool label="Quote" icon={IconQuote} active={s.quote} onClick={() => chain().toggleBlockquote().run()} />
      <Tool label="Code block" icon={IconSourceCode} active={s.codeBlock} onClick={() => chain().toggleCodeBlock().run()} />
      <Tool label="Table" icon={IconTable} onClick={() => chain().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run()} />
      <Tool label="Link" icon={IconLink} active={s.link} onClick={setLink} />
      <Divider orientation="vertical" mx={2} />
      <Tool label="Attach files (or paste / drop them)" icon={IconPaperclip} onClick={onAttach} />
    </Group>
  )
}

/**
 * The Markdown composer for items and replies (SM-ATT-003): TipTap with open-source extensions only, serialized to
 * Markdown (never HTML). Pasted, dropped or attached files are uploaded to the space; images are placed inline, other
 * files as links and listed below the editor.
 */
export function Composer({ spaceId, initialMarkdown = '', placeholder = 'Write in Markdown, paste or drop images and files…', onChange, onSubmit, minHeight = 140, ariaLabel = 'Message' }: Props) {
  const [files, setFiles] = useState<UploadedAttachment[]>([])
  const [uploading, setUploading] = useState(0)
  const [error, setError] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  const filesRef = useRef(files)
  filesRef.current = files
  const submitRef = useRef(onSubmit)
  submitRef.current = onSubmit

  const emit = (e: Editor) => {
    const markdown = referenceAttachments(e.getMarkdown(), spaceId)
    const ids = new Set([...referencedAttachments(markdown), ...filesRef.current.map((f) => f.id)])
    onChange({ markdown, attachmentIds: [...ids] })
  }

  const upload = async (e: Editor, list: File[], pos?: number) => {
    setError('')
    for (const file of list) {
      setUploading((n) => n + 1)
      try {
        const att = await uploadAttachment(spaceId, file)
        // Insert after the current selection, never over it: an inserted image stays selected, and the next file would
        // otherwise replace it.
        const chain = e.chain().focus().setTextSelection(pos ?? e.state.selection.to)
        if (att.inline) {
          chain.setImage({ src: att.url, alt: att.name }).run()
        } else {
          setFiles((f) => [...f, att])
          chain.insertContent({ type: 'text', text: att.name, marks: [{ type: 'link', attrs: { href: att.url } }] }).insertContent(' ').run()
        }
        emit(e)
      } catch (err) {
        setError(`${file.name}: ${errorMessage(err)}`)
      } finally {
        setUploading((n) => n - 1)
      }
    }
  }
  const uploadRef = useRef(upload)
  uploadRef.current = upload

  const editor = useEditor({
    extensions: [
      StarterKit.configure({ link: { openOnClick: false, autolink: true, protocols: ['http', 'https', 'mailto'] } }),
      Image.configure({ inline: false, allowBase64: false }),
      TaskList,
      TaskItem.configure({ nested: true }),
      TableKit.configure({ table: { resizable: false } }),
      Markdown,
      Placeholder.configure({ placeholder }),
      FileHandler.configure({
        onPaste: (e, list) => void uploadRef.current(e, list),
        onDrop: (e, list, pos) => void uploadRef.current(e, list, pos),
      }),
    ],
    content: resolveAttachments(initialMarkdown, spaceId),
    contentType: 'markdown',
    editorProps: {
      attributes: { 'aria-label': ariaLabel, 'aria-multiline': 'true', role: 'textbox', class: 'sm-composer-content' },
      handleKeyDown: (_view, event) => {
        if (event.key === 'Enter' && (event.metaKey || event.ctrlKey) && submitRef.current) {
          submitRef.current()
          return true
        }
        return false
      },
    },
    onUpdate: ({ editor: e }) => emit(e),
    immediatelyRender: true,
  })

  // Removing a file chip drops it from the attachment list (its link text stays editable in the body).
  useEffect(() => {
    if (editor) emit(editor)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [files])

  if (!editor) return null
  return (
    <Stack gap={4}>
      <Box className="sm-composer">
        <Toolbar editor={editor} onAttach={() => fileInput.current?.click()} />
        <EditorContent editor={editor} style={{ minHeight }} />
        <input
          ref={fileInput}
          type="file"
          multiple
          hidden
          aria-hidden
          onChange={(e) => {
            const list = Array.from(e.target.files ?? [])
            e.target.value = ''
            if (list.length) void upload(editor, list)
          }}
        />
      </Box>
      <Group gap={6}>
        {files.map((f) => (
          <Badge key={f.id} variant="outline" color="gray" size="md" radius="sm" rightSection={
            <ActionIcon size="xs" aria-label={`Remove ${f.name}`} onClick={() => setFiles((l) => l.filter((x) => x.id !== f.id))}><IconX size={10} /></ActionIcon>
          }>
            <IconPaperclip size={10} /> {f.name} · {formatBytes(f.size)}
          </Badge>
        ))}
        {uploading > 0 && <Group gap={4}><Loader size={12} /><Text size="xs" c="dimmed">Uploading…</Text></Group>}
      </Group>
      {error && <Text size="xs" c="owlRed" role="alert">{error}</Text>}
    </Stack>
  )
}
