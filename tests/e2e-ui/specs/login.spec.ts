import { test, expect } from '@playwright/test';

// Spec 1: Sanity — global-setup already authenticated; this just verifies
// the storageState is applied correctly and the shell renders.
test('admin lands on dashboard after login', async ({ page }) => {
  await page.goto('/');
  // Shell nav must be in the DOM (indicates successful auth + app mount).
  // Use toBeAttached so the assertion passes even when the sidebar is
  // off-screen in collapsed/mobile layout.
  await expect(page.getByTestId('shell-nav')).toBeAttached();
  await expect(page).toHaveURL('/');
});
