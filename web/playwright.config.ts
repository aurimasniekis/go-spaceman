import { defineConfig, devices } from '@playwright/test'

// Runs against a stack from `scripts/dev-stack.sh up` (SM_URL and friends come from its env file).
export default defineConfig({
  testDir: './e2e',
  // The documentation capture runs only through scripts/screenshots.sh (it needs its own fresh stack).
  // The site audit runs only through `make site-check` (it needs the built site).
  // The demonstration's browser steps run only from its driver (`make demo`, test/demo).
  testIgnore: [
    ...(process.env.SM_SCREENSHOTS_OUT ? [] : ['**/screenshots.spec.ts']),
    ...(process.env.SM_SITE_DIST ? [] : ['**/site-a11y.spec.ts']),
    ...(process.env.SM_DEMO_PHASE ? [] : ['**/demo.spec.ts']),
  ],
  timeout: 90_000,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: process.env.SM_URL ?? 'http://127.0.0.1:38180',
    trace: 'retain-on-failure',
    viewport: { width: 1440, height: 900 },
    colorScheme: 'dark',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
})
