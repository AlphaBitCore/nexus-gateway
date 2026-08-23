import type { CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import type { TrafficEvent } from '../../../api/types';
import { formatTokens } from '@/lib/format';
import { fmtCost } from './auditDrawerPrimitives';
import css from './trafficAuditDrawer.module.css';

// Costs breakdown: three-section taxonomy.
//   1) Upstream provider cost — what the LLM provider charged
//      (input/cache-read/cache-write/output components).
//   2) Nexus internal-ops — extra spend caused by our cache
//      machinery (L2 embedding); ai-guard cost is tagged
//      separately as a security hard-cost.
//   3) vs. no-gateway baseline — what the naive sticker cost
//      would have been, plus how much a gateway cache HIT saved.
// Hidden (returns null) when every line is null/zero so non-LLM rows stay clean.
export function CostBreakdown({
  e,
  internalOpsExcluded,
}: {
  e: TrafficEvent;
  internalOpsExcluded: boolean;
}) {
  const { t } = useTranslation();
  const reasoning = e.reasoningCostUsd ?? 0;
  const embedding = e.embeddingCostUsd ?? 0;
  const aiGuard = e.aiGuardCostUsd ?? 0;
  const breakdown = e.internalOpsBreakdown ?? [];
  const breakdownTotal = breakdown.reduce((sum, b) => sum + (b.costUsd ?? 0), 0);
  const gatewaySavings = e.gatewayCacheSavingsUsd ?? 0;

  // Model pricing snapshot (LEFT JOIN at fetch time).
  const inputPM = e.modelInputPricePerMillion ?? null;
  const outputPM = e.modelOutputPricePerMillion ?? null;
  const cacheReadPM = e.modelCachedInputReadPricePerMillion ?? null;
  const cacheWritePM = e.modelCachedInputWritePricePerMillion ?? null;
  const promptT = e.promptTokens ?? 0;
  const completionT = e.completionTokens ?? 0;
  const cacheReadT = e.cacheReadTokens ?? 0;
  const cacheCreationT = e.cacheCreationTokens ?? 0;
  const uncachedT = Math.max(0, promptT - cacheReadT - cacheCreationT);

  // Per-component upstream math. The four catalog prices
  // displayed here are the same numbers the gateway used to
  // compute the row's cost (read from the Model row), so
  // the explicit rows sum exactly to the subtotal — no residual
  // line needed. If a Model row has NULL cache prices,
  // LookupCachePricing falls back to InputPricePM (flat rate),
  // which still keeps the decomposition closed.
  const uncachedCost = (inputPM != null && uncachedT > 0) ? (uncachedT * inputPM / 1_000_000) : null;
  const cachedReadCost = (cacheReadPM != null && cacheReadT > 0) ? (cacheReadT * cacheReadPM / 1_000_000) : null;
  const cachedWriteCost = (cacheWritePM != null && cacheCreationT > 0) ? (cacheCreationT * cacheWritePM / 1_000_000) : null;
  const outputCost = (outputPM != null && completionT > 0) ? (completionT * outputPM / 1_000_000) : null;

  // `primary` (upstream subtotal) is derived from the
  // per-component math above, not a separate
  // estimated_cost_usd column. The four components close
  // exactly (see comment block above). Reasoning sits
  // inside completion at output rate, so we exclude it
  // from this sum to avoid double-count.
  const primary = (uncachedCost ?? 0) + (cachedReadCost ?? 0) + (cachedWriteCost ?? 0) + (outputCost ?? 0);
  // Modality rows (image / tts / rerank) carry no prompt/completion tokens, so
  // the per-component recompute above is 0. Rather than hide the whole panel,
  // surface the gateway's stamped per-unit total (traffic_event.
  // estimated_cost_usd) as a single upstream line — the authoritative cost the
  // gateway already computed for the endpoint's own unit (images / characters /
  // search units). Only used when the token recompute produced nothing, so
  // token rows never double-count.
  const stampedTotal = e.estimatedCostUsd ?? 0;
  const modalityCost = primary === 0 && stampedTotal > 0 ? stampedTotal : 0;
  if (
    primary === 0 && modalityCost === 0 && reasoning === 0 && embedding === 0 &&
    aiGuard === 0 && breakdownTotal === 0 && gatewaySavings === 0
  ) {
    return null;
  }

  // Naive baseline = "what this exact request would cost
  // without Nexus":
  //   all prompt tokens × full input price
  // + all completion tokens × full output price
  // (Reasoning tokens are a subset of completion — already
  // counted at output rate. ai-guard, L2 embedding, cache
  // savings are all Nexus-side and are explicitly NOT in
  // the baseline.)
  const naiveBaseline = (inputPM != null && outputPM != null)
    ? (promptT * inputPM + completionT * outputPM) / 1_000_000
    : null;

  // ai-guard is "internal-ops" too (we paid for the
  // classifier call), but it's a SECURITY hard cost so we
  // tag the row separately while still folding it into the
  // section subtotal. That keeps the math identity intact:
  //   Provider total + Internal-ops total = Net total.
  const internalOpsCost = embedding + breakdownTotal + aiGuard;
  const netSpend = primary + modalityCost + internalOpsCost;
  // vsBaseline = net spend with Nexus − naive without Nexus.
  // Positive = paid more (typical when caching adds write
  // surcharge or ai-guard adds security cost). Negative =
  // saved (typical on cache HIT — no upstream call at all).
  // Baseline deliberately excludes ai-guard / embedding —
  // those wouldn't exist in a no-Nexus world.
  const vsBaseline = naiveBaseline != null ? netSpend - naiveBaseline : null;
  // vsBaseline > 0 = paid more than naive (e.g. write surcharge + embedding without HIT)
  // vsBaseline < 0 = saved money (cache HIT, cache-read discount)

  const monoRight = { padding: 'var(--g-space-xs) 0', textAlign: 'right' as const, fontFamily: 'var(--g-font-mono)' };
  const monoRightStrong = { ...monoRight, fontWeight: 'var(--g-font-weight-semibold)' };
  const labelCell = (muted = false): CSSProperties => ({
    padding: 'var(--g-space-xs) 0',
    color: muted ? 'var(--g-color-text-muted)' : undefined,
    fontSize: 'var(--g-font-size-sm)',
  });
  const sectionHeader = {
    padding: 'var(--g-space-sm) 0 var(--g-space-xs)',
    fontSize: 'var(--g-font-size-xs)',
    textTransform: 'uppercase' as const,
    letterSpacing: '0.04em',
    color: 'var(--g-color-text-muted)',
  };

  return (
    <div className={css.fieldGroup}>
      <h3 className={css.sectionTitle}>{t('pages:traffic.detail.costs.title')}</h3>
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <tbody>
          {/* ─── 1. Upstream provider cost ─── */}
          {primary !== 0 && (
            <>
              <tr>
                <td colSpan={2} style={sectionHeader}>
                  {t('pages:traffic.detail.costs.section.upstream')}
                </td>
              </tr>
              {uncachedT > 0 && (
                <tr>
                  <td style={labelCell(true)}>
                    {formatTokens(uncachedT)} {inputPM != null ? <>× {fmtCost(inputPM)}/M</> : null}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.input')})</span>
                    {inputPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                  </td>
                  <td style={monoRight}>{uncachedCost != null ? fmtCost(uncachedCost) : '—'}</td>
                </tr>
              )}
              {cacheReadT > 0 && (
                <tr>
                  <td style={labelCell(true)}>
                    {formatTokens(cacheReadT)} {cacheReadPM != null ? <>× {fmtCost(cacheReadPM)}/M</> : null}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.cacheRead')})</span>
                    {cacheReadPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                  </td>
                  <td style={monoRight}>{cachedReadCost != null ? fmtCost(cachedReadCost) : '—'}</td>
                </tr>
              )}
              {cacheCreationT > 0 && (
                <tr>
                  <td style={labelCell(true)}>
                    {formatTokens(cacheCreationT)} {cacheWritePM != null ? <>× {fmtCost(cacheWritePM)}/M</> : null}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.cacheWrite')})</span>
                    {cacheWritePM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                  </td>
                  <td style={monoRight}>{cachedWriteCost != null ? fmtCost(cachedWriteCost) : '—'}</td>
                </tr>
              )}
              {completionT > 0 && (
                <tr>
                  <td style={labelCell(true)}>
                    {formatTokens(completionT)} {outputPM != null ? <>× {fmtCost(outputPM)}/M</> : null}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.output')})</span>
                    {outputPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                  </td>
                  <td style={monoRight}>{outputCost != null ? fmtCost(outputCost) : '—'}</td>
                </tr>
              )}
              {reasoning !== 0 && (
                <tr>
                  <td style={labelCell(true)}>{t('pages:traffic.detail.costs.reasoning')}</td>
                  <td style={monoRight}>{fmtCost(reasoning)}</td>
                </tr>
              )}
              <tr style={{ borderTop: '1px solid var(--g-color-border-subtle)' }}>
                <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>{t('pages:traffic.detail.costs.section.upstreamSubtotal')}</td>
                <td style={monoRightStrong}>{fmtCost(primary)}</td>
              </tr>
            </>
          )}

          {/* ─── 1b. Upstream provider cost — modality (per-unit) ───
              image / tts / rerank have no token decomposition; the
              gateway's stamped per-unit total stands in for the
              component rows so the panel is not hidden. */}
          {modalityCost > 0 && (
            <>
              <tr>
                <td colSpan={2} style={sectionHeader}>
                  {t('pages:traffic.detail.costs.section.upstream')}
                </td>
              </tr>
              <tr>
                <td style={labelCell(true)}>{t('pages:traffic.detail.costs.modality')}</td>
                <td style={monoRight}>{fmtCost(modalityCost)}</td>
              </tr>
              <tr style={{ borderTop: '1px solid var(--g-color-border-subtle)' }}>
                <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>{t('pages:traffic.detail.costs.section.upstreamSubtotal')}</td>
                <td style={monoRightStrong}>{fmtCost(modalityCost)}</td>
              </tr>
            </>
          )}

          {/* ─── 2. Nexus internal-ops ───
              Combines cache-machinery costs (L2 embedding,
              future hook-side LLMs) with security hard
              costs (ai-guard). All are gateway-side spend
              outside the primary upstream call. Each row
              carries an inline tag identifying its sub-
              category ([cache] / [security]) so admins can
              still distinguish optimization spend from
              security spend without breaking the math
              identity: Provider total + Internal-ops total
              = Net total. */}
          {(embedding !== 0 || aiGuard !== 0 || breakdownTotal !== 0) && (
            <>
              <tr>
                <td colSpan={2} style={sectionHeader}>
                  {t('pages:traffic.detail.costs.section.internalOps')}
                </td>
              </tr>
              {embedding !== 0 && (
                <tr>
                  <td style={labelCell()}>
                    + {t('pages:traffic.detail.costs.embedding')}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>{t('pages:traffic.detail.costs.cacheTag')}</span>
                  </td>
                  <td style={monoRight}>+ {fmtCost(embedding)}</td>
                </tr>
              )}
              {aiGuard !== 0 && (
                <tr>
                  <td style={labelCell()}>
                    + {t('pages:traffic.detail.costs.aiGuard')}&nbsp;
                    <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>{t('pages:traffic.detail.costs.aiGuardTag')}</span>
                  </td>
                  <td style={monoRight}>+ {fmtCost(aiGuard)}</td>
                </tr>
              )}
              {breakdown.map((b, i) => (
                <tr key={`brk-${i}`}>
                  <td style={labelCell()}>+ {b.type}{b.model ? ` (${b.model})` : ''}</td>
                  <td style={monoRight}>+ {fmtCost(b.costUsd ?? 0)}</td>
                </tr>
              ))}
              <tr style={{ borderTop: '1px solid var(--g-color-border-subtle)' }}>
                <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>{t('pages:traffic.detail.costs.section.internalOpsSubtotal')}</td>
                <td style={monoRightStrong}>+ {fmtCost(internalOpsCost)}</td>
              </tr>
            </>
          )}

          {/* ─── Net total ─── */}
          <tr style={{ borderTop: '2px solid var(--g-color-border)' }}>
            <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-bold)', paddingTop: 'var(--g-space-sm)' }}>
              {t('pages:traffic.detail.costs.netTotal')}
            </td>
            <td style={{ ...monoRightStrong, fontWeight: 'var(--g-font-weight-bold)', paddingTop: 'var(--g-space-sm)' }}>
              {fmtCost(netSpend)}
            </td>
          </tr>

          {/* ─── 4. vs. no-gateway baseline (comparison line) ─── */}
          {naiveBaseline != null && (
            <>
              <tr>
                <td colSpan={2} style={sectionHeader}>
                  {t('pages:traffic.detail.costs.section.vsBaseline')}
                </td>
              </tr>
              <tr>
                <td style={labelCell(true)}>{t('pages:traffic.detail.costs.naiveBaseline')}</td>
                <td style={monoRight}>{fmtCost(naiveBaseline)}</td>
              </tr>
              {gatewaySavings !== 0 && (
                <tr>
                  <td style={labelCell(true)}>{t('pages:traffic.detail.costs.cacheSavings')}</td>
                  <td style={monoRight}>− {fmtCost(gatewaySavings)}</td>
                </tr>
              )}
              <tr>
                <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>
                  {vsBaseline! < 0 ? t('pages:traffic.detail.costs.savedVsBaseline') : t('pages:traffic.detail.costs.paidMoreVsBaseline')}
                </td>
                <td style={{ ...monoRightStrong, color: vsBaseline! < 0 ? 'var(--g-color-success)' : undefined }}>
                  {vsBaseline! < 0 ? '−' : '+'} {fmtCost(Math.abs(vsBaseline!))}
                </td>
              </tr>
            </>
          )}
        </tbody>
      </table>
      {/* Quota policy note — make it clear whether ai-guard
          / embedding count against this VK's quota. */}
      {(embedding !== 0 || aiGuard !== 0 || breakdownTotal !== 0) && (
        <div style={{ marginTop: 'var(--g-space-sm)', fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>
          {t(internalOpsExcluded
            ? 'pages:traffic.detail.costs.internalOpsNoteExcluded'
            : 'pages:traffic.detail.costs.internalOpsNoteCounted')}
        </div>
      )}
    </div>
  );
}
