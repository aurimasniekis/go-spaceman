import { referenceAttachments, referencedAttachments, renderMarkdown, resolveAttachments } from './markdown'

const id = 'att_01m6e3fshfe1d8efqs2sd4hyma'

describe('markdown', () => {
  it('round-trips attachment references and content URLs', () => {
    const md = `![shot](spaceman://attachment/${id}) and [log](spaceman://attachment/${id})`
    const shown = resolveAttachments(md, 'spc_1')
    expect(shown).toContain(`/api/v1/spaces/spc_1/attachments/${id}/content`)
    expect(referenceAttachments(shown, 'spc_1')).toBe(md)
    expect(referenceAttachments(`![x](http://127.0.0.1:38180/api/v1/spaces/spc_1/attachments/${id}/content)`, 'spc_1')).toBe(`![x](spaceman://attachment/${id})`)
    // Another space's URL is left alone.
    expect(referenceAttachments(`/api/v1/spaces/spc_2/attachments/${id}/content`, 'spc_1')).not.toContain('spaceman://')
    expect(referencedAttachments(md)).toEqual([id])
  })

  it('removes scripts, handlers, styles and remote images', () => {
    const html = renderMarkdown(
      '<script>alert(1)</script><img src=x onerror=alert(1)><p style="color:red" onclick="x()">hi</p>\n\n![t](https://evil.example/pixel.gif)\n\n[a](javascript:alert(1))',
      'spc_1',
    )
    expect(html).not.toMatch(/<script|onerror|onclick|style=|javascript:/i)
    expect(html).not.toContain('<img src="https://evil.example')
    expect(html).toContain('href="https://evil.example/pixel.gif"')
  })

  it('renders attachment images, task lists and tables', () => {
    const html = renderMarkdown(`![shot](spaceman://attachment/${id})\n\n- [x] done\n- [ ] todo\n\n| a | b |\n|---|---|\n| 1 | 2 |`, 'spc_1')
    expect(html).toContain(`<img src="/api/v1/spaces/spc_1/attachments/${id}/content"`)
    expect(html).toContain('type="checkbox"')
    expect(html).toContain('<table>')
  })

  it('opens links in a new tab without a referrer', () => {
    expect(renderMarkdown('[docs](https://example.com)', 'spc_1')).toContain('rel="noopener noreferrer nofollow"')
  })
})

describe('task lists', () => {
  it('names and disables task checkboxes', () => {
    const html = renderMarkdown('- [ ] Confirm the window\n- [x] Draft the plan', 'spc_a')
    expect(html).toContain('aria-label="Confirm the window"')
    expect(html).toContain('aria-label="Draft the plan"')
    expect(html.match(/disabled/g)?.length).toBe(2)
  })
})
