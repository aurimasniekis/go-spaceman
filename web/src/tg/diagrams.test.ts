import { expect, it } from 'vitest'
import { mermaidSources } from './diagrams'

it('finds mermaid fences and nothing else', () => {
  const body = 'text\n\n```mermaid\ngraph TD\n  A-->B\n```\n\n```js\nnope\n```\n\n~~~mermaid\nflowchart LR\n  X-->Y\n~~~\n\n```mermaid\nunclosed'
  expect(mermaidSources(body)).toEqual(['graph TD\n  A-->B', 'flowchart LR\n  X-->Y', 'unclosed'])
  expect(mermaidSources('no fences')).toEqual([])
})
