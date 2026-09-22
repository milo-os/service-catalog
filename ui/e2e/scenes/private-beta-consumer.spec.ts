import { test } from '../fixtures';

test('private beta access request', async ({ page }) => {
  // ── Catalog landing ──────────────────────────────────────────────
  await page.goto('/catalog');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);

  // ── Search for AI Platform card ──────────────────────────────────
  const search = page.getByRole('searchbox');
  if (await search.isVisible({ timeout: 5_000 }).catch(() => false)) {
    await search.pressSequentially('AI', { delay: 80 });
    await page.waitForTimeout(2_000);
    await search.clear();
    await page.waitForTimeout(1_000);
  }

  // ── Open AI Platform service detail ─────────────────────────────
  const aiCard = page.locator('a[href*="ai-platform"]').first();
  if (await aiCard.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await aiCard.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2_500);
  }

  // ── Scroll to request form ───────────────────────────────────────
  const textarea = page.locator('textarea').first();
  if (await textarea.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await textarea.scrollIntoViewIfNeeded();
    await textarea.click();
    await textarea.pressSequentially(
      "We're building an internal document search tool and would like to evaluate the embedding APIs during the beta period.",
      { delay: 40 }
    );
    await page.waitForTimeout(1_500);
  }

  // ── Submit access request ────────────────────────────────────────
  const requestBtn = page.getByRole('button', { name: /request.access/i });
  if (await requestBtn.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await requestBtn.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3_000);
  }

  // ── Entitlements landing ─────────────────────────────────────────
  const entitlementsHeading = page.getByRole('heading', {
    name: /my service access|entitlements/i,
  });
  if (
    await entitlementsHeading
      .isVisible({ timeout: 5_000 })
      .catch(() => false)
  ) {
    await page.waitForTimeout(2_500);
  } else {
    await page.waitForTimeout(2_000);
  }

  // ── PendingApproval status badge ─────────────────────────────────
  const pendingBadge = page.getByText(/pending.?approval/i).first();
  if (await pendingBadge.isVisible({ timeout: 3_000 }).catch(() => false)) {
    await pendingBadge.scrollIntoViewIfNeeded();
  }
  await page.waitForTimeout(2_500);
});
