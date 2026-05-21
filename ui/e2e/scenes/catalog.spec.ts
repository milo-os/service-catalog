import { test, expect } from '../fixtures';

test('consumer catalog', async ({ page }) => {
  await page.goto('/catalog');
  await page.waitForLoadState('networkidle');
  await expect(
    page.getByRole('heading', { name: /service catalog/i })
  ).toBeVisible({ timeout: 15_000 });
  await page.waitForTimeout(3_000);

  // Search to demonstrate filtering
  const search = page.getByRole('searchbox');
  if (await search.isVisible()) {
    await search.pressSequentially('compute', { delay: 80 });
    await page.waitForTimeout(2_000);
    await search.clear();
    await page.waitForTimeout(1_500);
  }

  // Click into the first service card
  const firstCard = page.locator('a[href^="/services/"]').first();
  if (await firstCard.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await firstCard.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3_000);
  }
});
