import { test, expect } from '@playwright/test';

// Spec 6: Regression guard for the useApi queryKey cache-leak bug.
//
// The bug: pages that share the same state-variable shape (debouncedSearch,
// offset, limit) produced identical queryKeys — navigating from providers
// to hooks showed the providers data under the hooks heading.
//
// This test detects the regression: the local seed has clearly different
// counts for providers (5) vs hooks (12), so if the queryKey cache leaks,
// both pages would render the same count from the first fetch and the
// assertion fails.
test('providers and hooks pages do not share cached data', async ({ page }) => {
  // Navigate to providers, wait for the table.
  await page.goto('/ai-gateway/providers');
  await expect(page.getByTestId('providers-table')).toBeVisible({ timeout: 10_000 });

  // Count the provider rows (tbody rows only — excludes the header row).
  const providerRowCount = await page
    .locator('[data-testid="providers-table"] table tbody tr')
    .count();

  // Navigate to hooks (compliance section), wait for its DataTable.
  await page.goto('/compliance/hooks');
  await expect(page.getByRole('table')).toBeVisible({ timeout: 10_000 });

  const hookRowCount = await page.getByRole('table').locator('tbody tr').count();

  // If the queryKey cache leaks the providers data would appear under the
  // hooks heading and the counts would match.
  expect(providerRowCount).not.toEqual(hookRowCount);
});
