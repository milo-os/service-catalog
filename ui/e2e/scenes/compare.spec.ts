import { test, expect } from '../fixtures';

test('configuration compare', async ({ page }) => {
  // ── Navigate to compare from the configurations tab ───────────────────────
  await page.goto('/services/compute-miloapis-com?tab=configurations');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);

  // ── Compare screen — empty pickers ───────────────────────────────────────
  await page.goto('/services/compute-miloapis-com/configurations/compare');
  await page.waitForLoadState('networkidle');
  await expect(
    page.locator('[data-e2e="page-title"]')
  ).toContainText(/compare configurations/i, { timeout: 10_000 });
  await page.waitForTimeout(3_000);

  // ── Populated diff (requires seed configs) ────────────────────────────────
  const left = 'compute-miloapis-com';
  const right = 'compute-miloapis-com-v1';

  const probe = await page.request.get(
    '/apis/services.miloapis.com/v1alpha1/serviceconfigurations'
  );
  if (probe.ok()) {
    const list = await probe.json().catch(() => null) as { items?: Array<{ metadata?: { name?: string } }> } | null;
    const names = new Set((list?.items ?? []).map(c => c.metadata?.name).filter(Boolean));

    if (names.has(left) && names.has(right)) {
      await page.goto(
        `/services/${left}/configurations/compare?left=${left}&right=${right}`
      );
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(4_000);
    }
  }
});
