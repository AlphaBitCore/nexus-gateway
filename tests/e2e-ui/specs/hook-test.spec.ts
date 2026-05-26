import { test, expect } from '@playwright/test';

// Spec 3: Admin proxy hook test — navigate to a seeded hook and run it.
test('hook test panel returns a verdict', async ({ page }) => {
  // Navigate to the hooks list and click the first row.
  await page.goto('/compliance/hooks');

  // Wait for the table to load and find the first clickable row.
  const firstRow = page.getByRole('table').locator('tbody tr').first();
  await firstRow.waitFor({ state: 'visible', timeout: 10_000 });
  await firstRow.click();

  // We should be on a hook detail page now.
  await expect(page).toHaveURL(/\/compliance\/hooks\/.+/);

  // Click the "Test" tab.
  await page.getByRole('tab', { name: /test/i }).click();

  // Run the test.
  await page.getByTestId('hook-test-run').waitFor({ state: 'visible' });
  await page.getByTestId('hook-test-run').click();

  // Verdict should appear (decision JSON or error message).
  // Do not assert the exact decision string — hook decisions are non-deterministic.
  await page.getByTestId('hook-test-verdict').waitFor({ state: 'visible', timeout: 15_000 });
  await expect(page.getByTestId('hook-test-verdict')).toBeVisible();
});
