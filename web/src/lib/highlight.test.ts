import { languageName, paint, tokenize } from './highlight'

describe('highlight', () => {
  it('maps fence names and aliases to grammars', () => {
    expect(languageName('go')).toBe('go')
    expect(languageName('golang')).toBe('go')
    expect(languageName('sh')).toBe('shell')
    expect(languageName('yml')).toBe('yaml')
    expect(languageName('typescript')).toBe('ts')
    // Plain text and unknown names have no grammar; so do inherited object keys.
    for (const n of ['text', 'plaintext', 'cobol', 'constructor', '__proto__', 'toString', 'hasOwnProperty']) expect(languageName(n)).toBeUndefined()
  })

  it('tokenizes code with a lazily loaded grammar', async () => {
    const tokens = await tokenize('package main\n// note\nfunc f() string { return "x" }', 'go')
    expect(tokens?.find((t) => t.value === 'func')?.className).toBe('keyword')
    expect(tokens?.find((t) => t.value === '"x"')?.className).toBe('string')
    expect(tokens?.map((t) => t.value).join('')).toBe('package main\n// note\nfunc f() string { return "x" }')
    expect(await tokenize('x', 'cobol')).toBeUndefined()
  })

  it('keeps several grammars at once', async () => {
    await tokenize('echo hi', 'bash')
    expect((await tokenize('const a = 1', 'ts'))?.find((t) => t.value === 'const')?.className).toBe('keyword')
    expect((await tokenize('echo hi', 'sh'))?.length).toBeGreaterThan(0)
  })

  it('paints tokens as text, never as HTML', async () => {
    const code = document.createElement('code')
    const src = 'const s = "<img src=x onerror=alert(1)>" // <script>alert(1)</script>'
    paint(code, (await tokenize(src, 'js'))!)
    expect(code.textContent).toBe(src)
    expect(code.querySelector('img, script')).toBeNull()
    expect(code.querySelector('.th-token.th-keyword')?.textContent).toBe('const')
  })
})
