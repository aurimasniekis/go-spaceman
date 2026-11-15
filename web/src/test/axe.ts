import axe from 'axe-core'

/**
 * Runs axe (WCAG 2.1 A/AA, as the e2e and site audits do) over a rendered component and returns its violations as
 * "rule: target" lines. jsdom does no layout, so colour contrast is left to the browser audits.
 */
export async function axeViolations(root: Element = document.body): Promise<string[]> {
  const res = await axe.run(root, {
    runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'] },
    rules: { 'color-contrast': { enabled: false } },
  })
  return res.violations.map((v) => `${v.id}: ${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')}`)
}
