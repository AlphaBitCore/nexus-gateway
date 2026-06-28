/**
 * Row-action glyphs for the alert inbox table — acknowledge (check) and
 * resolve (check-in-circle). Extracted as standalone presentational SVGs so
 * the page module stays focused on layout + data wiring.
 */
export function AckActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M20 6 9 17l-5-5" />
    </svg>
  );
}

export function ResolveActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="12" cy="12" r="9" />
      <path d="M8 12.5 10.5 15 16 9" />
    </svg>
  );
}
