import type { ReactNode } from 'react';
import type { TFunction } from 'i18next';
import type { NormalizedPayload } from '../../../api/types';

// renderTierBadge surfaces the normalizer-reported DetectedSpec + Confidence
// so operators can tell whether a row was parsed by a precise Tier-1
// protocol decoder ("anthropic-messages" confidence 0.95), the Tier-2
// multi-spec pattern probe ("pattern:chatgpt-web" confidence 0.78), or the
// structural fallback projection ("generic-http" — a typed view of the raw
// HTTP body with no protocol decode; its confidence speaks only for the
// projection, never for AI semantics). Legacy verbatim rows carry no
// detectedSpec and render no badge.
export function renderTierBadge(
  payload: NormalizedPayload,
  t: TFunction,
): ReactNode {
  if (!payload.detectedSpec) return null;
  const isStructural = payload.detectedSpec === 'generic-http';
  const isTier2 = !isStructural && payload.detectedSpec.startsWith('pattern:');
  const specLabel = isTier2
    ? payload.detectedSpec.slice('pattern:'.length)
    : payload.detectedSpec;
  const tierKey = isStructural ? 'structural' : isTier2 ? 'tier2' : 'tier1';
  const tierLabel = t(`pages:traffic.detail.normalized.tier.${tierKey}`);
  // Confidence is absent on older rows — omit when not present. Two
  // decimals. Two semantics suppress the numeral:
  //   - Structural projections: their 1.0 means "faithful projection",
  //     and printing it next to a real decode would read as more trusted.
  //   - Host-matched rows (selectionEvidence='host'): the adapter was
  //     chosen by host, not coverage; its confidence is the honest
  //     coverage of a known-adapter body (~0.6 for single-prompt specs),
  //     a different scale than a sniffed decode — show a "host-matched"
  //     label instead so the two never read as one comparable number.
  const isHostMatched = payload.selectionEvidence === 'host';
  const confLabel =
    !isStructural && !isHostMatched && typeof payload.confidence === 'number' && payload.confidence > 0
      ? ` · ${payload.confidence.toFixed(2)}`
      : '';
  const evidenceLabel = isHostMatched
    ? ` · ${t('pages:traffic.detail.normalized.tier.hostMatched')}`
    : '';
  const styles: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 'var(--g-space-1)',
    padding: 'var(--g-space-0-5) var(--g-space-1-5)',
    marginBottom: 'var(--g-space-2)',
    borderRadius: 'var(--radius-sm)',
    fontSize: 'var(--g-font-size-xs)',
    fontFamily: 'var(--g-font-mono)',
    // Structural projection is deliberately neutral — the green Tier-1
    // styling would overclaim an AI decode that never happened.
    background: isStructural
      ? 'color-mix(in srgb, var(--color-text-muted) 10%, transparent)'
      : isTier2 ? 'var(--color-warning-soft)' : 'var(--color-success-soft)',
    color: isStructural
      ? 'var(--color-text-muted)'
      : isTier2 ? 'var(--color-warning-text)' : 'var(--color-text-primary)',
    border: '1px solid',
    borderColor: isStructural
      ? 'var(--color-border)'
      : isTier2 ? 'var(--color-warning-border)' : 'var(--color-border-subtle)',
  };
  return (
    <div style={styles} title={t('pages:traffic.detail.normalized.tier.hint')}>
      <span>{tierLabel}</span>
      <span>·</span>
      <span>{specLabel}{confLabel}{evidenceLabel}</span>
    </div>
  );
}
