import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Configurations tab on the Service detail page.
 *
 * Per UX spec for task #6, the Configurations tab groups configurations into
 * two sections:
 *   - "Active" — Published configurations
 *   - "Version History" — Draft, Deprecated, Retired
 *
 * Tab activation is driven by the `?tab=` search param (the existing
 * loader reads `searchParams.get("tab") ?? "overview"`), so this spec
 * navigates directly to `?tab=configurations` rather than clicking the
 * trigger. The spec tolerates either populated or empty cluster state and
 * skips entirely when no Services exist.
 */
test.describe('/services/:name?tab=configurations — phase grouping', () => {
  test('renders Active and Version History sections, or the empty state', async ({ page }) => {
    await page.goto('/services');
    await page.waitForLoadState('domcontentloaded');

    const firstRowLink = page.getByRole('table').getByRole('link').first();
    if (!(await firstRowLink.isVisible().catch(() => false))) {
      test.skip(true, 'No Services in cluster — skipping Configurations tab smoke');
    }

    const href = await firstRowLink.getAttribute('href');
    test.skip(!href, 'First row link has no href — cannot navigate to detail page');

    await page.goto(`${href}?tab=configurations`);
    await page.waitForLoadState('domcontentloaded');
    await expect(page).toHaveURL(/\/services\/[^/]+\?tab=configurations$/);

    // Tab trigger should be visible and selected.
    const tab = page.getByRole('tab', { name: /configurations/i });
    await expect(tab).toBeVisible();

    // Either grouped sections are visible or an empty state is rendered.
    // Implementation uses an h3 "Active configuration" header (singular)
    // and an h3 "Version history" header. Empty state card title is
    // "No configurations yet". Use .first() on the combined locator so
    // that strict mode is satisfied when both section headings are
    // present at once (which is the case for the seeded compute service).
    const activeHeading = page.getByRole('heading', { name: /active configuration/i });
    const versionHistoryHeading = page.getByRole('heading', { name: /version history/i });
    const emptyTitle = page.getByText(/no configurations yet/i);

    await expect(
      activeHeading.or(versionHistoryHeading).or(emptyTitle).first()
    ).toBeVisible({ timeout: 10_000 });
  });
});
