import { defineConfig, devices } from '@playwright/test'

// Runs against a stack from `scripts/dev-stack.sh up` (SM_URL and friends come from its env file).
export default defineConfig({
  testDir: './e2e',
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
