import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Create Configuration wizard.
 *
 * Route: /services/:name/configurations/new
 * File:  ui/app/routes/services.$name_.configurations.new.tsx
 *
 * Per `STEP_LABELS` in the implementation, the wizard has 4 steps:
 *   1. Version & source
 *   2. Monitored resource types
 *   3. Meters
 *   4. Review & create
 *
 * Step 1 includes:
 *   - An info Alert titled "Version suggestion" with a "Use this" button.
 *     The Alert is gated on `form.version !== suggestedVersion`, and the
 *     form initialises with `version: suggestedVersion` — so on a fresh
 *     load the Alert is hidden. The test types a divergent value into the
 *     version input to surface it.
 *   - A `<RadioGroup>` with "Start blank" and "Clone an existing version".
 *     The clone radio is enabled when the service has ≥1 prior config.
 *
 * The wizard is keyed off `?step=N` for the active step. Advancement runs
 * through the "Next →" button so client-side validation fires.
 *
 * This test does NOT submit the wizard. The webhook is in `failurePolicy:
 * Fail` mode in dev and the test environment can't satisfy it. Manual
 * coverage of submission lives in test-plan.md.
 *
 * Each test waits on `networkidle` (not just `domcontentloaded`) so React
 * hydration completes before assertions or interactions run — without it,
 * the Next button clicks race a re-render and detach mid-click.
 */
test.describe('/services/:name/configurations/new — Create Configuration wizard', () => {
  const serviceName = 'compute-miloapis-com';
  const wizardUrl = `/services/${serviceName}/configurations/new`;

  test('renders the wizard heading and 4-step indicator', async ({ page }) => {
    await page.goto(wizardUrl);
    await page.waitForLoadState('networkidle');

    // Page heading is <h1>New configuration</h1>.
    await expect(
      page.getByRole('heading', { name: /^new configuration$/i })
    ).toBeVisible({ timeout: 10_000 });

    // Stepper labels (from STEP_LABELS) live inside the <aside> rail.
    // Scoping the locator there avoids ambiguity with other places the
    // same words appear (e.g. the radio description).
    const stepper = page.locator('aside');
    await expect(stepper).toContainText('Version & source');
    await expect(stepper).toContainText('Monitored resource types');
    await expect(stepper).toContainText('Meters');
    await expect(stepper).toContainText('Review & create');
  });

  test('surfaces the auto-version Alert with a "Use this" button when version diverges', async ({
    page,
  }) => {
    await page.goto(wizardUrl);
    await page.waitForLoadState('networkidle');

    // The Alert is hidden when form.version === suggestedVersion. Type a
    // divergent value to make it appear.
    const versionInput = page.getByLabel(/^version$/i);
    await versionInput.fill('9.9.9');

    // Info Alert: <AlertTitle>Version suggestion</AlertTitle> + a
    // <button>Use this</button> in its description.
    await expect(page.getByText(/version suggestion/i)).toBeVisible({
      timeout: 10_000,
    });
    await expect(
      page.getByRole('button', { name: /^use this$/i })
    ).toBeVisible();
  });

  test('exposes an enabled "Clone an existing version" radio (compute has prior configs)', async ({
    page,
  }) => {
    await page.goto(wizardUrl);
    await page.waitForLoadState('networkidle');

    const cloneRadio = page.getByRole('radio', {
      name: /clone an existing version/i,
    });
    await expect(cloneRadio).toBeVisible({ timeout: 10_000 });
    await expect(cloneRadio).toBeEnabled();
  });

  test('navigates from step 1 through to step 4 (Review) via Next', async ({
    page,
  }) => {
    await page.goto(wizardUrl);
    await page.waitForLoadState('networkidle');

    const nextButton = page.getByRole('button', { name: /^next/i });

    // Step 1 → 2.
    await expect(nextButton).toBeVisible({ timeout: 10_000 });
    await nextButton.click();
    await expect(page).toHaveURL(/[?&]step=2(&|$)/, { timeout: 10_000 });

    // Step 2 → 3 (blank MRTs allowed).
    await nextButton.click();
    await expect(page).toHaveURL(/[?&]step=3(&|$)/, { timeout: 10_000 });

    // Step 3 → 4 (blank meters allowed).
    await nextButton.click();
    await expect(page).toHaveURL(/[?&]step=4(&|$)/, { timeout: 10_000 });

    // Step 4 renders the "Review & create" h2.
    await expect(
      page.getByRole('heading', { name: /review & create/i })
    ).toBeVisible();
  });

  test('renders the Services / <name> / New configuration breadcrumb trail', async ({
    page,
  }) => {
    await page.goto(wizardUrl);
    await page.waitForLoadState('networkidle');

    const breadcrumb = page.getByRole('navigation', { name: /breadcrumb/i });
    await expect(
      breadcrumb.getByRole('link', { name: 'Services' })
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      breadcrumb.getByRole('link', { name: serviceName })
    ).toBeVisible();
    await expect(breadcrumb.getByText(/new configuration/i)).toBeVisible();
  });
});
