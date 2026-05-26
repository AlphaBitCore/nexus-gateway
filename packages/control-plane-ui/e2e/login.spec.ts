import { test, expect } from '@playwright/test';

test.describe('Login Page', () => {
  test('shows login form', async ({ page }) => {
    await page.goto('/login');
    await expect(page.getByRole('heading', { name: /nexus gateway/i })).toBeVisible();
    await expect(page.getByLabel(/username/i)).toBeVisible();
    await expect(page.getByLabel(/password/i)).toBeVisible();
    await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible();
  });

  test('shows error on invalid credentials', async ({ page }) => {
    await page.goto('/login');
    await page.getByLabel(/username/i).fill('wronguser');
    await page.getByLabel(/password/i).fill('wrongpass');
    await page.getByRole('button', { name: /sign in/i }).click();
    await expect(page.getByRole('alert')).toBeVisible();
  });

  test('redirects to dashboard on successful login', async ({ page }) => {
    await page.goto('/login');
    await page.getByLabel(/username/i).fill('admin');
    await page.getByLabel(/password/i).fill('admin');
    await page.getByRole('button', { name: /sign in/i }).click();
    // Should redirect to dashboard
    await expect(page).toHaveURL('/', { timeout: 10_000 });
    await expect(page.getByText(/dashboard/i)).toBeVisible();
  });

  test('has language switcher', async ({ page }) => {
    await page.goto('/login');
    const langSelect = page.locator('select[aria-label]');
    await expect(langSelect).toBeVisible();
  });
});
