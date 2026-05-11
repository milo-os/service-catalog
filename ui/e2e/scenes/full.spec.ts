import { test, expect } from '../fixtures';

test('full walkthrough', async ({ page }) => {
  // ── Consumer catalog ──────────────────────────────────────────────────────
  await page.goto('/catalog');
  await page.waitForLoadState('networkidle');
  await expect(
    page.getByRole('heading', { name: /service catalog/i })
  ).toBeVisible({ timeout: 15_000 });
  await page.waitForTimeout(3_000);

  // ── Services list ─────────────────────────────────────────────────────────
  await page.goto('/services');
  await page.waitForLoadState('networkidle');
  await expect(
    page.getByRole('heading', { name: /services/i }).first()
  ).toBeVisible({ timeout: 10_000 });
  await page.waitForTimeout(3_000);

  // ── Service detail — Compute ──────────────────────────────────────────────
  await page.getByRole('link', { name: 'compute-miloapis-com' }).click();
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);

  // ── Configurations tab ────────────────────────────────────────────────────
  const configsTab = page.getByRole('tab', { name: /configurations/i });
  if (await configsTab.isVisible()) {
    await configsTab.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3_000);
  }

  // ── New service wizard ────────────────────────────────────────────────────
  await page.goto('/services/new', { waitUntil: 'domcontentloaded' });
  await page.waitForLoadState('domcontentloaded');
  await expect(
    page.getByRole('heading', { name: /new service/i })
  ).toBeVisible({ timeout: 10_000 });
  await page.waitForTimeout(1_500);

  await page.getByLabel(/display name/i).pressSequentially('Analytics Platform', { delay: 60 });
  await page.waitForTimeout(800);
  await page.getByLabel(/description/i).pressSequentially(
    'Usage analytics and cost attribution for Milo-hosted workloads.',
    { delay: 30 }
  );
  await page.waitForTimeout(800);
  await page.getByLabel(/owner project/i).pressSequentially('platform-producer-project', {
    delay: 40,
  });
  await page.waitForTimeout(2_000);

  const nextBtn = page.getByRole('button', { name: /next/i });
  if (await nextBtn.isVisible()) {
    await nextBtn.click();
    await page.waitForTimeout(2_500);
  }

  // ── Outro: back to catalog ────────────────────────────────────────────────
  await page.goto('/catalog');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);
});
