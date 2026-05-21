import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright configuration for Milo Service Catalog UI E2E tests.
 *
 * The Remix dev server proxies the Kubernetes API server, so the tests need
 * a kubeconfig with read access to `services.miloapis.com/v1alpha1` resources.
 * Start the dev server first with `task ui:dev` (or let Playwright spawn it
 * via the `webServer` config below — `reuseExistingServer: true` will pick
 * up an already-running instance).
 */
// Allow tests to target a non-default UI host (e.g. a deployed dev cluster on
// :3001) without editing the config. Falls back to the local dev default.
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? 'http://localhost:3000';

export default defineConfig({
  testDir: './e2e',

  // Run tests in files in parallel
  fullyParallel: true,

  // Fail the build on CI if you accidentally left test.only in the source code
  forbidOnly: !!process.env.CI,

  // Retry on CI only
  retries: process.env.CI ? 2 : 0,

  // Opt out of parallel tests on CI
  workers: process.env.CI ? 1 : undefined,

  // Reporter to use
  reporter: 'html',

  // Shared settings for all the projects below
  use: {
    // Base URL to use in actions like `await page.goto('/')`. Override with
    // PLAYWRIGHT_BASE_URL when targeting a non-local instance.
    baseURL,

    // Collect trace when retrying the failed test
    trace: 'on-first-retry',

    // Screenshot on failure
    screenshot: 'only-on-failure',
  },

  // Configure projects for major browsers
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  // Run the dev server before starting the tests. Reuse an existing one if
  // already running so contributors can keep `task ui:dev` open in another
  // terminal.
  webServer: {
    command: 'pnpm dev',
    url: `${baseURL}/health`,
    reuseExistingServer: !process.env.CI,
    stdout: 'ignore',
    stderr: 'pipe',
    timeout: 120 * 1000,
  },
});
