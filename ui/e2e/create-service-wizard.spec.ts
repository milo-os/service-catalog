import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Create Service Wizard.
 *
 * Route (confirmed): /services/new
 *
 * Per the implementation in `services.new.tsx`, the wizard has 4 steps
 * with labels (from STEP_LABELS):
 *   1. Service identity
 *   2. Monitored resource types
 *   3. Meters
 *   4. Review & create
 *
 * Page heading is `<h1>New service</h1>` (not "Create service"). The
 * wizard mounts on step 1 by default; ?step=N navigates between steps.
 *
 * This smoke test asserts the heading and the four step labels render.
 * Per-step validation, navigation, and submission are covered in the
 * manual plan (test-plan.md §9.4).
 */
test.describe('/services/new — Create Service Wizard', () => {
  test('renders the wizard heading and 4-step indicator', async ({ page }) => {
    await page.goto('/services/new');
    await page.waitForLoadState('domcontentloaded');

    await expect(
      page.getByRole('heading', { name: /new service/i })
    ).toBeVisible({ timeout: 10_000 });

    // Step labels — assert each label appears at least once on the page
    // (they appear in the stepper at the top and the active-step h2).
    await expect(page.getByText(/service identity/i).first()).toBeVisible();
    await expect(page.getByText(/monitored resource types/i).first()).toBeVisible();
    await expect(page.getByText(/meters/i).first()).toBeVisible();
    await expect(page.getByText(/review/i).first()).toBeVisible();
  });
});
