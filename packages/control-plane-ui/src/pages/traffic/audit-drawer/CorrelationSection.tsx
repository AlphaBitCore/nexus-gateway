import { useState, useEffect, useCallback, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import type { TrafficEvent } from '../../../api/types';
import { Stack } from '@/components/ui';
import { correlationPivotPatch } from '../filters/correlationPivot';
import type { LiveTrafficFiltersState } from '../filters/liveTrafficFilters';
import css from './trafficAuditDrawer.module.css';

/**
 * One correlation-id row: monospace value + one-click copy. When `onPivot`
 * is supplied the value renders as a click-to-pivot action (jump from "this
 * event" to that id's whole slice of the live list); otherwise it is plain
 * copy-only text. Empty values render as an em dash with no actions.
 */
function CorrelationRow({
  label,
  value,
  onPivot,
  pivotHint,
  copyLabel,
  copiedLabel,
  testId,
}: {
  label: string;
  value: string | null | undefined;
  onPivot?: () => void;
  pivotHint?: string;
  copyLabel: string;
  copiedLabel: string;
  testId: string;
}) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timerRef.current), []);

  const handleCopy = useCallback(() => {
    // clipboard is undefined outside secure contexts; a permission denial
    // rejects the promise. Either way the copy silently no-ops rather than
    // throwing from a click handler.
    if (!value || !navigator.clipboard) return;
    navigator.clipboard
      .writeText(value)
      .then(() => {
        setCopied(true);
        clearTimeout(timerRef.current);
        timerRef.current = setTimeout(() => setCopied(false), 1500);
      })
      .catch(() => {});
  }, [value]);

  return (
    <div>
      <div className={css.detailLabel}>{label}</div>
      <div className={css.corrRow}>
        {!value ? (
          <span className={css.corrValue}>—</span>
        ) : onPivot ? (
          <button
            type="button"
            className={css.corrPivotBtn}
            onClick={onPivot}
            title={pivotHint ? `${pivotHint}: ${value}` : value}
            aria-label={pivotHint ? `${pivotHint}: ${value}` : value}
            data-testid={`corr-pivot-${testId}`}
          >
            {value}
          </button>
        ) : (
          <span className={css.corrValue} title={value}>
            {value}
          </span>
        )}
        {value ? (
          <button
            type="button"
            className={css.corrCopyBtn}
            onClick={handleCopy}
            aria-label={`${copyLabel}: ${label}`}
            data-testid={`corr-copy-${testId}`}
          >
            {copied ? copiedLabel : copyLabel}
          </button>
        ) : null}
      </div>
    </div>
  );
}

/**
 * Correlation — the three caller-facing grains (request ⊂ session ⊂
 * end-user) plus the client's own request id and the cross-service trace
 * id. The event id doubles as the gateway request id (the
 * x-nexus-request-id response header). End-user / session are gateway-only
 * stamps: shown with an em dash on gateway rows where the caller sent no
 * tag, hidden entirely for proxy/agent rows. Client request id and trace id
 * are copy-only (no indexed list filter) and hidden when absent.
 */
export function CorrelationSection({
  e,
  isGatewayTraffic,
  onPivot,
}: {
  e: TrafficEvent;
  isGatewayTraffic: boolean;
  onPivot?: (patch: Partial<LiveTrafficFiltersState>) => void;
}) {
  const { t } = useTranslation();
  const copyLabel = t('pages:traffic.detail.correlation.copy');
  const copiedLabel = t('pages:traffic.detail.correlation.copied');

  return (
    <div data-testid="audit-drawer-correlation">
      <h3 className={css.sectionTitle}>{t('pages:traffic.detail.correlation.title')}</h3>
      <Stack gap="sm">
        <CorrelationRow
          label={t('pages:traffic.detail.correlation.requestId')}
          value={e.id}
          onPivot={onPivot ? () => onPivot(correlationPivotPatch('requestId', e.id)) : undefined}
          pivotHint={t('pages:traffic.detail.correlation.pivotRequest')}
          copyLabel={copyLabel}
          copiedLabel={copiedLabel}
          testId="request-id"
        />
        {isGatewayTraffic && (
          <>
            <CorrelationRow
              label={t('pages:traffic.detail.correlation.endUserId')}
              value={e.endUserId}
              onPivot={
                onPivot && e.endUserId
                  ? () => onPivot(correlationPivotPatch('endUserId', e.endUserId ?? ''))
                  : undefined
              }
              pivotHint={t('pages:traffic.detail.correlation.pivotEndUser')}
              copyLabel={copyLabel}
              copiedLabel={copiedLabel}
              testId="end-user-id"
            />
            <CorrelationRow
              label={t('pages:traffic.detail.correlation.sessionId')}
              value={e.sessionId}
              onPivot={
                onPivot && e.sessionId
                  ? () => onPivot(correlationPivotPatch('sessionId', e.sessionId ?? ''))
                  : undefined
              }
              pivotHint={t('pages:traffic.detail.correlation.pivotSession')}
              copyLabel={copyLabel}
              copiedLabel={copiedLabel}
              testId="session-id"
            />
          </>
        )}
        {/* Client's own X-Request-Id — what a caller's support ticket
            usually quotes. Copy-only: the column carries no index, so
            there is deliberately no list filter for it. */}
        {e.externalRequestId ? (
          <CorrelationRow
            label={t('pages:traffic.detail.correlation.clientRequestId')}
            value={e.externalRequestId}
            copyLabel={copyLabel}
            copiedLabel={copiedLabel}
            testId="client-request-id"
          />
        ) : null}
        {e.traceId ? (
          <CorrelationRow
            label={t('pages:traffic.detail.correlation.traceId')}
            value={e.traceId}
            copyLabel={copyLabel}
            copiedLabel={copiedLabel}
            testId="trace-id"
          />
        ) : null}
      </Stack>
    </div>
  );
}
