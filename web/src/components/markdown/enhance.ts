import { MARKDOWN_LIMITS } from '@/lib/markdown'

const PLAIN = new Set(['', 'text', 'txt', 'plaintext', 'plain'])

let highlightModule: Promise<typeof import('@/lib/highlight')> | undefined
/** Loads lib/highlight once per page (one import, however many views need it). */
const loadHighlight = () => (highlightModule ??= import('@/lib/highlight').catch((e: unknown) => {
    highlightModule = undefined // a failed chunk load is retried next time
    throw e
  }))

const yieldToMain = () => new Promise<void>((r) => setTimeout(r, 0))

/** Copies text to the clipboard; without the Clipboard API (an insecure origin) it selects the text instead. */
async function copyText(text: string, el: HTMLElement): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    const range = el.ownerDocument.createRange()
    range.selectNodeContents(el)
    const sel = el.ownerDocument.getSelection()
    sel?.removeAllRanges()
    sel?.addRange(range)
    return false
  }
}

function addCopyButton(block: HTMLElement) {
  const code = block.querySelector('code')
  if (!code || block.querySelector(':scope > .sm-copy')) return
  const btn = block.ownerDocument.createElement('button')
  btn.type = 'button'
  btn.className = 'sm-copy'
  btn.textContent = 'Copy'
  btn.setAttribute('aria-label', 'Copy code')
  let timer: ReturnType<typeof setTimeout> | undefined
  btn.addEventListener('click', () => {
    void copyText(code.textContent ?? '', code).then((ok) => {
      btn.textContent = ok ? 'Copied' : 'Press Ctrl+C'
      btn.setAttribute('aria-label', ok ? 'Copied' : 'Code selected: press Ctrl+C to copy')
      clearTimeout(timer)
      timer = setTimeout(() => {
        btn.textContent = 'Copy'
        btn.setAttribute('aria-label', 'Copy code')
      }, 1500)
    })
  })
  block.append(btn)
}

/**
 * Adds copy buttons to a rendered body's code blocks and highlights the ones with a language. The highlighter and each
 * grammar load lazily, once per page; blocks are highlighted one at a time, yielding between them, and the limits leave
 * very long or very many blocks plain. Returns a function that stops the work (the body changed or unmounted).
 */
export function enhanceCode(root: HTMLElement): () => void {
  let stopped = false
  const blocks = Array.from(root.querySelectorAll<HTMLElement>('.sm-codeblock'))
  blocks.forEach(addCopyButton)
  const todo = blocks
    .map((b) => b.querySelector<HTMLElement>('code[data-lang]'))
    .filter((c): c is HTMLElement => !!c && !PLAIN.has(c.dataset.lang ?? '') && !c.querySelector('.th-token') && (c.textContent ?? '').length <= MARKDOWN_LIMITS.codeLength)
    .slice(0, MARKDOWN_LIMITS.codeBlocks)
  if (todo.length) {
    void (async () => {
      const hl = await loadHighlight()
      for (const code of todo) {
        if (stopped) return
        const tokens = await hl.tokenize(code.textContent ?? '', code.dataset.lang ?? '')
        if (stopped) return
        if (tokens) hl.paint(code, tokens)
        await yieldToMain()
      }
    })().catch(() => undefined) // Highlighting is cosmetic: on failure the block stays plain.
  }
  return () => {
    stopped = true
  }
}
