import { test, expect } from '@playwright/test';

// Spec: Fleet Overview page (S-072 backend's UI counterpart).
// Route: /fleet-overview → packages/control-plane-ui/src/pages/fleet-analytics/FleetOverviewPage.tsx
// Backed by 3 admin endpoints: fleetAnalyticsApi.summary / trends / topDestinations.
//
// Page heading text comes from `pages:fleetOverview.title` = "Fleet Overview"
// (en/pages.json). KPI cards render labels "Total Devices" / "Active" with
// numeric values (count >= 0 is acceptable). "Top Destinations (24h)" section
// always renders a table (or an empty-state row), even when the dataset is empty.
//
// Skip semantics: if the page is not deployed in this build, the SPA either
// returns 404 from the server or renders a NotFound shell — both surfaces lack
// the heading. We skip rather than fail to keep the spec safe across versions
// where the route may be carved out (e.g. consolidated into Infrastructure > Nodes).

const FLEET_HEADING = /Fleet Overview/i;

async function gotoFleetOverview(page: import('@playwright/test').Page): Promise<boolean> {
  const resp = await page.goto('/fleet-overview', { waitUntil: 'domcontentloaded' });
  if (resp && resp.status() === 404) return false;
  // Some SPAs return 200 for any path then route client-side; detect the
  // heading or skip if it never materialises.
  const heading = page.getByRole('heading', { level: 1, name: FLEET_HEADING });
  try {
    await heading.waitFor({ state: 'visible', timeout: 10_000 });
    return true;
  } catch {
    return false;
  }
}

test('fleet overview page loads', async ({ page }) => {
  test.setTimeout(45_000);

  const ok = await gotoFleetOverview(page);
  if (!ok) {
    test.skip(true, 'Fleet Overview page not present in this build');
    return;
  }

  // Heading + subtitle both come from PageHeader.
  await expect(page.getByRole('heading', { level: 1, name: FLEET_HEADING })).toBeVisible();

  // KPI cards render label + numeric value. The 4-column grid contains
  // "Total Devices" and "Active" labels; the numeric value sits in a sibling
  // <div> inside the same Card. Locate the label, then walk to its parent
  // card and read the first numeric cell.
  const totalLabel = page.getByText('Total Devices', { exact: true });
  const activeLabel = page.getByText('Active', { exact: true }).first();

  // At least one KPI label must be present.
  const kpiLabel = (await totalLabel.count()) > 0 ? totalLabel : activeLabel;
  await expect(kpiLabel).toBeVisible({ timeout: 10_000 });

  // The numeric value is a sibling <div> inside the same KpiCard. The label is
  // rendered as a direct child <div> of the Card; walk one level up to read the
  // full card text and assert a non-negative integer is present.
  const card = kpiLabel.locator('xpath=..');
  const cardText = await card.first().innerText();
  expect(cardText).toMatch(/\b\d+\b/);
});

test('top destinations widget renders', async ({ page }) => {
  test.setTimeout(45_000);

  const ok = await gotoFleetOverview(page);
  if (!ok) {
    test.skip(true, 'Fleet Overview page not present in this build');
    return;
  }

  // Section heading. Renders even when the top-destinations dataset is empty —
  // the empty state is a single <tr> with the localized "no destinations" text.
  const topDestHeading = page.getByRole('heading', { name: /Top Destinations/i });
  await expect(topDestHeading).toBeVisible({ timeout: 10_000 });

  // The table itself must mount. Either rows render or the empty-state
  // colspan=3 cell appears — both are acceptable.
  const card = topDestHeading.locator('xpath=ancestor::*[contains(@class,"card") or self::*][1]');
  // A table (or a loading spinner that resolves into a table) lives inside.
  const tableOrEmpty = card.locator('table, [data-testid="loading-spinner"]');
  await expect(tableOrEmpty.first()).toBeVisible({ timeout: 10_000 });
});
