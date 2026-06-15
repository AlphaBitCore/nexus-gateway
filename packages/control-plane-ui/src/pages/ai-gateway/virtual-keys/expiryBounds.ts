/**
 * Application VKs are server-capped to 3 months from now: create
 * (`maxExpiry := time.Now().AddDate(0, 3, 0)` in
 * `control-plane/internal/ai/virtualkeys/handler/vk.go` CreateVirtualKey),
 * renew (RenewVirtualKey in approval.go), AND the general PUT update path
 * (UpdateVirtualKey) all reject an expiry beyond that ceiling. This module is
 * the single source of the client-side selectable window so the create form
 * and the detail-page edit form stay in lockstep with the server cap.
 *
 * Computed window: earliest = tomorrow, latest = +3 months minus a 2-day
 * margin so the end-of-day `T23:59:59Z` stamp applied at submit stays
 * comfortably under the server's `now + 3 months` check.
 */
export function expiryBounds(): { min: string; max: string } {
  const ymd = (d: Date) => d.toISOString().slice(0, 10);
  const min = new Date();
  min.setDate(min.getDate() + 1);
  const max = new Date();
  max.setMonth(max.getMonth() + 3);
  max.setDate(max.getDate() - 2);
  return { min: ymd(min), max: ymd(max) };
}

/**
 * Stamp a `YYYY-MM-DD` date (from `<input type="date">`) as end-of-day UTC
 * RFC3339, matching the create form. The backend unmarshals expiresAt into
 * time.Time and accepts RFC3339 or `YYYY-MM-DD`; stamping end-of-day keeps
 * "expires on May 2" usable through that whole calendar day.
 */
export function stampExpiryEndOfDay(ymd: string): string {
  return `${ymd}T23:59:59Z`;
}

/**
 * Resolve the `expiresAt` value a VK PUT update should send, given the edit
 * form state. Application VKs MUST carry a non-null expiry (the server rejects
 * null / never-expire and caps it at 3 months), so this never emits `null` for
 * them — it stamps the chosen date end-of-day, or `undefined` (leave unchanged)
 * if somehow blank. Personal VKs may clear their expiry to never-expire
 * (`null`) or send a date. Mirrors the server rule in UpdateVirtualKey.
 *
 * Returns: a stamped RFC3339 string (set), `null` (clear — personal only), or
 * `undefined` (omit the field → leave the column unchanged).
 */
export function deriveUpdateExpiry(args: {
  vkType: string | undefined;
  editExpiresAt: string;
  editNeverExpires: boolean;
}): string | null | undefined {
  const { vkType, editExpiresAt, editNeverExpires } = args;
  if (vkType === 'application') {
    return editExpiresAt ? stampExpiryEndOfDay(editExpiresAt) : undefined;
  }
  if (editNeverExpires) return null;
  return editExpiresAt || undefined;
}
