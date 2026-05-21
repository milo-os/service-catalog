import { test, expect } from './fixtures';

/**
 * E2E smoke test for /services (Service list route).
 *
 * Visits the route, asserts the page title/heading, and either confirms the
 * table column headers when at least one Service exists, or confirms the
 * EmptyContent message when none do. The test passes against either state so
 * it can run on a freshly-initialised kind cluster.
 *
 * Expected columns (per ux-spec §8.2): Name, Service Name, Phase,
 * Configurations, Age, Owner.
 */

test.describe('/services — Service list', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/services');
    await page.waitForLoadState('domcontentloaded');
  });

  test('renders the Services page heading and description', async ({ page }) => {
    await expect(page).toHaveTitle(/Service Catalog|Services/i);

    // PageTitle renders the title as a span with text-2xl font-medium.
    await expect(page.getByText('Services', { exact: true }).first()).toBeVisible();

    await expect(
      page.getByText(
        'Cluster-scoped governance catalog entries for provider-registered services.'
      )
    ).toBeVisible();
  });

  test('renders the breadcrumb trail Home / Services', async ({ page }) => {
    const breadcrumb = page.getByRole('navigation', { name: /breadcrumb/i });
    await expect(breadcrumb.getByRole('link', { name: 'Home' })).toBeVisible();
    await expect(breadcrumb.getByText('Services').first()).toBeVisible();
  });

  test('renders either the table column headers or the empty state', async ({ page }) => {
    const table = page.getByRole('table');
    const emptyHeading = page.getByText(/no services have been registered yet/i);

    // Wait for one of the two terminal states.
    await expect(table.or(emptyHeading)).toBeVisible({ timeout: 10_000 });

    if (await table.isVisible()) {
      const expectedHeaders = [
        'Name',
        'Service Name',
        'Phase',
        'Configurations',
        'Age',
        'Owner',
      ];

      for (const header of expectedHeaders) {
        await expect(
          page.getByRole('columnheader', { name: header, exact: true })
        ).toBeVisible();
      }
    } else {
      await expect(emptyHeading).toBeVisible();
      await expect(
        page.getByText(
          'Services define the canonical catalog entries for provider APIs.'
        )
      ).toBeVisible();
    }
  });
});
