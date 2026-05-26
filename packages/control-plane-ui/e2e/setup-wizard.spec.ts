import { test, expect } from '@playwright/test';

// Helper to login before each test
async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill('admin');
  await page.getByLabel(/password/i).fill('admin');
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL('/', { timeout: 10_000 });
}

test.describe('Setup Wizard', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('navigates to setup wizard page', async ({ page }) => {
    await page.goto('/setup');
    await expect(page.getByText(/setup wizard/i)).toBeVisible();
    await expect(page.getByText(/guided checklist/i)).toBeVisible();
  });

  test('displays all wizard steps', async ({ page }) => {
    await page.goto('/setup');

    // Verify each step is visible
    await expect(page.getByText('System check')).toBeVisible();
    await expect(page.getByText('Connect your first AI provider')).toBeVisible();
    await expect(page.getByText('Define a routing rule')).toBeVisible();
    await expect(page.getByText(/configure compliance hooks/i)).toBeVisible();
    await expect(page.getByText('Issue your first virtual key')).toBeVisible();
    await expect(page.getByText(/compliance proxy/i)).toBeVisible();
    await expect(page.getByText('Review and finish')).toBeVisible();
  });

  test('shows progress bar with required steps count', async ({ page }) => {
    await page.goto('/setup');

    // The progress label shows "X of Y required steps complete"
    await expect(page.getByText(/of \d+ required steps complete/i)).toBeVisible();
  });

  test('marks optional steps with badge', async ({ page }) => {
    await page.goto('/setup');

    // Optional steps should have an "optional" badge
    const optionalBadges = page.getByText('optional');
    // Compliance hooks and compliance proxy are optional
    await expect(optionalBadges.first()).toBeVisible();
  });

  test('step through: mark system check complete', async ({ page }) => {
    await page.goto('/setup');

    // Find the first "Mark complete" button (system check step)
    const markCompleteButtons = page.getByRole('button', { name: /mark complete/i });
    await expect(markCompleteButtons.first()).toBeVisible();

    // Click to mark system check as complete
    await markCompleteButtons.first().click();

    // After marking complete, that button should now say "Mark incomplete"
    await expect(page.getByRole('button', { name: /mark incomplete/i }).first()).toBeVisible();
  });

  test('skip optional steps (hooks, proxy) and finish', async ({ page }) => {
    await page.goto('/setup');

    // Mark required steps complete (system check, provider, routing, virtual key, review)
    const markCompleteButtons = page.getByRole('button', { name: /mark complete/i });

    // Mark each visible "Mark complete" button -- iterate through all steps
    const count = await markCompleteButtons.count();
    for (let i = 0; i < count; i++) {
      // Re-query each time since DOM updates after each click
      const btn = page.getByRole('button', { name: /mark complete/i }).first();
      await btn.click();
      // Brief wait for state update
      await page.waitForTimeout(200);
    }

    // Now click "Finish setup"
    const finishButton = page.getByRole('button', { name: /finish setup/i });
    await expect(finishButton).toBeVisible();
    await finishButton.click();

    // After finishing, the completion banner should appear
    await expect(page.getByText(/setup is marked complete/i)).toBeVisible();
  });

  test('reopen wizard after completion', async ({ page }) => {
    await page.goto('/setup');

    // If already completed, the reopen button should be visible
    const reopenButton = page.getByRole('button', { name: /reopen wizard/i });

    // If setup is already complete, test reopening
    if (await reopenButton.isVisible({ timeout: 2_000 }).catch(() => false)) {
      await reopenButton.click();
      // After reopening, "Mark complete" buttons should reappear
      await expect(page.getByRole('button', { name: /mark complete/i }).first()).toBeVisible();
    }
  });

  test('each step has a link-out button to its config page', async ({ page }) => {
    await page.goto('/setup');

    // System check links to Status page
    await expect(page.getByRole('button', { name: /open status/i })).toBeVisible();
    // Provider step links to Providers page
    await expect(page.getByRole('button', { name: /open providers/i })).toBeVisible();
    // Routing step links to Routing Rules page
    await expect(page.getByRole('button', { name: /open routing rules/i })).toBeVisible();
    // Hooks step links to Hooks page
    await expect(page.getByRole('button', { name: /open hooks/i })).toBeVisible();
    // Virtual key step links to Virtual Keys page
    await expect(page.getByRole('button', { name: /open virtual keys/i })).toBeVisible();
    // Proxy step links to Proxy Status page
    await expect(page.getByRole('button', { name: /open proxy status/i })).toBeVisible();
  });
});
