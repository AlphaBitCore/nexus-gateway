import { test, expect } from '@playwright/test';

// Spec 5: Traffic audit drawer — click a row and verify drilldown sections.
test('audit drawer shows hooks and routing sections', async ({ page }) => {
  await page.goto('/traffic');

  // Wait for the traffic table to load.
  await expect(page.getByTestId('traffic-table')).toBeVisible({ timeout: 15_000 });

  // Find the first data row in the table.
  const firstRow = page.locator('[data-testid="traffic-table"] table tbody tr').first();

  const rowCount = await firstRow.count();
  if (rowCount === 0) {
    test.skip(true, 'no traffic rows available — run traffic-monitor spec first');
    return;
  }

  await firstRow.click();

  // Drawer should open. It defaults to the Overview tab — Hooks and
  // Routing live behind their own tab buttons (one content panel at a
  // time), so we click each and assert visibility separately.
  await expect(page.getByTestId('traffic-row-drawer')).toBeVisible({ timeout: 5_000 });

  // Compliance tab → audit-drawer-hooks-tab content (empty state OK).
  await page.getByTestId('audit-drawer-tab-compliance').click();
  await expect(page.getByTestId('audit-drawer-hooks-tab')).toBeVisible({ timeout: 5_000 });

  // AI & Routing tab is only present for ai-gateway-source rows. If the
  // first row is a compliance-proxy row, the tab won't exist; in that
  // case the spec exercises only the Compliance pane.
  const aiTab = page.getByTestId('audit-drawer-tab-ai');
  if (await aiTab.count() > 0) {
    await aiTab.click();
    await expect(page.getByTestId('audit-drawer-routing-tab')).toBeVisible({ timeout: 5_000 });
  }
});
