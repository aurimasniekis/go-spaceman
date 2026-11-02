// Adapted from Porch site/.vitepress/config.mts (Apache-2.0, same author).
import { defineConfig } from 'vitepress';
import { withMermaid } from 'vitepress-plugin-mermaid';

const repo = 'https://github.com/aurimasniekis/go-spaceman';

// Engineering docs in ../docs link to each other as FOO.md; on the site they are included into these pages.
const docPages: Record<string, string> = {
  TRACEABILITY: '/guide/status',
  LIMITATIONS: '/reference/limitations',
  THREAT_MODEL: '/security/threat-model',
  DECISIONS: '/security/decisions',
  SCREENSHOTS: '/development/screenshots',
  GOAL: repo + '/blob/main/docs/GOAL.md',
  PLAN: repo + '/blob/main/docs/PLAN.md',
};

export default withMermaid(
  defineConfig({
    title: 'SpaceMan',
    description:
      'Self-hosted control plane for persistent teams of coding agents: Claude Code and Codex in herdr-core terminals on your own nodes, with leases, recordings and audit.',
    lang: 'en-US',
    // GitHub Pages project site: https://aurimasniekis.github.io/go-spaceman/
    base: '/go-spaceman/',
    cleanUrls: true,
    lastUpdated: false,
    srcExclude: ['README.md'],
    head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/go-spaceman/logo.svg' }]],
    appearance: 'dark',
    markdown: {
      theme: { light: 'github-light', dark: 'night-owl' },
      config: (md) => {
        const original = md.renderer.rules.link_open!;
        md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
          const href = tokens[idx].attrGet('href');
          const m = href?.match(/^(?:\.\/)?([A-Z_]+)\.md(#.*)?$/);
          if (m && docPages[m[1]]) tokens[idx].attrSet('href', docPages[m[1]] + (m[2] ?? ''));
          return original(tokens, idx, options, env, self);
        };
      },
    },
    mermaid: {},
    themeConfig: {
      logo: '/logo.svg',
      nav: [
        { text: 'Guide', link: '/guide/what-is-spaceman' },
        { text: 'Operators', link: '/operators/install' },
        { text: 'Reference', link: '/reference/api' },
        { text: 'Security', link: '/security/' },
        { text: 'Development', link: '/development/testing' },
      ],
      sidebar: [
        {
          text: 'Guide',
          items: [
            { text: 'What is SpaceMan?', link: '/guide/what-is-spaceman' },
            { text: 'Concepts', link: '/guide/concepts' },
            { text: 'Try it locally', link: '/guide/try-locally' },
            { text: 'First node and space', link: '/guide/first-space' },
            { text: 'The inbox', link: '/guide/inbox' },
            { text: 'The execution queue', link: '/guide/queue' },
            { text: 'Human actions', link: '/guide/human-actions' },
            { text: 'Agent tools', link: '/guide/agent-tools' },
            { text: 'Messages', link: '/guide/messages' },
            { text: 'Control and workers', link: '/guide/control' },
            { text: 'Events and subscriptions', link: '/guide/events' },
            { text: 'Project status', link: '/guide/status' },
          ],
        },
        {
          text: 'Operators',
          items: [
            { text: 'Install from source', link: '/operators/install' },
            { text: 'Sign-in and access', link: '/operators/access' },
            { text: 'Nodes and leases', link: '/operators/nodes' },
            { text: 'Accounts', link: '/operators/accounts' },
            { text: 'Terminals and recordings', link: '/operators/terminals' },
          ],
        },
        {
          text: 'Reference',
          items: [
            { text: 'HTTP API', link: '/reference/api' },
            { text: 'Known limitations', link: '/reference/limitations' },
          ],
        },
        {
          text: 'Security',
          items: [
            { text: 'Overview', link: '/security/' },
            { text: 'Threat model', link: '/security/threat-model' },
            { text: 'Decisions (ADRs)', link: '/security/decisions' },
          ],
        },
        {
          text: 'Development',
          items: [
            { text: 'Building and testing', link: '/development/testing' },
            { text: 'Screenshots', link: '/development/screenshots' },
            { text: 'Release notes', link: '/development/changelog' },
          ],
        },
      ],
      socialLinks: [{ icon: 'github', link: repo }],
      search: { provider: 'local' },
      footer: { message: 'Apache-2.0', copyright: '© 2026 Aurimas Niekis' },
    },
  }),
);
