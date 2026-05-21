import { test, expect } from '../fixtures';

const SERVICE = 'compute-miloapis-com';

test('new configuration wizard', async ({ page }) => {
  await page.goto(`/services/${SERVICE}/configurations/new`, { waitUntil: 'domcontentloaded' });
  await page.waitForLoadState('domcontentloaded');

  await expect(page.locator('h1').first()).toBeVisible({ timeout: 15_000 });

  // Fail fast if the loader couldn't fetch the service from k8s.
  await expect(page.getByRole('heading', { name: /couldn.*t load service/i })).not.toBeVisible();

  await page.waitForTimeout(1_500);

  // ── Step 1 — Version & source ─────────────────────────────────────────────
  // Target the version input directly by id since getByLabel resolution
  // depends on datum-ui's Label rendering which may vary.
  const versionInput = page.locator('input#version');
  await expect(versionInput).toBeVisible({ timeout: 15_000 });
  await versionInput.fill('2.0.0');
  await page.waitForTimeout(1_500);

  // ── Step 2 — Monitored resource types ─────────────────────────────────────
  const nextButton = page.getByRole('button', { name: /^next/i });
  await nextButton.click({ timeout: 5_000 });
  await page.waitForTimeout(2_500);

  // ── Step 3 — Meters ───────────────────────────────────────────────────────
  await nextButton.click({ timeout: 5_000 });
  await page.waitForTimeout(2_500);

  // ── Step 4 — Review & create ──────────────────────────────────────────────
  await nextButton.click({ timeout: 5_000 });
  await page.waitForTimeout(2_500);
});
