import { test, expect } from '../fixtures';

test('new service wizard', async ({ page }) => {
  await page.goto('/services/new', { waitUntil: 'domcontentloaded' });
  await page.waitForLoadState('domcontentloaded');

  await expect(page.locator('h1').filter({ hasText: /new service/i })).toBeVisible({ timeout: 10_000 });
  await page.waitForTimeout(500);

  // ── Step 1 — Service identity ─────────────────────────────────────────────
  await page.getByLabel(/display name/i).fill('Analytics Platform');
  await page.waitForTimeout(300);

  await page.getByLabel(/description/i).fill(
    'Usage analytics and cost attribution for Milo-hosted workloads.'
  );
  await page.waitForTimeout(300);

  await page.getByLabel(/owner project/i).fill('platform-producer-project');
  await page.waitForTimeout(1_000);

  // ── Step 2 — Monitored resource types ─────────────────────────────────────
  const nextBtn = page.getByRole('button', { name: /next/i });
  await nextBtn.click({ timeout: 5_000 });
  await page.waitForTimeout(1_000);

  const addMrtBtn = page.getByRole('button', { name: /add.*resource type|add mrt/i }).first();
  await expect(addMrtBtn).toBeVisible({ timeout: 5_000 });
  await addMrtBtn.click({ timeout: 3_000 });
  await page.waitForTimeout(300);

  await expect(page.getByPlaceholder('e.g. compute-instance').first()).toBeVisible({ timeout: 3_000 });
  await page.locator('input#mrt-0-type').fill('analytics-job');
  await page.locator('input#mrt-0-displayName').fill('Analytics Job');
  await page.locator('input#mrt-0-group').fill('analytics.miloapis.com');
  await page.locator('input#mrt-0-kind').fill('Job');
  await page.waitForTimeout(1_000);

  // ── Step 3 — Meters ───────────────────────────────────────────────────────
  await nextBtn.click({ timeout: 5_000 });
  await page.waitForTimeout(1_500);

  // ── Step 4 — Review ───────────────────────────────────────────────────────
  await nextBtn.click({ timeout: 5_000 });
  await page.waitForTimeout(1_500);
});
