import { test, expect } from '@playwright/test';

// Spec: Cost stamping UI surface (E58) — verifies that estimatedCostUsd
// renders as USD-shaped numeric text on the two top-level cost surfaces:
//   1. Analytics page (`/analytics`) — KPI strip + breakdown table.
//      Source: packages/control-plane-ui/src/pages/analytics/AnalyticsPage.tsx
//   2. Provider detail → Usage tab (`/ai-gateway/providers/:id` → "Usage").
//      Source: packages/control-plane-ui/src/pages/ai-gateway/providers/detail/ProviderUsageTab.tsx
//
// We are not asserting specific dollar values (data depends on local fixtures)
// — only that the page renders past skeleton AND at least one cell on the
// surface holds a currency-shaped string (or the documented em-dash
// placeholder for empty/null cells).
//
// USD-shaped strings produced by `formatUsd` (packages/control-plane-ui/src/lib/format.ts):
//   "$0"               — zero state
//   "$0.00"            — KPI strip (precision=2)
//   "$1.2345"          — breakdown / usage table (max 4 frac digits)
//   "$0.000123"        — sub-cent value (max 6 frac digits)
//   "<$0.000001"       — below-resolution floor
//   ">-$0.000001"      — negative floor
//   "-$0.0050"         — net negative
// Plus the em-dash placeholder "—" rendered when a cost field is null.
// The regex accepts: digits, dot, comma, $, -, <, >, em-dash, whitespace.

const USD_LIKE = /^[\s$\d.,\-—<>]+$/;

test('analytics page cost cells render', async ({ page }) => {
  const resp = await page.goto('/analytics');

  // Skip if the route is not registered (feature flagged off / older build).
  if (resp && resp.status() === 404) {
    test.skip(true, 'analytics route not available');
    return;
  }

  // PageHeader renders i18n key pages:traffic.analytics = "Traffic & Analytics"
  // for the section title text. Use a permissive substring match so the spec
  // doesn't break when the en bundle is retitled.
  const heading = page.locator('h1', { hasText: /Analytics/i });
  await expect(heading).toBeVisible({ timeout: 15_000 });

  // KPI strip: each StatCard renders an AnimatedNumber. The "Total Cost" card
  // formats with `$${n.toFixed(2)}`. The breakdown table's "Cost (USD)" column
  // formats with `$${n.toFixed(4)}`. Both surface USD-shaped text.
  //
  // We collect every visible text node that looks USD-shaped and require at
  // least one. Empty state (zero data) is OK — `$0` / `$0.00` still match.
  // We deliberately don't require a positive value: empty-fixture envs are
  // a supported render path.
  const usdCells = page
    .locator('body')
    .getByText(USD_LIKE)
    .filter({ hasText: /\$|—/ });

  // The page may take a beat to settle after the summary fetch resolves and
  // the StatCard AnimatedNumber finishes its tween. Allow up to 15s.
  await expect(usdCells.first()).toBeVisible({ timeout: 15_000 });
  const count = await usdCells.count();
  expect(count).toBeGreaterThanOrEqual(1);

  // Sanity-check the first few candidates so a stray non-currency cell that
  // happens to match the regex (e.g. "—" alone) doesn't pass as a USD render.
  // We require at least ONE cell to contain a "$" — that's the cost render
  // signature. Pure "—" placeholders alone aren't enough on the analytics
  // page; the Total Cost KPI is always shown when the source filter
  // includes VK traffic, so a "$"-bearing cell must exist.
  const sample = Math.min(count, 8);
  let sawDollar = false;
  for (let i = 0; i < sample; i++) {
    const text = (await usdCells.nth(i).innerText()).trim();
    expect(text).toMatch(USD_LIKE);
    if (text.includes('$')) sawDollar = true;
  }
  expect(sawDollar).toBe(true);
});

test('provider usage tab cost columns render', async ({ page }) => {
  const listResp = await page.goto('/ai-gateway/providers');

  if (listResp && listResp.status() === 404) {
    test.skip(true, 'providers route not available');
    return;
  }

  // List page wraps the DataTable in a Card carrying data-testid="providers-table".
  // Source: packages/control-plane-ui/src/pages/ai-gateway/providers/list/ProviderList.tsx
  const providersTable = page.getByTestId('providers-table');
  await expect(providersTable).toBeVisible({ timeout: 15_000 });

  // If the fixture has no providers, the table renders an empty-state row and
  // we can't navigate to a detail page — skip-graceful per the brief.
  const rows = providersTable.locator('table tbody tr');
  const rowCount = await rows.count();
  if (rowCount === 0) {
    test.skip(true, 'no providers in fixture');
    return;
  }

  // DataTable wires onRowClick on the <tr>; clicking the first row navigates
  // to `/ai-gateway/providers/${row.id}`.
  await rows.first().click();
  await page.waitForURL(/\/ai-gateway\/providers\/[^/]+$/, { timeout: 10_000 });

  // ProviderDetailPage tab strip renders each tab as a <button>. The "Usage"
  // tab uses i18n key pages:providers.usage = "Usage".
  const usageTab = page.getByRole('button', { name: /^Usage$/ });
  await expect(usageTab).toBeVisible({ timeout: 10_000 });
  await usageTab.click();

  // ProviderUsageTab renders:
  //   • Summary stat cards (one of which is `formatUsd(totalEstimatedCostUsd)`).
  //   • Per-project, per-virtual-key, per-model tables each carrying a
  //     "Cost" column rendered through formatUsd.
  // Wait for the summary grid OR an empty-state card to mount past the
  // "Loading usage data..." placeholder.
  await expect(
    page.locator('text=/Total requests|Loading usage data/i').first(),
  ).toBeVisible({ timeout: 15_000 });

  // If usage analytics returned no data the tab renders only the empty-state
  // card with no cost surface — skip-graceful.
  const loading = await page.locator('text=/Loading usage data/i').count();
  if (loading > 0) {
    // Wait briefly for the fetch to settle (still loading => fixture has no
    // analytics endpoint behavior; surface verified by the empty render).
    await page.waitForTimeout(2_000);
  }

  // Same USD-or-em-dash matcher as the analytics page.
  const usdCells = page
    .locator('body')
    .getByText(USD_LIKE)
    .filter({ hasText: /\$|—/ });

  await expect(usdCells.first()).toBeVisible({ timeout: 15_000 });
  const count = await usdCells.count();
  expect(count).toBeGreaterThanOrEqual(1);

  // Confirm at least one candidate is the documented shape. The Usage-tab
  // "Estimated cost" summary card is always rendered (it shows "$0" when the
  // provider has zero traffic), so a "$"-bearing cell must exist. If the
  // entire summary block falls back to placeholders we accept that too —
  // the surface still rendered.
  const sample = Math.min(count, 8);
  let sawCurrencyOrPlaceholder = false;
  for (let i = 0; i < sample; i++) {
    const text = (await usdCells.nth(i).innerText()).trim();
    expect(text.length).toBeGreaterThan(0);
    expect(text).toMatch(USD_LIKE);
    if (text.includes('$') || text === '—') sawCurrencyOrPlaceholder = true;
  }
  expect(sawCurrencyOrPlaceholder).toBe(true);
});
