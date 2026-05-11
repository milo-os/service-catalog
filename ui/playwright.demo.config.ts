import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright config for the automated demo recording (ui/e2e/demo.spec.ts).
 *
 * Differences from the main config:
 * - Targets only demo.spec.ts
 * - Video always on
 * - 1280×800 viewport for a clean recording frame
 * - Single worker, no retries — one clean take
 */
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? 'http://localhost:3000';

export default defineConfig({
  testDir: './e2e/scenes',
  testMatch: '**/*.spec.ts',

  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  timeout: 300_000,

  reporter: [['html', { outputFolder: 'playwright-report-demo' }]],

  use: {
    baseURL,
    video: 'on',
    screenshot: 'only-on-failure',
    trace: 'off',
    viewport: { width: 1280, height: 800 },
    actionTimeout: 10_000,
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  webServer: {
    command: 'pnpm dev',
    url: `${baseURL}/health`,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
    timeout: 120 * 1000,
  },
});
