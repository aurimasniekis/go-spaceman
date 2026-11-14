import { MantineProvider } from '@mantine/core'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const renderMermaid = vi.hoisted(() => vi.fn())
vi.mock('@/lib/mermaid', () => ({ renderMermaid }))

import { MarkdownView } from './MarkdownView'
import { MermaidDiagram } from './markdown/MermaidDiagram'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  renderMermaid.mockReset()
})

const wrap = (ui: React.ReactNode) => render(<MantineProvider>{ui}</MantineProvider>)
const fence = (lang: string, body: string) => '```' + lang + '\n' + body + '\n```'

describe('MarkdownView', () => {
  it('highlights fenced code lazily and leaves plain blocks alone', async () => {
    const { container } = wrap(<MarkdownView source={`${fence('go', 'func main() {}')}\n\n${fence('', 'plain <b>')}`} spaceId="spc_1" />)
    await waitFor(() => expect(container.querySelector('code[data-lang="go"] .th-keyword')?.textContent).toBe('func'))
    const plain = container.querySelectorAll('.sm-codeblock code')[1]
    expect(plain.textContent).toBe('plain <b>')
    expect(plain.querySelector('.th-token')).toBeNull()
  })

  it('copies a code block with a keyboard-reachable button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    wrap(<MarkdownView source={fence('sh', 'make web-check')} spaceId="spc_1" />)
    const btn = screen.getByRole('button', { name: 'Copy code' })
    expect(btn.tagName).toBe('BUTTON')
    fireEvent.click(btn)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument())
    expect(writeText).toHaveBeenCalledWith('make web-check')
    // The code block itself scrolls by keyboard.
    expect(btn.parentElement?.querySelector('pre')?.getAttribute('tabindex')).toBe('0')
  })

  it('renders Mermaid fences as diagrams with their source as the text alternative', async () => {
    renderMermaid.mockResolvedValue('<svg data-testid="diagram"><g><text>A</text></g></svg>')
    wrap(<MarkdownView source={`Before\n\n${fence('mermaid', 'graph TD; A-->B')}\n\nAfter`} spaceId="spc_1" />)
    await waitFor(() => expect(screen.getByTestId('diagram')).toBeInTheDocument())
    expect(renderMermaid).toHaveBeenCalledWith('graph TD; A-->B', 'light')
    expect(screen.getByRole('img', { name: /Diagram/ })).toContainElement(screen.getByTestId('diagram'))
    expect(screen.getByText('Diagram source')).toBeInTheDocument()
    expect(screen.getByText('graph TD; A-->B', { selector: 'details code' })).toBeInTheDocument()
  })

  it('shows a short error and the source when a diagram fails, and the rest of the body still renders', async () => {
    renderMermaid.mockRejectedValue(new Error('Parse error on line 1:\ngraph TD; A-->\n----^\nExpecting NODE'))
    wrap(<MarkdownView source={`${fence('mermaid', 'graph TD; A-->')}\n\n**still here**`} spaceId="spc_1" />)
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent("Couldn't render this diagram: Parse error on line 1: graph TD; A-->"))
    expect(screen.getByRole('status').textContent).not.toContain('Expecting NODE')
    expect(document.querySelector('details.sm-mermaid-source')).toHaveAttribute('open')
    expect(screen.getByText('still here').tagName).toBe('STRONG')
  })

  it('renders at most 20 diagrams per body; the rest show their source', async () => {
    renderMermaid.mockResolvedValue('<svg></svg>')
    const src = Array.from({ length: 25 }, (_, i) => fence('mermaid', `graph TD; N${i}-->M`)).join('\n\n')
    const { container } = wrap(<MarkdownView source={src} spaceId="spc_1" />)
    await waitFor(() => expect(renderMermaid).toHaveBeenCalledTimes(20))
    expect(container.querySelectorAll('[data-sm-mermaid][data-sm-mounted]')).toHaveLength(20)
    expect(container.querySelectorAll('[data-sm-mermaid]:not([data-sm-mounted]) > pre')).toHaveLength(5)
  })

  it('updates when the body changes', async () => {
    renderMermaid.mockResolvedValue('<svg data-testid="d2"></svg>')
    const { rerender, container } = wrap(<MarkdownView source="first" spaceId="spc_1" />)
    rerender(<MantineProvider><MarkdownView source={fence('mermaid', 'graph LR; X-->Y')} spaceId="spc_1" /></MantineProvider>)
    await waitFor(() => expect(screen.getByTestId('d2')).toBeInTheDocument())
    rerender(<MantineProvider><MarkdownView source="third" spaceId="spc_1" /></MantineProvider>)
    await act(async () => {})
    expect(container.textContent).toContain('third')
    expect(screen.queryByTestId('d2')).toBeNull()
  })
})

describe('MermaidDiagram', () => {
  it('works on its own and refuses oversized sources without loading mermaid', async () => {
    wrap(<MermaidDiagram source={'graph TD\n' + 'A-->B\n'.repeat(3000)} />)
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('longer than 10000 characters'))
    expect(renderMermaid).not.toHaveBeenCalled()
  })

  it('uses the dark theme in dark mode', async () => {
    renderMermaid.mockResolvedValue('<svg></svg>')
    render(<MantineProvider forceColorScheme="dark"><MermaidDiagram source="graph TD; A-->B" /></MantineProvider>)
    await waitFor(() => expect(renderMermaid).toHaveBeenCalledWith('graph TD; A-->B', 'dark'))
  })
})
