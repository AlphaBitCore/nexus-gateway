import { test, expect } from '@playwright/test';

// Helper to login before each test
async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill('admin');
  await page.getByLabel(/password/i).fill('admin');
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL('/', { timeout: 10_000 });
}

test.describe('Virtual Key Management', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('navigates to virtual keys page and shows list', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    // Page header should be visible
    await expect(page.getByText(/virtual keys/i).first()).toBeVisible();

    // The DataTable renders column headers
    await expect(page.getByText(/slug/i).first()).toBeVisible();
    await expect(page.getByText(/status/i).first()).toBeVisible();
  });

  test('has a create virtual key button', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    // The "Create Virtual Key" button should be visible
    const createButton = page.getByRole('button', { name: /create virtual key/i });
    await expect(createButton).toBeVisible();
  });

  test('navigates to create form on button click', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    const createButton = page.getByRole('button', { name: /create virtual key/i });
    await createButton.click();

    // Should navigate to the virtual key creation page
    await expect(page).toHaveURL(/\/security\/virtual-keys\/new/);

    // The create form should show the slug input
    await expect(page.getByText(/create virtual key/i).first()).toBeVisible();
  });

  test('create form has required fields', async ({ page }) => {
    await page.goto('/security/virtual-keys/new');

    // Slug field is required
    await expect(page.getByText(/slug/i).first()).toBeVisible();

    // Enabled switch should be present
    await expect(page.getByText(/enabled/i).first()).toBeVisible();

    // Submit button
    const submitButton = page.getByRole('button', { name: /create virtual key/i });
    await expect(submitButton).toBeVisible();

    // Cancel button
    const cancelButton = page.getByRole('button', { name: /cancel/i });
    await expect(cancelButton).toBeVisible();
  });

  test('fill and submit create virtual key form', async ({ page }) => {
    await page.goto('/security/virtual-keys/new');

    // Generate a unique slug for this test run
    const testSlug = `e2e-test-vk-${Date.now()}`;

    // Fill in the slug
    const slugInput = page.locator('input[name="slug"]');
    await slugInput.fill(testSlug);

    // Submit the form
    const submitButton = page.getByRole('button', { name: /create virtual key/i });
    await submitButton.click();

    // After creation, the success screen should appear with the secret key
    await expect(
      page.getByText(/virtual key created/i).or(page.getByText(/secret key/i)),
    ).toBeVisible({ timeout: 10_000 });
  });

  test('copy secret key after creation', async ({ page }) => {
    await page.goto('/security/virtual-keys/new');

    const testSlug = `e2e-copy-test-${Date.now()}`;

    // Fill in the slug
    const slugInput = page.locator('input[name="slug"]');
    await slugInput.fill(testSlug);

    // Submit the form
    const submitButton = page.getByRole('button', { name: /create virtual key/i });
    await submitButton.click();

    // Wait for the success screen
    await expect(page.getByText(/secret key/i)).toBeVisible({ timeout: 10_000 });

    // Click the copy button
    const copyButton = page.getByRole('button', { name: /copy/i });
    await expect(copyButton).toBeVisible();
    await copyButton.click();

    // After copying, the button text should change to "Copied"
    await expect(page.getByRole('button', { name: /copied/i })).toBeVisible();
  });

  test('search filter works on virtual key list', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    // The search input should be present
    const searchInput = page.getByPlaceholder(/search/i);
    await expect(searchInput).toBeVisible();

    // Type a search query
    await searchInput.fill('nonexistent-key-xyz');

    // Wait for debounced search
    await page.waitForTimeout(500);

    // Should show empty state or zero results
    await expect(
      page.getByText(/no virtual keys/i).or(page.getByText(/0 total/i)),
    ).toBeVisible({ timeout: 5_000 });
  });

  test('project filter dropdown is available', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    // The project filter dropdown should be present
    const projectFilter = page.locator('select[aria-label*="project" i]');
    await expect(projectFilter).toBeVisible();
  });

  test('status filter dropdown is available', async ({ page }) => {
    await page.goto('/security/virtual-keys');

    // The status filter dropdown should be present
    const statusFilter = page.locator('select[aria-label*="status" i]');
    await expect(statusFilter).toBeVisible();
  });

  test('navigate back to list from create page', async ({ page }) => {
    await page.goto('/security/virtual-keys/new');

    // Cancel button should navigate back
    const cancelButton = page.getByRole('button', { name: /cancel/i });
    await cancelButton.click();

    await expect(page).toHaveURL(/\/security\/virtual-keys$/);
  });
});
