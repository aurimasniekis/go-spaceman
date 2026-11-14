import { beforeEach, describe, expect, it, vi } from 'vitest'

const m = vi.hoisted(() => ({ initialize: vi.fn(), render: vi.fn() }))
vi.mock('mermaid', () => ({ default: m }))

import { mermaidConfig, renderMermaid } from './mermaid'

beforeEach(() => {
  m.initialize.mockReset()
  m.render.mockReset()
})

describe('mermaid', () => {
  it('renders in strict mode with capped input and the app theme', async () => {
    m.render.mockResolvedValue({ svg: '<svg><g><text>A</text></g></svg>' })
    expect(await renderMermaid('graph TD; A-->B', 'dark')).toContain('<text>A</text>')
    const cfg = m.initialize.mock.calls[0][0]
    expect(cfg).toMatchObject({ startOnLoad: false, securityLevel: 'strict', suppressErrorRendering: true, maxTextSize: 10_000, maxEdges: 500, theme: 'base', darkMode: true })
    expect(cfg.themeVariables.background).toBe('#0b2942')
    expect(mermaidConfig('light').darkMode).toBe(false)
    expect(mermaidConfig('light').securityLevel).toBe('strict')
  })

  it("keeps a diagram's %%{init}%% and front matter from changing the theme or the limits", () => {
    const secure = mermaidConfig('dark').secure
    for (const k of ['secure', 'securityLevel', 'startOnLoad', 'maxTextSize', 'suppressErrorRendering', 'maxEdges', 'theme', 'themeVariables', 'themeCSS', 'darkMode', 'fontFamily']) {
      expect(secure).toContain(k)
    }
  })

  it('sanitizes the SVG mermaid returns', async () => {
    m.render.mockResolvedValue({
      svg: '<svg onload="alert(1)"><script>alert(1)</script><style>.a{fill:red}</style><a href="javascript:alert(1)"><text>link</text></a>' +
        '<foreignObject><div onclick="alert(1)"><img src=x onerror="alert(1)"><span>label</span></div></foreignObject>' +
        '<image href="https://evil.example/pixel.png"/><animate attributeName="href" to="javascript:alert(1)"/></svg>',
    })
    const svg = await renderMermaid('graph TD; A-->B', 'light')
    expect(svg).not.toMatch(/<script|onload|onclick|onerror|javascript:|<image|<animate|<a |<img/i)
    expect(svg).toContain('<span>label</span>')
    expect(svg).toContain('<style>')
    expect(svg).toContain('link')
  })

  it('rejects oversized sources without calling mermaid', async () => {
    await expect(renderMermaid('graph TD\n' + 'A-->B\n'.repeat(5000), 'light')).rejects.toThrow(/longer than 10000/)
    // 4,000 bare node IDs fit in 10,000 characters but took 7 seconds: the statement count is capped too.
    await expect(renderMermaid('graph TD;' + Array.from({ length: 1000 }, (_, i) => `N${i}`).join(';'), 'light')).rejects.toThrow(/more than 500 statements/)
    await expect(renderMermaid('graph TD\n' + Array.from({ length: 600 }, (_, i) => `N${i}`).join(' & '), 'light')).rejects.toThrow(/more than 500 statements/)
    expect(m.render).not.toHaveBeenCalled()
  })

  it('removes images from HTML labels, whatever their URL', async () => {
    m.render.mockResolvedValue({
      svg: '<svg><foreignObject><div><img src="/api/v1/auth/logout"><img src="https://tracker.example/p.gif" srcset="https://tracker.example/p2.gif 2x"><span>label</span></div></foreignObject></svg>',
    })
    const svg = await renderMermaid('graph TD; A["<img src=/api/v1/auth/logout>"]', 'light')
    expect(svg).not.toMatch(/<img|srcset|tracker|logout/i)
    expect(svg).toContain('<span>label</span>')
  })

  it('skips a queued render whose view went away before its turn', async () => {
    let release!: () => void
    m.render.mockImplementationOnce(() => new Promise((r) => (release = () => r({ svg: '<svg></svg>' }))))
    m.render.mockResolvedValue({ svg: '<svg><text>later</text></svg>' })
    const first = renderMermaid('graph TD; A-->B', 'light')
    const ctl = new AbortController()
    const gone = renderMermaid('graph TD; C-->D', 'light', ctl.signal)
    const kept = renderMermaid('graph TD; E-->F', 'light', new AbortController().signal)
    await vi.waitFor(() => expect(m.render).toHaveBeenCalledTimes(1))
    ctl.abort()
    release()
    await expect(first).resolves.toContain('<svg')
    await expect(gone).rejects.toMatchObject({ name: 'AbortError' })
    await expect(kept).resolves.toContain('later')
    expect(m.render).toHaveBeenCalledTimes(2)
    expect(m.render.mock.calls.map((c) => c[1])).toEqual(['graph TD; A-->B', 'graph TD; E-->F'])
  })

  it('removes what a failed render leaves behind and keeps rendering', async () => {
    m.render.mockImplementationOnce(async (id: string) => {
      const d = document.createElement('div')
      d.id = `d${id}`
      document.body.append(d)
      throw new Error('Parse error on line 1:\ngraph TD; A-->\n-----^\nExpecting NODE')
    })
    await expect(renderMermaid('graph TD; A-->', 'light')).rejects.toThrow(/Parse error/)
    expect(document.querySelector('[id^="dsm-mermaid-"]')).toBeNull()
    m.render.mockResolvedValue({ svg: '<svg></svg>' })
    await expect(renderMermaid('graph TD; A-->B', 'light')).resolves.toContain('<svg')
  })

  it('renders one diagram at a time', async () => {
    let active = 0
    let most = 0
    m.render.mockImplementation(async () => {
      most = Math.max(most, ++active)
      await new Promise((r) => setTimeout(r, 5))
      active--
      return { svg: '<svg></svg>' }
    })
    await Promise.all([renderMermaid('a', 'light'), renderMermaid('b', 'dark'), renderMermaid('c', 'light')])
    expect(most).toBe(1)
  })
})
