// Adapted from Porch site/.vitepress/config.mts (Apache-2.0, same author).
import { defineConfig } from 'vitepress';
import { fileURLToPath, URL } from 'node:url';
import { withMermaid } from 'vitepress-plugin-mermaid';

const repo = 'https://github.com/aurimasniekis/go-spaceman';

// Engineering docs in ../docs link to each other as FOO.md; on the site they are included into these pages.
const docPages: Record<string, string> = {
  TRACEABILITY: '/guide/status',
  LIMITATIONS: '/reference/limitations',
  THREAT_MODEL: '/security/threat-model',
  DECISIONS: '/security/decisions',
  SCREENSHOTS: '/development/screenshots',
  DEMO: '/development/demo',
  GOAL: repo + '/blob/main/docs/GOAL.md',
  PLAN: repo + '/blob/main/docs/PLAN.md',
  SECURITY_REVIEW: repo + '/blob/main/docs/SECURITY_REVIEW.md',
  TEST_STRATEGY: repo + '/blob/main/docs/TEST_STRATEGY.md',
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
    // An accessible copy of the search button (theme/components/VPNavBarSearchButton.vue).
    vite: {
      resolve: {
        alias: [{ find: /^.*\/VPNavBarSearchButton\.vue$/, replacement: fileURLToPath(new URL('./theme/components/VPNavBarSearchButton.vue', import.meta.url)) }],
      },
    },
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
            { text: 'Enforced approvals', link: '/guide/approvals' },
            { text: 'Agent tools', link: '/guide/agent-tools' },
            { text: 'Messages', link: '/guide/messages' },
            { text: 'Control and workers', link: '/guide/control' },
            { text: 'Roles and prompts', link: '/guide/roles-and-prompts' },
            { text: 'Knowledge bases', link: '/guide/knowledge' },
            { text: 'Cross-space requests', link: '/guide/cross-space' },
            { text: 'Role memory', link: '/guide/memory' },
            { text: 'Dreams', link: '/guide/dreams' },
            { text: 'Usage and transcripts', link: '/guide/usage' },
            { text: 'Skills', link: '/guide/skills' },
            { text: 'MCP gateway', link: '/guide/mcp-gateway' },
            { text: 'Events and subscriptions', link: '/guide/events' },
            { text: 'Schedules', link: '/guide/schedules' },
            { text: 'Limits and budgets', link: '/guide/limits' },
            { text: 'Publishing SpaceMan events', link: '/guide/publishing-events' },
            { text: 'Single sign-on', link: '/guide/sign-in' },
            { text: 'Project status', link: '/guide/status' },
          ],
        },
        {
          text: 'Operators',
          items: [
            { text: 'Installation', link: '/operators/install' },
            { text: 'Containers and images', link: '/operators/containers' },
            { text: 'Upgrade and rollback', link: '/operators/upgrade' },
            { text: 'Sign-in and access', link: '/operators/access' },
            { text: 'Nodes and leases', link: '/operators/nodes' },
            { text: 'Relocating a space', link: '/operators/relocation' },
            { text: 'Accounts', link: '/operators/accounts' },
            { text: 'Terminals and recordings', link: '/operators/terminals' },
            { text: 'Backups and restore', link: '/operators/backup' },
            { text: 'Observability', link: '/operators/observability' },
            { text: 'Terraform', link: '/operators/terraform' },
            { text: 'Command line', link: '/operators/cli' },
          ],
        },
        {
          text: 'Reference',
          items: [
            { text: 'HTTP API', link: '/reference/api' },
            { text: 'Command line', link: '/reference/cli/' },
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
            { text: 'End-to-end demonstration', link: '/development/demo' },
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
