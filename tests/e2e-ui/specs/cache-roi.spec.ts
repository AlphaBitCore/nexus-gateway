import { test, expect } from '@playwright/test';

// Spec: Cache ROI dashboard (E61) — verifies the analytics surface loads and
// renders numeric stat cells. The route is `/cache-roi` (see
// packages/control-plane-ui/src/routes/shellRouteConfig.tsx). Hero strip lives
// in CacheROIDashboard.tsx; values come from analyticsApi.cacheROI().
//
// Numeric cells use the SummaryCard component (currency / percentage / count).
// We assert the page heading and that at least 2 summary value cells contain
// numeric-shaped text. Empty/zero values are acceptable.

test('cache ROI dashboard loads', async ({ page }) => {
  const resp = await page.goto('/cache-roi');

  // Skip if the route is not registered (feature flagged off, or older
  // build without E61).
  if (resp && resp.status() === 404) {
    test.skip(true, 'cache ROI not available');
    return;
  }

  // Page heading from i18n key pages:analytics.cacheRoi.title = "Cache ROI".
  // The PageHeader component renders an <h1>.
  const heading = page.locator('h1', { hasText: /Cache ROI/i });
  await expect(heading).toBeVisible({ timeout: 15_000 });
});

test('cache ROI cells render', async ({ page }) => {
  const resp = await page.goto('/cache-roi');
  if (resp && resp.status() === 404) {
    test.skip(true, 'cache ROI not available');
    return;
  }

  // Wait for the heading so we know the dashboard has mounted past skeleton.
  await expect(page.locator('h1', { hasText: /Cache ROI/i })).toBeVisible({
    timeout: 15_000,
  });

  // SummaryCard value nodes live inside the hero/grid sections. Each card
  // value renders an AnimatedNumber formatted as currency ($x.xxxx),
  // percentage (xx%), multiplier (xx.x×), or raw count (xxx).
  // CSS modules hash class names, so target by the [class*="summaryValue"]
  // substring match — stable across css-modules hash regeneration.
  const valueCells = page.locator('[class*="summaryValue"]');

  // Hero strip alone has 3-4 cards; gateway + provider sections add more.
  // Require at least 2 to guard against an empty render.
  await expect(valueCells.first()).toBeVisible({ timeout: 15_000 });
  const count = await valueCells.count();
  expect(count).toBeGreaterThanOrEqual(2);

  // Inspect the first few cells and confirm each holds numeric-shaped text.
  // Allowed forms:
  //   currency      — "$0.00", "$1.2345", "$0.000123"
  //   percentage    — "0%", "73%"
  //   multiplier    — "1.2×", "0.0×"
  //   token / count — "123", "1,234", "12.3K", "1.2M B"
  //   placeholder   — "—" (em-dash; null/zero state, acceptable)
  //   negative      — "-$0.0050" (net cost exceeds savings)
  // Pattern accepts digits / decimal / comma / sign / $ / % / × / K / M / B
  // / space / em-dash.
  const numericLike = /^[\s$\d.,%×\-—KMB]+$/;
  const sample = Math.min(count, 4);
  for (let i = 0; i < sample; i++) {
    const text = (await valueCells.nth(i).innerText()).trim();
    expect(text.length).toBeGreaterThan(0);
    expect(text).toMatch(numericLike);
  }
});
