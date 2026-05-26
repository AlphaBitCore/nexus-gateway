/**
 * Chart data helpers.
 */

/**
 * Collapse a long-tail categorical dataset into the top N rows by value
 * plus a single synthetic "Other" row aggregating the rest.
 *
 * Pie charts get unreadable past ~6-8 slices, so this is the canonical
 * way to cap visible slices while preserving total accuracy.
 *
 *   - items.length <= n + 1 → returned unchanged. Collapsing a single
 *     tail item into "Other" would obscure information without saving
 *     visual budget.
 *   - items.length >  n + 1 → top N (by value desc) followed by one
 *     synthetic row whose value equals the sum of the dropped tail.
 *     The original input array is not mutated.
 *
 * The synthetic row is produced by `makeOther(totalValue, droppedCount)`
 * so the caller controls its full shape — required because rows often
 * carry fields beyond label + value (e.g. a `groupLabel` or `percent`
 * computed downstream).
 */
export function topNWithOther<T>(
  items: T[],
  n: number,
  getValue: (item: T) => number,
  makeOther: (totalValue: number, droppedCount: number) => T,
): T[] {
  if (items.length <= n + 1) return items;
  const sorted = [...items].sort((a, b) => getValue(b) - getValue(a));
  const head = sorted.slice(0, n);
  const tail = sorted.slice(n);
  const tailTotal = tail.reduce((sum, it) => sum + getValue(it), 0);
  return [...head, makeOther(tailTotal, tail.length)];
}

/**
 * Default top-N cap for pie charts: 6 individually-named slices plus
 * one "Other" slice when the dataset exceeds 7 entries. Below that
 * threshold, all slices are shown verbatim (no "Other" added).
 */
export const PIE_SLICE_CAP = 6;
