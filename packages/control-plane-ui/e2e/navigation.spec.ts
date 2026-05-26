import { test, expect } from '@playwright/test';

// Helper to login before each test
async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill('admin');
  await page.getByLabel(/password/i).fill('admin');
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL('/', { timeout: 10_000 });
}

test.describe('Navigation', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('dashboard loads with metrics', async ({ page }) => {
    await expect(page.getByText(/dashboard/i)).toBeVisible();
  });

  test('navigate to providers page', async ({ page }) => {
    await page.getByRole('link', { name: /providers/i }).click();
    await expect(page.getByText(/providers/i)).toBeVisible();
  });

  test('navigate to routing rules page', async ({ page }) => {
    await page.getByRole('link', { name: /routing/i }).click();
    await expect(page.getByText(/routing rules/i)).toBeVisible();
  });

  test('navigate to traffic page', async ({ page }) => {
    await page.getByRole('link', { name: /traffic/i }).click();
    await expect(page.getByText(/traffic/i)).toBeVisible();
  });

  test('theme toggle works', async ({ page }) => {
    // Find theme toggle button and click
    const html = page.locator('html');
    const initialTheme = await html.getAttribute('data-theme');

    // Click theme toggle (sun/moon icon)
    await page.locator('button[aria-label*="Theme"]').click();
    // Select dark
    await page.getByText(/dark/i).click();

    await expect(html).toHaveAttribute('data-theme', 'dark');
  });
});
