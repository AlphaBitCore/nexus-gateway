import { test, expect } from '@playwright/test';

// Helper to login before each test
async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill('admin');
  await page.getByLabel(/password/i).fill('admin');
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL('/', { timeout: 10_000 });
}

test.describe('Provider Management', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('navigates to providers page and shows list', async ({ page }) => {
    await page.goto('/config/providers');

    // Page header should be visible
    await expect(page.getByText(/providers/i).first()).toBeVisible();

    // The DataTable renders column headers
    await expect(page.getByText(/name/i).first()).toBeVisible();
    await expect(page.getByText(/base url/i).first()).toBeVisible();
    await expect(page.getByText(/status/i).first()).toBeVisible();
  });

  test('has a create provider button', async ({ page }) => {
    await page.goto('/config/providers');

    // The "Create Provider" / "Connect an AI provider" button should be visible
    const createButton = page.getByRole('button', { name: /create|connect|add/i });
    await expect(createButton).toBeVisible();
  });

  test('navigates to provider wizard on create', async ({ page }) => {
    await page.goto('/config/providers');

    // Click the create / add provider button
    const createButton = page.getByRole('button', { name: /create|connect|add/i });
    await createButton.click();

    // Should navigate to the provider creation wizard
    await expect(page).toHaveURL(/\/config\/providers\/new/);

    // The wizard page should show step content
    await expect(page.getByText(/connect an ai provider/i)).toBeVisible();
  });

  test('provider wizard shows template selection step', async ({ page }) => {
    await page.goto('/config/providers/new');

    // Step 1: template selection
    // Common provider templates like OpenAI, Anthropic should appear
    await expect(page.getByText(/connect an ai provider/i)).toBeVisible();

    // The step indicator bar should be visible
    await expect(page.getByText(/template/i).first()).toBeVisible();
  });

  test('search filter works on provider list', async ({ page }) => {
    await page.goto('/config/providers');

    // The search input should be present
    const searchInput = page.getByPlaceholder(/search/i);
    await expect(searchInput).toBeVisible();

    // Type a search query
    await searchInput.fill('nonexistent-provider-xyz');

    // Wait for debounced search to take effect
    await page.waitForTimeout(500);

    // Should show "no providers match" or an empty state
    await expect(
      page.getByText(/no providers/i).or(page.getByText(/0 total/i)),
    ).toBeVisible({ timeout: 5_000 });
  });

  test('status filter dropdown is available', async ({ page }) => {
    await page.goto('/config/providers');

    // The status filter dropdown should be present
    const statusFilter = page.getByRole('combobox', { name: /status/i })
      .or(page.locator('select[aria-label*="status" i]'));
    await expect(statusFilter).toBeVisible();
  });

  test('clicking a provider row navigates to detail', async ({ page }) => {
    await page.goto('/config/providers');

    // If there are provider rows, clicking one should navigate to its detail
    const rows = page.locator('table tbody tr');
    const rowCount = await rows.count();

    if (rowCount > 0) {
      await rows.first().click();
      // Should navigate to provider detail page
      await expect(page).toHaveURL(/\/config\/providers\/[a-zA-Z0-9-]+$/);
    }
  });
});
