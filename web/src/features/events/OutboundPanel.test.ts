import { lagText } from './OutboundPanel'

describe('lagText', () => {
  it('formats the publication lag', () => {
    expect(lagText(0)).toBe('—')
    expect(lagText(12.4)).toBe('12 s')
    expect(lagText(150)).toBe('3 min')
    expect(lagText(5400)).toBe('1.5 h')
  })
})
