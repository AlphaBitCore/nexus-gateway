import { describe, it, expect } from 'vitest';
import { topNWithOther, PIE_SLICE_CAP } from './chartData';

interface Row {
  label: string;
  cost: number;
}

const makeOther = (total: number, count: number): Row => ({
  label: `Other (${count})`,
  cost: total,
});

describe('topNWithOther', () => {
  it('returns input unchanged when length <= n + 1', () => {
    const items: Row[] = [
      { label: 'a', cost: 10 },
      { label: 'b', cost: 5 },
      { label: 'c', cost: 3 },
    ];
    const out = topNWithOther(items, 2, (r) => r.cost, makeOther);
    // 3 items, n+1 = 3, so no collapse — leave them alone (otherwise we'd
    // hide a single "tail" item under "Other (1)" which is pure overhead).
    expect(out).toEqual(items);
  });

  it('collapses tail into "Other" when length > n + 1', () => {
    const items: Row[] = [
      { label: 'a', cost: 100 },
      { label: 'b', cost: 80 },
      { label: 'c', cost: 60 },
      { label: 'd', cost: 40 },
      { label: 'e', cost: 20 },
      { label: 'f', cost: 10 },
    ];
    const out = topNWithOther(items, 3, (r) => r.cost, makeOther);
    expect(out).toHaveLength(4);
    expect(out.slice(0, 3).map((r) => r.label)).toEqual(['a', 'b', 'c']);
    // Tail: d + e + f = 40 + 20 + 10 = 70 across 3 rows
    expect(out[3]).toEqual({ label: 'Other (3)', cost: 70 });
  });

  it('sorts by value desc before slicing', () => {
    // Pass items intentionally out of order to confirm sort happens
    const items: Row[] = [
      { label: 'small', cost: 1 },
      { label: 'big', cost: 100 },
      { label: 'medium', cost: 50 },
      { label: 'tiny', cost: 0.5 },
      { label: 'micro', cost: 0.1 },
    ];
    const out = topNWithOther(items, 2, (r) => r.cost, makeOther);
    expect(out.slice(0, 2).map((r) => r.label)).toEqual(['big', 'medium']);
    expect(out[2].label).toBe('Other (3)');
    expect(out[2].cost).toBeCloseTo(1.6, 10);
  });

  it('does not mutate the input array', () => {
    const items: Row[] = [
      { label: 'a', cost: 5 },
      { label: 'b', cost: 10 },
      { label: 'c', cost: 1 },
      { label: 'd', cost: 2 },
      { label: 'e', cost: 3 },
    ];
    const snapshot = items.map((r) => ({ ...r }));
    topNWithOther(items, 2, (r) => r.cost, makeOther);
    expect(items).toEqual(snapshot);
  });

  it('handles empty input', () => {
    const out = topNWithOther<Row>([], 6, (r) => r.cost, makeOther);
    expect(out).toEqual([]);
  });

  it('PIE_SLICE_CAP is 6 (top 6 + Other = 7 visible slices max)', () => {
    expect(PIE_SLICE_CAP).toBe(6);
  });
});
