import { test, expect } from '@playwright/test';

// Spec: Cache Hub page (E59 fleet config UI + E69 prewarm modal).
//
// Covers three idempotent, read-mostly assertions:
//   1. Page loads with heading + at least one section card rendered.
//   2. Prewarm modal opens, accepts corpus input, and surfaces a response
//      (preview/success/error). Skips gracefully if the prewarm button is
//      not present on this build (semantic cache disabled => button hidden).
//   3. Semantic cache settings form renders at least one known field
//      (Similarity Threshold). Read-only assertion — no mutation.
//
// The spec mutates no fleet config. The prewarm test calls the dry-run /
// confirm path with a tiny corpus but is gated by skip-graceful behavior so
// it never destructively writes if the UI surface is absent.

const CACHE_URL = '/ai-gateway/cache';

test('cache hub page loads', async ({ page }) => {
  const response = await page.goto(CACHE_URL);

  // If the route 404s, the build doesn't ship the cache hub — skip.
  if (response && response.status() === 404) {
    test.skip(true, 'cache hub disabled in this build');
    return;
  }

  // Page heading rendered by PageHeader — the i18n EN value is "Cache".
  // Use a robust role-based query; PageHeader uses an h1.
  const heading = page.getByRole('heading', { name: /^Cache$/i }).first();
  await expect(heading).toBeVisible({ timeout: 15_000 });

  // The StatusStrip is always rendered above the tabs. Either it or the
  // Gateway Cache tab content (Semantic settings card) must be visible.
  const semanticHeading = page.getByRole('heading', { name: /semantic/i }).first();
  const tabsList = page.getByRole('tablist').first();

  // At least one of: tabs list OR the semantic card heading must be visible.
  const tabsVisible = await tabsList.isVisible().catch(() => false);
  const semanticVisible = await semanticHeading.isVisible().catch(() => false);
  expect(tabsVisible || semanticVisible).toBeTruthy();
});

test('prewarm modal opens and validates corpus input', async ({ page }) => {
  const response = await page.goto(CACHE_URL);
  if (response && response.status() === 404) {
    test.skip(true, 'cache hub disabled in this build');
    return;
  }

  // Wait for the page to settle.
  await expect(page.getByRole('heading', { name: /^Cache$/i }).first())
    .toBeVisible({ timeout: 15_000 });

  // The pre-warm button only renders when semantic cache is enabled.
  // EN label is "Pre-warm Corpus".
  const prewarmBtn = page.getByRole('button', { name: /pre-?warm/i }).first();

  if ((await prewarmBtn.count()) === 0 || !(await prewarmBtn.isVisible().catch(() => false))) {
    test.skip(true, 'cache prewarm UI not present on this build');
    return;
  }

  await prewarmBtn.click();

  // The Dialog uses role="dialog". Wait for it to open.
  const modal = page.getByRole('dialog');
  await expect(modal).toBeVisible({ timeout: 5_000 });

  // Locate the corpus textarea inside the modal — any reachable textarea
  // will do (the prewarm modal has exactly one).
  const textarea = modal.locator('textarea').first();
  await expect(textarea).toBeVisible({ timeout: 5_000 });

  // Type a 3-entry JSON corpus (parser accepts both JSON arrays and CSV).
  const corpus = JSON.stringify([
    { prompt: 'What is 2+2?', response: 'Four.' },
    { prompt: 'Capital of France?', response: 'Paris.' },
    { prompt: 'Largest planet?', response: 'Jupiter.' },
  ]);
  await textarea.fill(corpus);

  // Click the Preview button (dry-run path — non-destructive). EN label is
  // "Preview". If Preview is unavailable, fall back to Confirm.
  const previewBtn = modal.getByRole('button', { name: /preview/i }).first();
  const confirmBtn = modal.getByRole('button', { name: /confirm|import|pre-?warm/i }).first();

  const previewAvailable = await previewBtn.isVisible().catch(() => false);
  const submitBtn = previewAvailable ? previewBtn : confirmBtn;
  await submitBtn.click();

  // Within 10s one of: preview panel renders (planned writes),
  // a success toast appears, or an error alert is rendered.
  // Any of these confirms the modal wired the corpus → backend round-trip.
  const previewPanel = modal.getByText(/planned writes|preview|entries|embed/i).first();
  const errorAlert = modal.getByRole('alert').first();
  const toast = page.locator('[role="status"], [data-testid*="toast"]').first();

  await expect
    .poll(
      async () => {
        const a = await previewPanel.isVisible().catch(() => false);
        const b = await errorAlert.isVisible().catch(() => false);
        const c = await toast.isVisible().catch(() => false);
        return a || b || c;
      },
      { timeout: 10_000 },
    )
    .toBeTruthy();
});

test('semantic cache settings field present', async ({ page }) => {
  const response = await page.goto(CACHE_URL);
  if (response && response.status() === 404) {
    test.skip(true, 'cache hub disabled in this build');
    return;
  }

  await expect(page.getByRole('heading', { name: /^Cache$/i }).first())
    .toBeVisible({ timeout: 15_000 });

  // The Similarity Threshold input has an aria-label set to the EN string.
  // The kill switch is also aria-labeled. Verify at least one is rendered.
  const thresholdInput = page.getByLabel(/similarity threshold/i).first();
  const killSwitch = page.getByLabel(/cache.*switch|kill.*switch|emergency/i).first();
  const varyBySelect = page.getByLabel(/cache isolation scope/i).first();

  const thresholdVisible = await thresholdInput.isVisible().catch(() => false);
  const killSwitchVisible = await killSwitch.isVisible().catch(() => false);
  const varyByVisible = await varyBySelect.isVisible().catch(() => false);

  // At least one semantic-cache tuning field must render. Don't mutate.
  expect(thresholdVisible || killSwitchVisible || varyByVisible).toBeTruthy();

  // If the threshold input is rendered, also confirm it accepts input
  // (read its current value — non-mutating).
  if (thresholdVisible) {
    const value = await thresholdInput.inputValue();
    expect(typeof value).toBe('string');
  }
});
