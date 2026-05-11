import { test as base, expect, type Page } from '@playwright/test';

async function assertShellRendered(page: Page) {
  await expect(page.locator('[data-sidebar="sidebar"]')).toBeVisible({ timeout: 10_000 });
  await expect(page.locator('[data-sidebar="trigger"] svg')).toBeVisible();

  // Verify datum-ui styles are loaded. --color-icon-primary is a datum-ui design
  // token not defined by Tailwind core — it's absent when the @import is missing.
  const iconColor = await page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue('--color-icon-primary').trim()
  );
  expect(iconColor, 'datum-ui styles not loaded — @import "@datum-cloud/datum-ui/styles" missing from index.css').toBeTruthy();
}

export const test = base.extend<object>({
  page: async ({ page }, use) => {
    const originalGoto = page.goto.bind(page);
    page.goto = async (url, options) => {
      const response = await originalGoto(url, options);
      await assertShellRendered(page);
      return response;
    };
    await use(page);
  },
});

export { expect } from '@playwright/test';
