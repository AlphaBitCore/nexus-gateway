/**
 * The VK expiry window, and the value a save should send.
 *
 * An expiry is a CIVIL DATE on the admin's calendar, not a UTC instant. The
 * control is an `<input type="date">`, which carries no timezone: it yields a
 * bare calendar day and the admin reads its calendar against their own clock. So
 * the picker floor, the displayed date, and the stamped instant all resolve
 * through the display TZ — anchoring any one of them to UTC makes the page
 * disagree with itself for every admin whose zone is not UTC. The wire and the
 * column stay absolute UTC instants; the display TZ decides only WHICH instant a
 * chosen day means.
 *
 * Application VKs must carry a FUTURE expiry — create (CreateVirtualKey), renew
 * (RenewVirtualKey), and the PUT update path (UpdateVirtualKey) all share
 * `requireApplicationExpiry` in
 * `control-plane/internal/ai/virtualkeys/handler/handler.go`, which rejects a
 * missing or already-elapsed expiry. The distance is NOT bounded, so the
 * selectable window is open-ended: only `min` (earliest = tomorrow) is
 * constrained, mirroring the server's future-ness check. This module is the
 * single source of that window so the create form and the detail-page edit
 * form stay in lockstep with the server rule.
 */
import { dateInputFromToday, endOfDayUTC } from '@/lib/format';

/** The selectable expiry window: opens tomorrow on the admin's calendar. */
export function expiryBounds(userTZ?: string): { min: string } {
  return { min: dateInputFromToday({ days: 1 }, userTZ) };
}

/**
 * Resolve the `expiresAt` value a VK PUT update should send, given the edit form
 * state and the state it was seeded with.
 *
 * The field is sent ONLY when the admin actually changed it. The PUT reads an
 * absent `expiresAt` as "leave the column unchanged", and omitting an untouched
 * field is what keeps a stored instant intact: a stored expiry carries a real
 * time component while the picker only round-trips a calendar day, so re-deriving
 * a value on every save would restamp that instant to end-of-day and move the
 * expiry whenever an admin edited some unrelated field.
 *
 * A chosen date is stamped end-of-day on BOTH paths: the PUT accepts a bare date
 * (`extractNullableTimeFromBody` takes RFC3339 or `2006-01-02`) but parses it to
 * midnight UTC, retiring the key before the day the admin picked; create / renew
 * bind into a Go `time.Time` and take RFC3339 only, so a bare date fails to
 * decode outright. Application VKs must keep a non-null future expiry, so this
 * never emits `null` for them. Personal VKs may additionally clear their expiry
 * to never-expire (`null`).
 *
 * Returns: a stamped RFC3339 string (set), `null` (clear — personal only), or
 * `undefined` (omit the field → leave the column unchanged).
 */
export function deriveUpdateExpiry(args: {
  vkType: string | undefined;
  editExpiresAt: string;
  editNeverExpires: boolean;
  initialExpiresAt: string;
  initialNeverExpires: boolean;
  userTZ?: string;
}): string | null | undefined {
  const { vkType, editExpiresAt, editNeverExpires, initialExpiresAt, initialNeverExpires, userTZ } =
    args;

  const touched = editExpiresAt !== initialExpiresAt || editNeverExpires !== initialNeverExpires;
  if (!touched) return undefined;

  if (vkType === 'application') {
    return editExpiresAt ? endOfDayUTC(editExpiresAt, userTZ) : undefined;
  }
  if (editNeverExpires) return null;
  return editExpiresAt ? endOfDayUTC(editExpiresAt, userTZ) : undefined;
}
