import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Overview tab on the Service detail page.
 *
 * Per UX spec for task #5, the Overview tab is the default tab on
 * `services.$name.tsx`. It surfaces:
 *   - Page heading (display name) with the canonical `serviceName`
 *     rendered in monospace below it
 *   - Three tabs: Overview (default), Configurations, Settings
 *   - Details card (Service Name, Display Name, Phase, Owner Project,
 *     Description)
 *   - Conditions card (renders an EmptyContent block when empty)
 *   - Active configuration section with two sub-cards: "Monitored
 *     Resource Types" and "Meters". When a Published configuration
 *     exists the section heading is a link to the configuration detail
 *     page; otherwise it falls back to a plain heading and shows
 *     "No active configuration".
 *
 * The tests target seeded services:
 *   - `compute-miloapis-com` — has a Published configuration with both
 *     monitored resource types and meters
 *   - `networking-miloapis-com` — has only a Draft configuration, so the
 *     overview should report "No active configuration"
 */
test.describe('/services/:name?tab=overview — Overview tab', () => {
  test.describe('compute-miloapis-com (Published service with active config)', () => {
    test.beforeEach(async ({ page }) => {
      await page.goto('/services/compute-miloapis-com');
      await page.waitForLoadState('networkidle');
    });

    test('Overview tab is visible and active by default', async ({ page }) => {
      await expect(page.getByRole('tab', { name: /overview/i })).toBeVisible();
      await expect(page.getByRole('tab', { name: /configurations/i })).toBeVisible();
      await expect(page.getByRole('tab', { name: /settings/i })).toBeVisible();

      // Overview is selected by default (no ?tab= param in the URL). Use
      // the accessibility tree's `selected` property so the assertion
      // works regardless of whether Radix exposes `aria-selected` or
      // `data-state="active"`.
      await expect(
        page.getByRole('tab', { name: /overview/i, selected: true })
      ).toBeVisible();
    });

    test('renders the Details card with the canonical Service Name', async ({ page }) => {
      // Details card title is rendered via datum-ui CardTitle which is a
      // <div data-slot="card-title">, not a heading role — match by text.
      await expect(page.getByText(/^Details$/)).toBeVisible();

      // Service Name label is a <dt>; value is a <dd> with a font-mono
      // span containing the canonical serviceName.
      await expect(page.getByText('Service Name', { exact: true })).toBeVisible();
      // The serviceName also appears in the page subtitle directly under
      // the heading, so two visible matches are expected — use .first().
      await expect(
        page.getByText('compute.miloapis.com', { exact: true }).first()
      ).toBeVisible();

      // Other Details fields per the UX spec.
      await expect(page.getByText('Display Name', { exact: true })).toBeVisible();
      await expect(page.getByText('Phase', { exact: true })).toBeVisible();
      await expect(page.getByText('Owner Project', { exact: true })).toBeVisible();
      await expect(page.getByText('Description', { exact: true })).toBeVisible();
    });

    test('renders the Active configuration section heading', async ({ page }) => {
      // When a Published configuration exists, the heading is rendered as
      // a link (to the configuration detail page) styled with uppercase
      // tracking. Match case-insensitively.
      const activeConfigLink = page.getByRole('link', {
        name: /active configuration/i,
      });
      await expect(activeConfigLink).toBeVisible();
    });

    test('renders the Monitored Resource Types card with at least one entry', async ({
      page,
    }) => {
      // CardTitle renders as a div, so match by text. The seeded compute
      // configuration has at least "Compute Instance" and "Persistent Disk".
      await expect(
        page.getByText('Monitored Resource Types', { exact: true })
      ).toBeVisible();
      await expect(
        page.getByText('Compute Instance', { exact: true }).first()
      ).toBeVisible();
    });

    test('renders the Meters card with at least one entry', async ({ page }) => {
      await expect(page.getByText('Meters', { exact: true }).first()).toBeVisible();
      // Seeded compute configuration includes vCPU Seconds.
      await expect(
        page.getByText('vCPU Seconds', { exact: true }).first()
      ).toBeVisible();
    });

    test('clicking the Active configuration heading navigates to the config detail page', async ({
      page,
    }) => {
      const activeConfigLink = page.getByRole('link', {
        name: /active configuration/i,
      });
      await expect(activeConfigLink).toBeVisible();

      await activeConfigLink.click();
      await page.waitForLoadState('networkidle');

      await expect(page).toHaveURL(
        /\/services\/compute-miloapis-com\/configurations\/[^/?#]+$/
      );
    });
  });

  test.describe('networking-miloapis-com (no Published configuration)', () => {
    test('Overview tab shows "No active configuration"', async ({ page }) => {
      await page.goto('/services/networking-miloapis-com');
      await page.waitForLoadState('networkidle');

      // Heading is rendered as a plain h3 (no link) when there is no
      // active config — assert the heading is still present, then assert
      // the empty-state copy.
      await expect(
        page.getByRole('heading', { name: /active configuration/i })
      ).toBeVisible();
      await expect(
        page.getByText('No active configuration', { exact: true })
      ).toBeVisible();
    });
  });
});
