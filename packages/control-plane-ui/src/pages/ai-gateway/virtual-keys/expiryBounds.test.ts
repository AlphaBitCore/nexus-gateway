import { describe, it, expect } from 'vitest';
import { expiryBounds, stampExpiryEndOfDay, deriveUpdateExpiry } from './expiryBounds';

describe('expiryBounds', () => {
  it('returns a window from tomorrow to just under 3 months from now', () => {
    const { min, max } = expiryBounds();
    const now = Date.now();
    const minMs = new Date(`${min}T00:00:00Z`).getTime();
    const maxMs = new Date(`${max}T00:00:00Z`).getTime();
    const day = 24 * 60 * 60 * 1000;

    // min is at least ~today (tomorrow's calendar date, accounting for TZ).
    expect(minMs).toBeGreaterThanOrEqual(now - day);
    // max stays strictly under now + 3 months (the server's hard ceiling),
    // with the 2-day margin keeping the end-of-day stamp safely inside it.
    const threeMonths = new Date();
    threeMonths.setMonth(threeMonths.getMonth() + 3);
    expect(maxMs).toBeLessThan(threeMonths.getTime());
    expect(maxMs).toBeGreaterThan(minMs);
  });
});

describe('stampExpiryEndOfDay', () => {
  it('stamps a YYYY-MM-DD date as end-of-day UTC RFC3339', () => {
    expect(stampExpiryEndOfDay('2026-09-01')).toBe('2026-09-01T23:59:59Z');
  });
});

describe('deriveUpdateExpiry', () => {
  it('application VK: stamps the chosen date and NEVER emits null', () => {
    expect(
      deriveUpdateExpiry({ vkType: 'application', editExpiresAt: '2026-09-01', editNeverExpires: true }),
    ).toBe('2026-09-01T23:59:59Z');
  });

  it('application VK: blank date omits the field (undefined), not null', () => {
    expect(
      deriveUpdateExpiry({ vkType: 'application', editExpiresAt: '', editNeverExpires: false }),
    ).toBeUndefined();
  });

  it('personal VK: never-expires clears the column with null', () => {
    expect(
      deriveUpdateExpiry({ vkType: 'personal', editExpiresAt: '2026-09-01', editNeverExpires: true }),
    ).toBeNull();
  });

  it('personal VK: a date is sent as-is (server accepts YYYY-MM-DD)', () => {
    expect(
      deriveUpdateExpiry({ vkType: 'personal', editExpiresAt: '2026-09-01', editNeverExpires: false }),
    ).toBe('2026-09-01');
  });

  it('personal VK: blank with no never-expires omits the field', () => {
    expect(
      deriveUpdateExpiry({ vkType: 'personal', editExpiresAt: '', editNeverExpires: false }),
    ).toBeUndefined();
  });
});
