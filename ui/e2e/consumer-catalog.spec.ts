import { test, expect } from './fixtures';

/**
 * E2E smoke test for the Consumer Service Catalog.
 *
 * Route (confirmed): /catalog
 *
 * Per the implementation in `catalog._index.tsx`, this is a card grid of
 * `Service`s where `spec.phase == "Published"`. Cards link to
 * `/services/:name`. Page heading is `<h1>Service catalog</h1>` with
 * subtitle "Browse services published for your projects." Empty state
 * uses `EmptyContent` with title "No services available yet."
 *
 * This smoke test asserts the heading renders and either ≥1 card link is
 * present or the empty state is shown.
 */
test.describe('/catalog — Consumer Service Catalog', () => {
  test('renders the heading and either ≥1 service card or the empty state', async ({
    page,
  }) => {
    await page.goto('/catalog');
    await page.waitForLoadState('domcontentloaded');

    await expect(
      page.getByRole('heading', { name: /service catalog/i })
    ).toBeVisible({ timeout: 10_000 });

    const firstCardLink = page.locator('a[href^="/services/"]').first();
    const emptyTitle = page.getByText(/no services available yet/i);

    await expect(firstCardLink.or(emptyTitle)).toBeVisible({ timeout: 10_000 });
  });
});
