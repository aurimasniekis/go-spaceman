import { MantineProvider } from '@mantine/core'
import { fireEvent, render, screen } from '@testing-library/react'

import { describe, expect, it, vi } from 'vitest'
import { SchemaForm, formable } from './SchemaForm'
import { compactValue, deref, optionsOf, supported } from './schemaValue'

// jsdom has no matchMedia (Mantine reads the color scheme).
window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} })) as unknown as typeof window.matchMedia

globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver

const schema = {
  type: 'object',
  required: ['site', 'mode'],
  properties: {
    site: { type: 'object', required: ['name'], properties: { name: { type: 'string', title: 'Site name' }, level: { $ref: '#/$defs/level' } } },
    contacts: { type: 'array', title: 'Contacts', items: { type: 'object', title: 'Contact', properties: { email: { type: 'string' }, retries: { type: 'integer', minimum: 0 } } } },
    mode: { oneOf: [{ const: 'a', title: 'Alpha' }, { const: 'b', title: 'Beta' }] },
    scores: { type: 'array', items: { type: 'number' } },
  },
  $defs: { level: { type: 'string', enum: ['low', 'high'], title: 'Level' } },
}

describe('complex schemas', () => {
  it('are formable when every part is supported', () => {
    expect(formable(schema)).toBe(true)
    expect(formable({ properties: { x: { oneOf: [{ type: 'string' }, { type: 'number' }] } } })).toBe(false)
    expect(formable({ properties: { x: { $ref: 'https://example.com/x.json' } } })).toBe(false)
    expect(supported({ type: 'object', properties: { a: { type: 'object', properties: { b: { type: 'object', properties: { c: { type: 'object', properties: { d: { type: 'object', properties: { e: { type: 'object', properties: { f: { type: 'string' } } } } } } } } } } } } }, {})).toBe(false)
  })
  it('resolves local refs and oneOf constants', () => {
    expect(deref({ $ref: '#/$defs/level' }, schema)?.enum).toEqual(['low', 'high'])
    expect(deref({ $ref: '#/$defs/missing' }, schema)).toBeUndefined()
    expect(optionsOf(schema.properties.mode)).toEqual([{ value: 'a', label: 'Alpha', raw: 'a' }, { value: 'b', label: 'Beta', raw: 'b' }])
  })
  it('survives malformed refs and null nodes, and keeps enum types', () => {
    expect(deref({ $ref: '#/$defs/100%' }, schema)).toBeUndefined()
    expect(formable({ properties: { a: null } })).toBe(false)
    expect(optionsOf({ enum: [1, true] })?.map((o) => o.raw)).toEqual([1, true])
    expect(compactValue({ a: { b: '' }, c: 1 })).toEqual({ c: 1 })
  })
  it('compacts nested empties', () => {
    expect(compactValue({ a: '', b: { c: '', d: [] }, e: [{ x: '' }, { x: 'y' }], f: 0, g: false })).toEqual({ e: [{ x: 'y' }], f: 0, g: false })
  })
  it('render nested objects and repeated groups, and report the nested value', () => {
    const onChange = vi.fn()
    render(<MantineProvider><SchemaForm label="Response" schema={schema} value={{}} onChange={onChange} /></MantineProvider>)
    fireEvent.change(screen.getByLabelText(/Site name/), { target: { value: 'North' } })
    expect(onChange).toHaveBeenLastCalledWith({ site: { name: 'North' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add Contact' }))
    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'a@example.com' } })
    expect(onChange).toHaveBeenLastCalledWith({ site: { name: 'North' }, contacts: [{ email: 'a@example.com' }] })
    expect(screen.getByRole('radio', { name: 'Alpha' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Remove Contact 1' }))
    expect(onChange).toHaveBeenLastCalledWith({ site: { name: 'North' } })
  })
})
