import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Settings tab on the Service detail page.
 *
 * Per UX spec for task #7 (and team-lead confirmation), the Settings tab is
 * a third tab on `services.$name.tsx`. The form submits via a Remix
 * `action` on that same route — there is no separate edit page.
 *
 * Tab activation uses the `?tab=` search param. This smoke test asserts
 * the form fields and Save/Cancel affordances are visible. The full edit
 * flow (PATCH, success toast, validation) is covered by the manual
 * walk-through in test-plan.md §9.2.
 */
test.describe('/services/:name?tab=settings — Settings tab', () => {
  test('renders Display Name, Description, Phase fields, and Save/Cancel buttons', async ({
    page,
  }) => {
    await page.goto('/services');
    await page.waitForLoadState('domcontentloaded');

    const firstRowLink = page.getByRole('table').getByRole('link').first();
    if (!(await firstRowLink.isVisible().catch(() => false))) {
      test.skip(true, 'No Services in cluster — skipping Settings tab smoke');
    }

    const href = await firstRowLink.getAttribute('href');
    test.skip(!href, 'First row link has no href — cannot navigate to detail page');

    await page.goto(`${href}?tab=settings`);
    await page.waitForLoadState('domcontentloaded');

    const tab = page.getByRole('tab', { name: /settings/i });
    await expect(tab).toBeVisible();

    // Identity card — labels are case-insensitive to tolerate copy tweaks.
    // Implementation uses "Service name", "Display name" (lowercase 'n'),
    // "Description", "Owner project" — see SettingsTabBody.
    await expect(page.getByLabel(/service name/i)).toBeVisible();
    await expect(page.getByLabel(/display name/i)).toBeVisible();
    await expect(page.getByLabel(/description/i)).toBeVisible();
    await expect(page.getByLabel(/owner project/i)).toBeVisible();

    // Lifecycle card — phase is in its own card with a Select control. Note
    // datum-ui's CardTitle renders as <div data-slot="card-title">, not a
    // heading role, so we match by visible text instead.
    await expect(page.getByText(/^Lifecycle$/)).toBeVisible();
    await expect(page.getByRole('combobox')).toBeVisible();

    // Identity card footer has Save changes + Reset. Lifecycle has its own
    // submit ("Update phase"). Match the Identity primary by `.first()`.
    await expect(
      page.getByRole('button', { name: /save changes/i }).first()
    ).toBeVisible();
    await expect(page.getByRole('button', { name: /reset/i })).toBeVisible();
  });
});
