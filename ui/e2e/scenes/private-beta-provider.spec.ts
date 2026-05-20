import { test, expect } from '../fixtures';

test('provider approval workflow', async ({ page }) => {
  // ── Access requests list ─────────────────────────────────────────────────
  await page.goto('/consumers');
  await page.waitForLoadState('networkidle');
  await expect(
    page.locator('[data-e2e="page-title"]').filter({ hasText: /access requests/i })
  ).toBeVisible().catch(() => undefined);
  await page.waitForTimeout(3_000);

  // ── Pending request callout ──────────────────────────────────────────────
  const pendingBadge = page.getByText(/pending/i).first();
  if (await pendingBadge.isVisible({ timeout: 2_000 }).catch(() => false)) {
    await page.waitForTimeout(2_000);
  }

  // ── Click Review link for first pending request ──────────────────────────
  let reviewLink = page.getByRole('link', { name: /review/i }).first();
  const reviewVisible = await reviewLink.isVisible({ timeout: 3_000 }).catch(() => false);
  if (reviewVisible) {
    await reviewLink.click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2_500);

    // ── Request detail — let viewer read consumer project + service name ─────
    await page.waitForTimeout(2_500);

    // ── Fill optional approval message ───────────────────────────────────────
    let messageField = page.getByRole('textbox');
    const textboxVisible = await messageField.isVisible({ timeout: 2_000 }).catch(() => false);
    if (!textboxVisible) {
      messageField = page.locator('textarea');
    }
    if (await messageField.isVisible({ timeout: 2_000 }).catch(() => false)) {
      await messageField.click();
      await messageField.pressSequentially(
        'Welcome to the AI Platform private beta. Your access is now active.',
        { delay: 45 }
      );
      await page.waitForTimeout(1_500);
    }

    // ── Approve ───────────────────────────────────────────────────────────────
    const approveBtn = page.getByRole('button', { name: /approve/i });
    if (await approveBtn.isVisible({ timeout: 3_000 }).catch(() => false)) {
      await approveBtn.click();
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(3_000);

      // ── Back on /consumers — show updated list with Active badge ─────────────
      const accessRequestsHeading = page
        .locator('[data-e2e="page-title"]')
        .filter({ hasText: /access requests/i });
      if (await accessRequestsHeading.isVisible({ timeout: 5_000 }).catch(() => false)) {
        await page.waitForTimeout(2_500);
      }
      await page.waitForTimeout(2_000);
    }
  } else {
    // ── Fallback: try direct consumer link ───────────────────────────────────
    const fallbackLink = page.locator('a[href^="/consumers/"]').first();
    if (await fallbackLink.isVisible({ timeout: 3_000 }).catch(() => false)) {
      await fallbackLink.click();
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(2_500);

      await page.waitForTimeout(2_500);

      let messageField = page.getByRole('textbox');
      const textboxVisible = await messageField.isVisible({ timeout: 2_000 }).catch(() => false);
      if (!textboxVisible) {
        messageField = page.locator('textarea');
      }
      if (await messageField.isVisible({ timeout: 2_000 }).catch(() => false)) {
        await messageField.click();
        await messageField.pressSequentially(
          'Welcome to the AI Platform private beta. Your access is now active.',
          { delay: 45 }
        );
        await page.waitForTimeout(1_500);
      }

      const approveBtn = page.getByRole('button', { name: /approve/i });
      if (await approveBtn.isVisible({ timeout: 3_000 }).catch(() => false)) {
        await approveBtn.click();
        await page.waitForLoadState('networkidle');
        await page.waitForTimeout(3_000);

        const accessRequestsHeading = page
          .locator('[data-e2e="page-title"]')
          .filter({ hasText: /access requests/i });
        if (await accessRequestsHeading.isVisible({ timeout: 5_000 }).catch(() => false)) {
          await page.waitForTimeout(2_500);
        }
        await page.waitForTimeout(2_000);
      }
    }
  }
});
