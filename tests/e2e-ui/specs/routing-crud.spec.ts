import { test, expect } from '@playwright/test';

// Spec 2: Full CRUD round-trip for routing rules via UI.
test('routing rule CRUD via UI', async ({ page }) => {
  const uniqueName = `e2e-test-rule-${Date.now()}`;

  // Navigate to the routing rules list.
  await page.goto('/ai-gateway/routing');
  await expect(page.getByTestId('routing-rules-table')).toBeVisible();

  // Click the "New Rule" button.
  await page.getByTestId('routing-rule-new').click();
  await expect(page).toHaveURL(/\/ai-gateway\/routing\/new/);

  // Step 0: fill in the rule name.
  const nameInput = page.getByTestId('routing-rule-name');
  await nameInput.waitFor({ state: 'visible' });
  await nameInput.fill(uniqueName);

  // Advance to Step 1 (strategy configuration).
  const continueBtn = page.getByRole('button', { name: /continue/i });
  await continueBtn.click();

  // Step 1: select the first available provider and model for the "single" strategy.
  // The single-config wrapper is only rendered on step 1 with single strategy.
  const singleConfig = page.getByTestId('routing-single-config');
  await singleConfig.waitFor({ state: 'visible' });

  const providerSelect = singleConfig.locator('select').first();
  const providerOption = providerSelect.locator('option').nth(1); // skip the placeholder
  const firstProvider = await providerOption.getAttribute('value');
  if (firstProvider) {
    await providerSelect.selectOption(firstProvider);
    // Wait for models to populate then select first model.
    const modelSelect = singleConfig.locator('select').nth(1);
    await page.waitForFunction(
      (sel) => {
        const el = document.querySelector(sel) as HTMLSelectElement | null;
        return el && el.options.length > 1;
      },
      `[data-testid="routing-single-config"] select:nth-of-type(2)`,
      { timeout: 5_000 },
    );
    const modelOption = modelSelect.locator('option').nth(1);
    const firstModel = await modelOption.getAttribute('value');
    if (firstModel) await modelSelect.selectOption(firstModel);
  }

  // Continue through steps 2 and 3.
  await continueBtn.click(); // Step 1 → 2
  await continueBtn.click(); // Step 2 → 3

  // On the last step, click "Create Rule".
  const createBtn = page.getByRole('button', { name: /create rule/i });
  await createBtn.waitFor({ state: 'visible' });
  await createBtn.click();

  // Should redirect to the rule detail page or back to the list.
  await page.waitForURL((url) => !url.pathname.includes('/new'), { timeout: 15_000 });

  // Navigate back to the routing list and assert the new rule appears.
  await page.goto('/ai-gateway/routing');
  await expect(page.getByTestId('routing-rules-table')).toBeVisible();
  await expect(page.getByText(uniqueName)).toBeVisible();

  // Delete the rule via the row's Delete button.
  // DataTable renders <tr role="button">, so we locate by CSS + text filter.
  const deleteBtn = page.locator('tr').filter({ hasText: uniqueName })
    .getByRole('button', { name: /delete/i });
  await deleteBtn.click();

  // Confirm the deletion dialog.
  await page.getByRole('button', { name: /delete/i }).last().click();

  // Row should disappear.
  await expect(page.getByText(uniqueName)).not.toBeVisible({ timeout: 5_000 });
});
