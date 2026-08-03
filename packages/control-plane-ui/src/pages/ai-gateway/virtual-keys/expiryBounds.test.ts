/**
 * The expiry picker is an `<input type="date">`: it carries no timezone and the
 * admin reads its calendar against their own clock. Every assertion here pins an
 * EXACT civil date for a frozen instant in a named zone. A tolerance band (e.g.
 * "min lands within a day or two of now") is satisfied by both a correct and a
 * UTC-skewed implementation, so it cannot detect the skew this module exists to
 * prevent.
 */
import { describe, it, expect, vi, afterEach } from 'vitest';
import { expiryBounds, deriveUpdateExpiry } from './expiryBounds';
import { setDisplayTZ, browserTZ } from '@/lib/format';

afterEach(() => {
  vi.useRealTimers();
  setDisplayTZ(null);
});

/** Freeze the wall clock at an absolute instant for the duration of a case. */
function freeze(utcInstant: string) {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(utcInstant));
}

describe('expiryBounds — the floor is tomorrow on the admin calendar', () => {
  // Each row pins a frozen absolute instant, the admin's zone, the civil date
  // that is "today" for them at that instant, and the floor the picker must
  // offer. The zone offsets are what make the rows distinct: one instant is a
  // different calendar day either side of UTC.
  const cases: Array<{
    name: string;
    instant: string;
    tz: string;
    localToday: string;
    expectedMin: string;
  }> = [
    {
      // Evening west of UTC: the instant has already rolled into the next UTC
      // day while the admin is still on the previous calendar day.
      name: 'Los Angeles 17:30 local — instant is already tomorrow in UTC',
      instant: '2026-07-16T00:30:00Z',
      tz: 'America/Los_Angeles',
      localToday: '2026-07-15',
      expectedMin: '2026-07-16',
    },
    {
      // Early morning east of UTC: the admin is on the next calendar day while
      // the instant is still on the previous UTC day.
      name: 'Shanghai 03:30 local — instant is still yesterday in UTC',
      instant: '2026-07-15T19:30:00Z',
      tz: 'Asia/Shanghai',
      localToday: '2026-07-16',
      expectedMin: '2026-07-17',
    },
    {
      // Mid-day east of UTC: local and UTC agree on the calendar day, so this
      // row alone cannot distinguish a correct floor from a skewed one. Kept to
      // pin that the common case does not regress.
      name: 'Shanghai 16:30 local — local and UTC agree on the day',
      instant: '2026-07-15T08:30:00Z',
      tz: 'Asia/Shanghai',
      localToday: '2026-07-15',
      expectedMin: '2026-07-16',
    },
    {
      name: 'UTC admin',
      instant: '2026-07-15T12:00:00Z',
      tz: 'UTC',
      localToday: '2026-07-15',
      expectedMin: '2026-07-16',
    },
    {
      // The floor carries into the next month and year rather than overflowing
      // to a 32nd day.
      name: 'Shanghai across the year boundary',
      instant: '2026-12-31T04:00:00Z',
      tz: 'Asia/Shanghai',
      localToday: '2026-12-31',
      expectedMin: '2027-01-01',
    },
    {
      // The floor crosses a spring-forward transition, where the local day is
      // 23 hours long. A calendar day must still advance the date by one.
      name: 'Los Angeles across spring-forward DST',
      instant: '2026-03-08T04:00:00Z',
      tz: 'America/Los_Angeles',
      localToday: '2026-03-07',
      expectedMin: '2026-03-08',
    },
  ];

  for (const c of cases) {
    it(`${c.name}: min is ${c.expectedMin} when local today is ${c.localToday}`, () => {
      freeze(c.instant);
      expect(expiryBounds(c.tz).min).toBe(c.expectedMin);
    });
  }

  it('the floor is strictly after the admin today, so today can never be picked', () => {
    // requireApplicationExpiry rejects an expiry that is not in the future, so
    // the picker must not offer a day the server would reject.
    freeze('2026-07-15T19:30:00Z');
    expect(expiryBounds('Asia/Shanghai').min > '2026-07-16').toBe(true);
  });

  it('defaults to the display TZ when no zone is passed', () => {
    freeze('2026-07-15T19:30:00Z');
    setDisplayTZ('Asia/Shanghai');
    expect(expiryBounds().min).toBe('2026-07-17');
  });

  it('follows a display-TZ override rather than the browser zone', () => {
    // A user whose profile pins a zone reads every rendered date in that zone,
    // so the picker floor must move with it.
    freeze('2026-07-15T19:30:00Z');
    setDisplayTZ('America/Los_Angeles');
    expect(expiryBounds().min).toBe('2026-07-16');
    setDisplayTZ('Asia/Shanghai');
    expect(expiryBounds().min).toBe('2026-07-17');
  });

  it('falls back to the browser zone when the display TZ is cleared', () => {
    freeze('2026-07-15T19:30:00Z');
    setDisplayTZ(null);
    expect(expiryBounds().min).toBe(expiryBounds(browserTZ()).min);
  });

  it('imposes no upper bound — expiry distance is unrestricted', () => {
    // A ceiling here would silently re-impose a cap on the picker even though
    // the server accepts any future date.
    expect('max' in expiryBounds()).toBe(false);
  });
});

describe('deriveUpdateExpiry — sends the expiry only when the admin changed it', () => {
  /** Form state in which the expiry field has not been edited. */
  function untouched(over: Partial<Parameters<typeof deriveUpdateExpiry>[0]> = {}) {
    return {
      vkType: 'application' as string | undefined,
      editExpiresAt: '2026-09-01',
      editNeverExpires: false,
      initialExpiresAt: '2026-09-01',
      initialNeverExpires: false,
      userTZ: 'UTC',
      ...over,
    };
  }

  it('omits the field when the expiry was never touched', () => {
    // The PUT reads an absent field as "leave the column unchanged". Re-deriving
    // a value on every save rewrites the stored instant whenever the admin edits
    // some unrelated field.
    expect(deriveUpdateExpiry(untouched())).toBeUndefined();
  });

  it('omits the field when an untouched never-expiring personal key is saved', () => {
    expect(
      deriveUpdateExpiry(
        untouched({
          vkType: 'personal',
          editExpiresAt: '',
          editNeverExpires: true,
          initialExpiresAt: '',
          initialNeverExpires: true,
        }),
      ),
    ).toBeUndefined();
  });

  it('sends the new date, stamped end-of-day, once the date changes', () => {
    expect(deriveUpdateExpiry(untouched({ editExpiresAt: '2026-09-02' }))).toBe(
      '2026-09-02T23:59:59.999Z',
    );
  });

  it('stamps end-of-day in the admin zone, not end-of-day UTC', () => {
    // "Expires Sep 2" means the key works through Sep 2 on the admin's
    // calendar. East of UTC that instant falls earlier in the UTC day.
    expect(
      deriveUpdateExpiry(untouched({ editExpiresAt: '2026-09-02', userTZ: 'Asia/Shanghai' })),
    ).toBe('2026-09-02T15:59:59.999Z');
    // West of UTC it falls on the following UTC day.
    expect(
      deriveUpdateExpiry(untouched({ editExpiresAt: '2026-09-02', userTZ: 'America/Los_Angeles' })),
    ).toBe('2026-09-03T06:59:59.999Z');
  });

  it('stamps the last representable moment of the chosen local day', () => {
    // One millisecond later is already the next calendar day for the admin.
    const stamped = deriveUpdateExpiry(
      untouched({ editExpiresAt: '2026-09-02', userTZ: 'Asia/Shanghai' }),
    ) as string;
    const oneMsLater = new Date(new Date(stamped).getTime() + 1);
    expect(
      new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai' }).format(oneMsLater),
    ).toBe('2026-09-03');
  });

  it('personal VK: switching on never-expires clears the column with null', () => {
    expect(
      deriveUpdateExpiry(
        untouched({ vkType: 'personal', editNeverExpires: true, initialNeverExpires: false }),
      ),
    ).toBeNull();
  });

  it('application VK: never emits null even when never-expires is somehow on', () => {
    // requireApplicationExpiry rejects a null expiry, so an application key must
    // never send one.
    expect(
      deriveUpdateExpiry(untouched({ editNeverExpires: true, initialNeverExpires: false })),
    ).toBe('2026-09-01T23:59:59.999Z');
  });

  it('application VK: blanking the date omits the field rather than sending null', () => {
    expect(deriveUpdateExpiry(untouched({ editExpiresAt: '' }))).toBeUndefined();
  });

  it('personal VK: blanking the date without never-expires leaves the expiry alone', () => {
    // Clearing the field while the toggle stays off is an ambiguous intent —
    // never-expires is the explicit way to clear an expiry. Omitting the field
    // keeps the stored value rather than guessing at null.
    expect(
      deriveUpdateExpiry(untouched({ vkType: 'personal', editExpiresAt: '' })),
    ).toBeUndefined();
  });

  it('unknown vkType: a changed date is stamped rather than sent bare', () => {
    // vkType is optional in the client type even though the column is NOT NULL,
    // so the helper must handle undefined rather than fall through to an
    // unstamped date. The PUT accepts a bare date but parses it to midnight,
    // retiring the key a day before the date the admin picked.
    expect(deriveUpdateExpiry(untouched({ vkType: undefined, editExpiresAt: '2026-09-02' }))).toBe(
      '2026-09-02T23:59:59.999Z',
    );
  });

  it('personal VK: turning never-expires back off sends the chosen date', () => {
    expect(
      deriveUpdateExpiry(
        untouched({
          vkType: 'personal',
          editNeverExpires: false,
          initialNeverExpires: true,
          initialExpiresAt: '',
        }),
      ),
    ).toBe('2026-09-01T23:59:59.999Z');
  });
});
