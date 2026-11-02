import githubIssue from './github-issues-opened.sample.json'

/**
 * Sample events for testing templates in the UI. The GitHub issue sample is the event go-pulse-github v0.1.0 (MIT) builds
 * for its issues_opened fixture (derivation: internal/events/template/testdata/README.md).
 */
export const samples: Record<string, { subject: string; type: string; source: string; data: unknown }> = {
  'github-issue': { subject: 'evt.github.example-org.webhook.issues.opened', type: 'github.webhook.issues.opened.v1', source: 'pulse-github', data: githubIssue },
}
