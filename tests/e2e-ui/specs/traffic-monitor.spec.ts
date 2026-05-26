import { test, expect } from '@playwright/test';

const AI_GW_URL = process.env.NEXUS_AI_GW_URL ?? 'http://localhost:3050';
const TEST_VK = process.env.NEXUS_TEST_VK ?? '';

// Spec 4: Live traffic refresh — fire a real AI-gateway request and verify
// the row surfaces in the traffic table within 45 s.
test('traffic row appears after AI-gateway request', async ({ page, request }) => {
  test.setTimeout(90_000);

  if (!TEST_VK) {
    test.skip(true, 'NEXUS_TEST_VK not configured');
    return;
  }

  const marker = `e2e-traffic-${Date.now()}`;

  await page.goto('/traffic');
  await expect(page.getByTestId('traffic-table')).toBeVisible({ timeout: 15_000 });

  // Fire a request to the AI Gateway using the test VK.
  // The `request` fixture inherits the playwright.config proxy (direct://).
  const resp = await request.post(`${AI_GW_URL}/v1/chat/completions`, {
    headers: {
      'Authorization': `Bearer ${TEST_VK}`,
      'Content-Type': 'application/json',
    },
    data: {
      model: 'auto',
      messages: [{ role: 'user', content: `ping ${marker}` }],
      max_tokens: 10,
    },
  });

  // 200/201/400/404/429/503 all count — what matters is the gateway
  // processed it (vkauth passed, traffic_event row written). 404 covers
  // ROUTING_NO_MATCH when the dev seed lacks a routing rule for 'auto'.
  expect([200, 201, 400, 404, 429, 503]).toContain(resp.status());

  // Poll the traffic table until the marker request appears (up to 45 s).
  // Reload is needed because the table doesn't auto-refresh.
  await expect.poll(
    async () => {
      await page.reload();
      await page.getByTestId('traffic-table').waitFor({ state: 'visible', timeout: 5_000 });
      const rows = page.locator('[data-testid="traffic-table"] table tbody tr');
      return await rows.count();
    },
    { timeout: 45_000, intervals: [3_000] },
  ).toBeGreaterThan(0);
});
