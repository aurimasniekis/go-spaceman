import { definitionError, parseDefinition, starterDefinition } from './definition'

describe('parseDefinition', () => {
  it('accepts the starter definition', () => {
    const r = parseDefinition(JSON.stringify(starterDefinition))
    expect(r.errors).toEqual([])
    expect(r.def?.subject).toBe(starterDefinition.subject)
  })

  it('reports invalid JSON and non-objects', () => {
    expect(parseDefinition('{').errors[0]).toMatch(/^Not valid JSON/)
    expect(parseDefinition('[]').errors).toEqual(['The definition must be a JSON object'])
  })

  it('names the fields that are wrong', () => {
    const r = parseDefinition(JSON.stringify({
      subject: 'github.{{vars.org}}.>',
      variables: [{ name: 'Org' }, { name: 'repo', pattern: '(' }, { name: 'repo' }],
      filters: [{ path: 'vars.x', op: 'like' }, { path: 'data.a', op: 'in' }, { path: 'data.b', op: 'eq', value: '{{vars.missing}}' },
        { path: 'data.c', op: 'absent', if_set: 'nope', unless_set: 'repo' }],
      title: '',
      body: '',
      instructions: 'Look at {{data.title}}',
      urgency: 'soon',
    }))
    expect(r.def).toBeDefined()
    expect(r.errors).toEqual(expect.arrayContaining([
      'subject: must be a Pulse filter under evt.',
      'title: is required',
      'urgency: must be low, normal, high or urgent',
      'variables: "Org" is not a valid name',
      'variables.repo.pattern: is not a valid regular expression',
      'variables: "repo" is defined twice',
      'subject: variable "org" is not defined',
      'filters[0].path: must be event.… or data.…',
      'filters[0].op: must be eq, ne, in, exists, absent or prefix',
      'filters[1]: in needs values',
      'filters[2]: variable "missing" is not defined',
      'filters[3].if_set: variable "nope" is not defined',
      'instructions: may only use {{vars.name}} placeholders; event content is untrusted and cannot become instructions',
    ]))
  })

  it('accepts optional filters that name a variable', () => {
    const r = parseDefinition(JSON.stringify({
      subject: 'evt.ops.alert.>', title: 'x', variables: [{ name: 'env' }, { name: 'all', pattern: 'true' }],
      filters: [{ path: 'data.env', op: 'eq', value: '{{vars.env}}', if_set: 'env' }, { path: 'data.muted', op: 'absent', unless_set: 'all' }],
    }))
    expect(r.errors).toEqual([])
  })

  it('fills missing lists so the definition can be sent', () => {
    const r = parseDefinition('{"subject":"evt.ops.alert.>","title":"x"}')
    expect(r.errors).toEqual([])
    expect(r.def).toMatchObject({ variables: [], filters: [], body: '' })
  })
})

describe('definitionError', () => {
  it('returns the first problem with a count', () => {
    expect(definitionError({ value: JSON.stringify(starterDefinition) })).toBeUndefined()
    expect(definitionError({ value: '{"subject":"","title":""}' })).toBe('subject: is required (and 1 more)')
  })
})
