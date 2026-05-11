import { test, expect } from '../fixtures';

test('services list and detail', async ({ page }) => {
  // ── Services list — show all phases ──────────────────────────────────────
  await page.goto('/services');
  await page.waitForLoadState('networkidle');
  await expect(page.locator('[data-e2e="page-title"]').filter({ hasText: /services/i })).toBeVisible();
  await page.waitForTimeout(3_000);

  // ── Service detail — Compute ──────────────────────────────────────────────
  const computeLink = page.getByRole('link', { name: 'compute-miloapis-com' });
  if (await computeLink.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await computeLink.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3_000);

    // ── Configurations tab ──────────────────────────────────────────────────
    const configsTab = page.getByRole('tab', { name: /configurations/i });
    if (await configsTab.isVisible()) {
      await configsTab.click();
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(3_000);
    }

    // ── Settings tab ────────────────────────────────────────────────────────
    const settingsTab = page.getByRole('tab', { name: /settings/i });
    if (await settingsTab.isVisible()) {
      await settingsTab.click();
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(2_500);
    }
  }
});
