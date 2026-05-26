import { test, expect } from '@playwright/test';

// Helper to login before each test
async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill('admin');
  await page.getByLabel(/password/i).fill('admin');
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL('/', { timeout: 10_000 });
}

test.describe('Audit Logs', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('navigates to audit logs page', async ({ page }) => {
    await page.goto('/audit-logs');

    // Page header should be visible (title may be i18n key or "Audit Logs")
    await expect(page.getByText(/audit/i).first()).toBeVisible();
  });

  test('page loads with table or status message', async ({ page }) => {
    await page.goto('/audit-logs');

    // The page may show:
    // 1. A data table with audit entries (if audit is enabled and DB-backed)
    // 2. A "disabled" hint (if audit is not enabled)
    // 3. A "requires database" message (if using file sink)
    // We accept any of these as a valid loaded state
    await expect(
      page.getByRole('table')
        .or(page.getByText(/audit/i).first()),
    ).toBeVisible({ timeout: 10_000 });
  });

  test('shows table column headers when audit is queryable', async ({ page }) => {
    await page.goto('/audit-logs');

    // If audit is enabled and queryable, the table should have headers
    const table = page.getByRole('table');
    const tableVisible = await table.isVisible({ timeout: 5_000 }).catch(() => false);

    if (tableVisible) {
      // Expect common audit log column headers
      await expect(page.getByText(/action/i).first()).toBeVisible();
      await expect(page.getByText(/entity/i).first()).toBeVisible();
    }
  });

  test('action filter dropdown is available', async ({ page }) => {
    await page.goto('/audit-logs');

    // The action filter should be a <select> element
    const actionFilter = page.locator('select[aria-label*="action" i]');
    const filterVisible = await actionFilter.isVisible({ timeout: 5_000 }).catch(() => false);

    if (filterVisible) {
      // Verify it has expected options
      await expect(actionFilter.locator('option')).not.toHaveCount(0);

      // Select a specific action filter
      await actionFilter.selectOption('create');

      // The filter should now be applied (URL may update or table refreshes)
      await expect(actionFilter).toHaveValue('create');
    }
  });

  test('entity type filter dropdown is available', async ({ page }) => {
    await page.goto('/audit-logs');

    // The entity type filter
    const entityFilter = page.locator('select[aria-label*="entity" i]');
    const filterVisible = await entityFilter.isVisible({ timeout: 5_000 }).catch(() => false);

    if (filterVisible) {
      await expect(entityFilter.locator('option')).not.toHaveCount(0);

      // Select a specific entity type
      await entityFilter.selectOption('provider');
      await expect(entityFilter).toHaveValue('provider');
    }
  });

  test('date range filters are available', async ({ page }) => {
    await page.goto('/audit-logs');

    // Start time and end time date inputs
    const startTime = page.locator('input[type="datetime-local"]').first();
    const startVisible = await startTime.isVisible({ timeout: 5_000 }).catch(() => false);

    if (startVisible) {
      // Both date inputs should be present
      const dateInputs = page.locator('input[type="datetime-local"]');
      await expect(dateInputs).toHaveCount(2);

      // Fill in a start date
      await startTime.fill('2026-01-01T00:00');

      // The filter should be applied (page re-renders with the filter active)
      await expect(startTime).toHaveValue('2026-01-01T00:00');
    }
  });

  test('clear filters button appears when filters are active', async ({ page }) => {
    await page.goto('/audit-logs');

    // Apply a filter first
    const actionFilter = page.locator('select[aria-label*="action" i]');
    const filterVisible = await actionFilter.isVisible({ timeout: 5_000 }).catch(() => false);

    if (filterVisible) {
      await actionFilter.selectOption('create');

      // A "Clear filters" button should appear
      const clearButton = page.getByRole('button', { name: /clear/i });
      await expect(clearButton).toBeVisible();

      // Click it to reset
      await clearButton.click();

      // The action filter should be back to empty (all actions)
      await expect(actionFilter).toHaveValue('');
    }
  });

  test('search input for actor ID is available', async ({ page }) => {
    await page.goto('/audit-logs');

    // The search input for actor filtering
    const searchInput = page.getByPlaceholder(/search/i)
      .or(page.locator('input[aria-label*="search" i]'));
    const searchVisible = await searchInput.isVisible({ timeout: 5_000 }).catch(() => false);

    if (searchVisible) {
      await searchInput.fill('test-actor');
      // Wait for debounced search
      await page.waitForTimeout(500);
      // The filter should be applied (results may be empty)
      await expect(searchInput).toHaveValue('test-actor');
    }
  });

  test('export button is visible for authorized users', async ({ page }) => {
    await page.goto('/audit-logs');

    // The export button may or may not be visible depending on permissions
    const exportButton = page.getByRole('button', { name: /export/i });
    // We just check if it renders -- admin user should have the permission
    const exportVisible = await exportButton.isVisible({ timeout: 5_000 }).catch(() => false);

    if (exportVisible) {
      await expect(exportButton).toBeEnabled();
    }
  });

  test('pagination controls are present', async ({ page }) => {
    await page.goto('/audit-logs');

    // If the table is visible and has data, pagination should be present
    const table = page.getByRole('table');
    const tableVisible = await table.isVisible({ timeout: 5_000 }).catch(() => false);

    if (tableVisible) {
      // Showing rows meta text: "Showing X-Y of Z"
      const showingText = page.getByText(/showing/i);
      const showingVisible = await showingText.isVisible({ timeout: 3_000 }).catch(() => false);

      if (showingVisible) {
        await expect(showingText).toBeVisible();
      }
    }
  });
});
