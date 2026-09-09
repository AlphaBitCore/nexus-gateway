import { useState, useEffect, useCallback, useRef } from "react";
import { useTranslation } from "react-i18next";
import type { TrafficEvent } from "../../../api/types";
import { Stack } from "@/components/ui";
import { correlationPivotPatch } from "../filters/correlationPivot";
import type { LiveTrafficFiltersState } from "../filters/liveTrafficFilters";
import css from "./trafficAuditDrawer.module.css";

/**
 * The caller's own tag bag, read out of the event's details JSON.
 *
 * The gateway persists X-Nexus-Client-Tags into details.clientTags, so the
 * value was reachable only by opening the raw details blob — the same place an
 * operator goes when they have already given up on the drawer. Rendered as one
 * row beside the end-user and session tags, which is where the other two
 * caller-declared facts already are.
 *
 * Returns null rather than an empty string when there is nothing to show, so
 * the row is omitted entirely instead of adding an em dash to every row a
 * caller never tagged.
 */
export function readClientTags(details: unknown): string | null {
  if (details === null || typeof details !== "object") return null;
  const raw = (details as Record<string, unknown>).clientTags;
  if (raw === null || typeof raw !== "object" || Array.isArray(raw))
    return null;
  const pairs = Object.entries(raw as Record<string, unknown>)
    .filter(([, v]) => typeof v === "string" && v !== "")
    .map(([k, v]) => `${k}=${String(v)}`);
  return pairs.length > 0 ? pairs.join(", ") : null;
}

/**
 * One correlation-id row: monospace value + one-click copy. When `onPivot`
 * is supplied the value renders as a click-to-pivot action (jump from "this
 * event" to that id's whole slice of the live list); otherwise it is plain
 * copy-only text. Empty values render as an em dash with no actions.
 */
function CorrelationRow({
  label,
  labelHint,
  value,
  onPivot,
  pivotHint,
  copyLabel,
  copiedLabel,
  testId,
}: {
  label: string;
  /** What this id IS, on the label's tooltip. Three ids sit in this section
   *  and their names alone do not separate them: the row's own id, the
   *  caller's, and the one that groups a request's rows. A tooltip answers that
   *  without spending a line of drawer on every row. */
  labelHint?: string;
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
      <div className={css.detailLabel} title={labelHint}>
        {label}
      </div>
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
 * Correlation — three ids with three owners, plus the caller-facing session
 * and end-user grains. Event id is this row's own key, minted by the emitting
 * service and shown copy-only: it identifies one row and matches nothing else.
 * Request id is what we returned on X-Nexus-Request-Id — resolved from that
 * header, or from its x-request-id alias, or minted when the caller sent
 * neither — and it is the one that groups rows, so it carries the pivot: a
 * realtime session's exchanges share it, and so do the rows several services
 * write for one request WHEN THE CALLER SENT AN ID. A caller who sends none
 * gets a separately minted id per service, and those rows correlate by client
 * IP and time instead. Trace id is the caller's own W3C trace from a
 * traceparent header; it is copy-only because the slice it names lives in
 * their tracing system, not here. End-user / session are gateway-only stamps:
 * shown with an em dash on gateway rows where the caller sent no tag, hidden
 * entirely for proxy/agent rows. Request id and trace id are hidden when
 * absent.
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
  const copyLabel = t("pages:traffic.detail.correlation.copy");
  const copiedLabel = t("pages:traffic.detail.correlation.copied");
  const clientTags = readClientTags(e.details);

  return (
    <div data-testid="audit-drawer-correlation">
      <h3 className={css.sectionTitle}>
        {t("pages:traffic.detail.correlation.title")}
      </h3>
      <Stack gap="sm">
        <CorrelationRow
          label={t("pages:traffic.detail.correlation.eventId")}
          labelHint={t("pages:traffic.detail.correlation.eventIdHint")}
          value={e.id}
          copyLabel={copyLabel}
          copiedLabel={copiedLabel}
          testId="event-id"
        />
        {/* The request id — what the gateway handed back on
            X-Nexus-Request-Id and what a support ticket quotes. It is the
            list filter's key, so it pivots: one request can produce a row
            per service it crossed. */}
        {e.externalRequestId ? (
          <CorrelationRow
            label={t("pages:traffic.detail.correlation.requestId")}
            labelHint={t("pages:traffic.detail.correlation.requestIdHint")}
            value={e.externalRequestId}
            onPivot={
              onPivot
                ? () =>
                    onPivot(
                      correlationPivotPatch(
                        "requestId",
                        e.externalRequestId ?? "",
                      ),
                    )
                : undefined
            }
            pivotHint={t("pages:traffic.detail.correlation.pivotRequest")}
            copyLabel={copyLabel}
            copiedLabel={copiedLabel}
            testId="request-id"
          />
        ) : null}
        {isGatewayTraffic && (
          <>
            <CorrelationRow
              label={t("pages:traffic.detail.correlation.endUserId")}
              value={e.endUserId}
              onPivot={
                onPivot && e.endUserId
                  ? () =>
                      onPivot(
                        correlationPivotPatch("endUserId", e.endUserId ?? ""),
                      )
                  : undefined
              }
              pivotHint={t("pages:traffic.detail.correlation.pivotEndUser")}
              copyLabel={copyLabel}
              copiedLabel={copiedLabel}
              testId="end-user-id"
            />
            <CorrelationRow
              label={t("pages:traffic.detail.correlation.sessionId")}
              value={e.sessionId}
              onPivot={
                onPivot && e.sessionId
                  ? () =>
                      onPivot(
                        correlationPivotPatch("sessionId", e.sessionId ?? ""),
                      )
                  : undefined
              }
              pivotHint={t("pages:traffic.detail.correlation.pivotSession")}
              copyLabel={copyLabel}
              copiedLabel={copiedLabel}
              testId="session-id"
            />
            {clientTags ? (
              <CorrelationRow
                label={t("pages:traffic.detail.correlation.clientTags")}
                value={clientTags}
                copyLabel={copyLabel}
                copiedLabel={copiedLabel}
                testId="client-tags"
              />
            ) : null}
          </>
        )}
        {/* The caller's own W3C trace, present only when they sent a
            traceparent. It pivots like the others: the traffic list filters on
            trace_id, so an operator arriving from their APM with a trace id in
            hand gets the gateway's rows for it. */}
        {e.traceId ? (
          <CorrelationRow
            label={t("pages:traffic.detail.correlation.traceId")}
            labelHint={t("pages:traffic.detail.correlation.traceIdHint")}
            value={e.traceId}
            onPivot={
              onPivot
                ? () =>
                    onPivot(correlationPivotPatch("traceId", e.traceId ?? ""))
                : undefined
            }
            pivotHint={t("pages:traffic.detail.correlation.pivotTrace")}
            copyLabel={copyLabel}
            copiedLabel={copiedLabel}
            testId="trace-id"
          />
        ) : null}
      </Stack>
    </div>
  );
}
