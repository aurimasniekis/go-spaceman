import { diagramStatements, limitNesting, planDiagrams, referenceAttachments, referencedAttachments, renderMarkdown, resolveAttachments } from './markdown'

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

describe('GFM features', () => {
  it('renders strikethrough, autolinks, tables in a scroll block and footnotes', () => {
    const html = renderMarkdown('~~old~~ see https://example.com/x\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nClaim[^1].\n\n[^1]: Source **here**.', 'spc_1')
    expect(html).toContain('<del>old</del>')
    expect(html).toMatch(/<a href="https:\/\/example\.com\/x"[^>]*>https:\/\/example\.com\/x<\/a>/)
    expect(html).toMatch(/<div class="sm-table" tabindex="0"><table>/)
    expect(html).toContain('<section class="footnotes"')
    // Footnote links stay in the page and carry the view's ID prefix.
    expect(html).toMatch(/<a id="user-content-fn-ref-1" href="#user-content-fn-1"[^>]*>1<\/a>/)
    expect(html).not.toMatch(/href="#user-content-fn-1"[^>]*target=/)
    expect(html).toContain('<li id="user-content-fn-1">')
  })

  it('gives headings unique, prefixed anchors', () => {
    const html = renderMarkdown('# Title\n\n## Next steps\n\n## Next steps\n\n### Location', 'spc_1', { idPrefix: 'user-content-r1-' })
    expect(html).toContain('<h1 id="user-content-r1-title">Title<a class="sm-anchor" href="#user-content-r1-title"')
    expect(html).toContain('id="user-content-r1-next-steps"')
    expect(html).toContain('id="user-content-r1-next-steps-1"')
    // Not removed as DOM clobbering (a bare id="location" would be).
    expect(html).toContain('id="user-content-r1-location"')
  })

  it('marks fenced code for highlighting and Mermaid fences for diagrams', () => {
    const html = renderMarkdown('```go\nfunc main() { fmt.Println("<b>") }\n```\n\n```\nplain\n```\n\n```mermaid\ngraph TD; A-->B\n```', 'spc_1')
    expect(html).toContain('<div class="sm-codeblock"><pre tabindex="0"><code class="language-go" data-lang="go">func main() { fmt.Println("&lt;b&gt;") }')
    expect(html).toContain('<div class="sm-codeblock"><pre tabindex="0"><code>plain')
    expect(html).toContain('<div class="sm-mermaid" data-sm-mermaid=""><pre tabindex="0"><code>graph TD; A--&gt;B')
  })

  it('resolves attachments through a custom resolver (another transport)', () => {
    const html = renderMarkdown(`![shot](spaceman://attachment/${id}) [log](spaceman://attachment/${id})`, 'spc_1', { resolveAttachment: (a) => `/mini/${a}` })
    expect(html).toContain(`<img src="/mini/${id}" alt="shot" loading="lazy" decoding="async">`)
    expect(html).toContain(`href="/mini/${id}"`)
  })

  it('keeps blob: URLs the resolver produced, and only those', () => {
    const blob = 'blob:https://app.example/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0'
    const html = renderMarkdown(`![shot](spaceman://attachment/${id}) [file](spaceman://attachment/${id}) <img src="blob:https://app.example/other"> [x](blob:https://app.example/other)`, 'spc_1', {
      resolveAttachment: () => blob,
    })
    const doc = new DOMParser().parseFromString(`<body>${html}</body>`, 'text/html')
    const imgs = Array.from(doc.querySelectorAll('img'))
    expect(imgs.map((i) => i.getAttribute('src'))).toEqual([blob])
    const links = Array.from(doc.querySelectorAll('a'))
    expect(links.map((a) => a.getAttribute('href'))).toEqual([blob, null])
    expect(html).not.toContain('blob:https://app.example/other')
  })

  it('leaves attachment references inside code alone', () => {
    expect(renderMarkdown(`\`spaceman://attachment/${id}\``, 'spc_1')).toContain(`<code>spaceman://attachment/${id}</code>`)
  })
})

describe('hostile input', () => {
  const vectors = [
    '<script>alert(1)</script>',
    '<img src=x onerror=alert(1)>',
    '<svg onload=alert(1)><circle /></svg>',
    '<math><mtext><table><mglyph><style><img src=x onerror=alert(1)></style></mglyph></table></mtext></math>',
    '<iframe src="https://evil.example"></iframe>',
    '<object data="x.swf"></object><embed src="x.swf">',
    '<form action="https://evil.example"><input name="password"><button>Sign in</button></form>',
    '<a href="javascript:alert(1)">x</a>',
    '<a href="JaVaScRiPt:alert(1)">x</a>',
    '<a href="&#106;avascript:alert(1)">x</a>',
    '<a href=" javascript:alert(1)">x</a>',
    '[x](javascript:alert(1))',
    '[x](<javascript:alert(1)>)',
    '[x](vbscript:msgbox(1))',
    '![x](javascript:alert(1))',
    '<img src="data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+">',
    '![x](data:text/html,<script>alert(1)</script>)',
    '<div style="background:url(javascript:alert(1))" onclick="x()">x</div>',
    '<style>body{display:none}</style>',
    '<link rel="stylesheet" href="https://evil.example/x.css"><meta http-equiv="refresh" content="0;url=https://evil.example"><base href="https://evil.example/">',
    '<details open ontoggle=alert(1)>x</details>',
    '<p><textarea><img src=x onerror=alert(1)></textarea></p>',
    '```"><img src=x onerror=alert(1)>\ncode\n```',
    '```mermaid\n</code></pre><img src=x onerror=alert(1)>\n```',
    '<input type="text" value="phish" autofocus onfocus=alert(1)>',
    `![x](spaceman://attachment/${id}" onerror="alert(1))`,
    '<a href="https://example.com" target="_self" rel="opener">x</a>',
    '<img src="/api/v1/spaces/spc_1/../../auth/logout">',
    '<noscript><p title="</noscript><img src=x onerror=alert(1)>"></noscript>',
    `<img src="spaceman://attachment/${id}" srcset="https://evil.example/p.gif 1x" sizes="1px">`,
  ]
  it.each(vectors)('neutralizes %s', (v) => {
    const html = renderMarkdown(v, 'spc_1')
    const doc = new DOMParser().parseFromString(`<body>${html}</body>`, 'text/html')
    for (const el of Array.from(doc.body.querySelectorAll('*'))) {
      expect(['SCRIPT', 'IFRAME', 'OBJECT', 'EMBED', 'FORM', 'BUTTON', 'TEXTAREA', 'STYLE', 'LINK', 'META', 'BASE', 'SVG', 'MATH']).not.toContain(el.tagName.toUpperCase())
      for (const a of Array.from(el.attributes)) {
        expect(a.name).not.toMatch(/^on/i)
        expect(a.name).not.toMatch(/^(style|srcset|sizes)$/)
        if (['href', 'src', 'action', 'formaction', 'xlink:href'].includes(a.name)) expect(a.value.trim()).not.toMatch(/^(javascript|vbscript|data):/i)
      }
      if (el.tagName === 'IMG') expect(el.getAttribute('src')).toMatch(/^\/api\/v1\/spaces\/spc_1\/attachments\/att_[0-9a-z]{26}\/content$/)
      if (el.tagName === 'INPUT') expect(el.getAttribute('type')).toBe('checkbox')
      if (el.tagName === 'A' && !el.getAttribute('href')?.startsWith('#')) {
        expect(el.getAttribute('target')).toBe('_blank')
        expect(el.getAttribute('rel')).toBe('noopener noreferrer nofollow')
      }
    }
  })

  it.each([
    ['nested lists', Array.from({ length: 2000 }, (_, i) => `${'  '.repeat(i)}- level ${i}`).join('\n')],
    ['tab-nested lists', Array.from({ length: 2000 }, (_, i) => `${'\t'.repeat(i)}- level ${i}`).join('\n')],
    ['stacked list markers', '- '.repeat(20_000) + 'x'],
    ['stacked ordered markers', '1. '.repeat(20_000) + 'x'],
    ['nested quotes', '>'.repeat(20_000) + ' deep'],
    ['quotes and lists', '> - '.repeat(10_000) + 'x'],
    ['brackets', '['.repeat(50_000) + 'x' + ']'.repeat(50_000)],
    ['an asterisk run', '*'.repeat(20_000) + 'x' + '*'.repeat(20_000)],
    ['unmatched emphasis', '*a '.repeat(20_000)],
    ['unmatched mixed emphasis', '*a _b ~c '.repeat(10_000)],
  ])('renders %s without freezing', (_, md) => {
    const t0 = performance.now()
    expect(typeof renderMarkdown(md, 'spc_1')).toBe('string')
    // jsdom is several times slower than a browser; unbounded, these took from seconds to minutes.
    expect(performance.now() - t0).toBeLessThan(3_000)
  })

  it('limits nesting outside fenced code only', () => {
    expect(limitNesting(' '.repeat(100) + '- x')).toBe(' '.repeat(64) + '- x')
    expect(limitNesting('> '.repeat(20) + 'x')).toBe('> '.repeat(16) + 'x')
    const fenced = '```\n' + ' '.repeat(100) + '- x\n' + '> '.repeat(20) + '\n```'
    expect(limitNesting(fenced)).toBe(fenced)
    expect(limitNesting('- a\n  - b\n    > c')).toBe('- a\n  - b\n    > c')
    // Too many delimiters in a paragraph: escaped, except in code spans and existing escapes; other paragraphs untouched.
    const many = '*a '.repeat(300) + '`x*y` \\*'
    const limited = limitNesting(`**bold**\n\n${many}`).split('\n\n')
    expect(limited[0]).toBe('**bold**')
    expect(limited[1]).toBe('\\*a '.repeat(300) + '`x*y` \\*')
    expect(renderMarkdown(many, 'spc_1')).toContain('*a *a')
  })

  it('plans diagrams: small ones render on their own, large ones on a click, none over the limits or the budget', () => {
    const nodes = (n: number, sep = '\n') => 'graph TD' + sep + Array.from({ length: n }, (_, i) => `N${i}`).join(sep)
    expect(diagramStatements('graph TD; A-->B\n  C & D --> E\n\n')).toBe(4)
    expect(planDiagrams([nodes(40), nodes(49, ';')])).toEqual([{ render: 'auto' }, { render: 'auto' }])
    expect(planDiagrams([nodes(60)])).toEqual([{ render: 'click' }])
    expect(planDiagrams(['graph TD\nA["' + 'x'.repeat(3000) + '"]'])).toEqual([{ render: 'click' }])
    expect(planDiagrams([nodes(600)])[0]).toMatchObject({ render: 'never', reason: expect.stringMatching(/500 statements/) })
    expect(planDiagrams(['graph TD\n' + 'A-->B\n'.repeat(2000)])[0]).toMatchObject({ render: 'never', reason: expect.stringMatching(/10000 characters/) })
    // The budget: 3 × 9,000 characters is over 25,000, so the third doesn't render; a later small one still fits.
    const long = 'graph TD\nA["' + 'x'.repeat(9000) + '"]'
    expect(planDiagrams([long, long, long, nodes(3)]).map((p) => p.render)).toEqual(['click', 'click', 'never', 'auto'])
    expect(planDiagrams(Array.from({ length: 21 }, () => nodes(2)))[20]).toMatchObject({ render: 'never' })
  })

  it('renders a huge body and a huge diagram as text', () => {
    const big = renderMarkdown('word '.repeat(200_000), 'spc_1')
    expect(big.length).toBeGreaterThan(1_000_000)
    const diagram = renderMarkdown('```mermaid\n' + 'graph TD\n' + Array.from({ length: 5000 }, (_, i) => `N${i}-->N${i + 1}`).join('\n') + '\n```', 'spc_1')
    expect(diagram).toContain('data-sm-mermaid')
    expect(diagram).not.toContain('<svg')
  })
})
