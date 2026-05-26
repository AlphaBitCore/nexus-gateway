/**
 * InfraDiagModePage — Agent Diagnostic Mode admin view.
 *
 * Two-section layout (per spec §11.4):
 *
 *   1. Active windows — table of `thing_diag_mode_window` rows with
 *      `endedAt > now()`, polled every 10 s. Each row exposes a "Disable"
 *      action gated behind a confirm dialog.
 *
 *   2. Bulk enable form — filter (thingIds + agentVersion + os) plus a
 *      window/reason combo. The user clicks "Resolve preview" first, which
 *      lists agents and applies the filter client-side so the operator sees
 *      the exact match count before committing. The CP server caps the
 *      filter at 500 ids; the UI duplicates that gate so we never POST a
 *      request the server is going to 400.
 *
 * Bulk semantics: the CP returns 207 Multi-Status when one or more per-thing
 * enables fail. The shared `useMutation` toast surfaces a single message,
 * so we drive the bulk action with a manual `try { bulk() }` instead and
 * render a per-item result panel reflecting the `items[i].ok` array.
 *
 * Why a client-side preview rather than a server "dryRun":
 *   The CP bulk handler does not expose a dry-run mode today. Replicating
 *   the filter against `/api/admin/nodes?type=agent` keeps the API surface
 *   stable and bounded — agent fleets cap in the low thousands and we only
 *   show a count, never the full list, so memory stays modest.
 */
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { diagModeApi } from '@/api/services/infrastructure/diag/diagmode';
import type {
  BulkDiagModeResult,
  DiagModeWindow,
} from '@/api/services/infrastructure/diag/diagmode';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { Node } from '@/api/services/infrastructure/nodes/hub';
import {
  PageHeader, Stack, Card, Button, Input, Textarea, Select,
  AlertDialog, LoadingSpinner, ErrorBanner, FormField,
} from '@/components/ui';
import styles from './InfraDiagModePage.module.css';

/** Active-windows polling interval (ms). Spec §11.4: "Refresh every 10 s". */
const REFRESH_INTERVAL_MS = 10_000;

/** Server-side cap on bulk filter resolution; mirror it client-side. */
const MAX_BULK_THINGS = 500;

/** Window presets — values in milliseconds, capped at 24 h by the server. */
const WINDOW_OPTIONS: Array<{ value: string; ms: number }> = [
  { value: '1h', ms: 1 * 60 * 60 * 1000 },
  { value: '4h', ms: 4 * 60 * 60 * 1000 },
  { value: '12h', ms: 12 * 60 * 60 * 1000 },
  { value: '24h', ms: 24 * 60 * 60 * 1000 },
];

const ANY_VALUE = '__any__';

/** Format a future ISO timestamp as "Nm left" / "Nh left" relative to now. */
function fmtEndsIn(endedAt: string): string {
  const ms = new Date(endedAt).getTime() - Date.now();
  if (Number.isNaN(ms) || ms <= 0) return '0m';
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rem = minutes % 60;
  return rem === 0 ? `${hours}h` : `${hours}h ${rem}m`;
}

function fmtTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

/**
 * Apply the UI filter (thingIds, agentVersion, os) to the agent list. Mirrors
 * the CP `ResolveBulkAgents` semantics on `node.version` (== agentVersion) and
 * `node.metadata.os` (== os, if present). When `thingIds` is non-empty we use
 * it directly and ignore the other criteria — same as the server.
 */
function applyFilter(
  agents: Node[],
  thingIds: string[],
  agentVersion: string,
  os: string,
): Node[] {
  if (thingIds.length > 0) {
    const set = new Set(thingIds);
    return agents.filter((a) => set.has(a.id));
  }
  return agents.filter((a) => {
    if (agentVersion && (a.version ?? '') !== agentVersion) return false;
    if (os) {
      // Best-effort match against `metadata.os` when the Hub publishes it; if
      // the field is missing we drop the agent rather than over-match.
      const meta = (a as unknown as { metadata?: Record<string, unknown> }).metadata ?? {};
      const agentOs = String(meta.os ?? '');
      if (agentOs !== os) return false;
    }
    return true;
  });
}

/** Parse a textarea blob into a deduped, trimmed list of thing_ids. */
function parseThingIds(raw: string): string[] {
  const out = new Set<string>();
  for (const line of raw.split(/[\r\n,]+/)) {
    const t = line.trim();
    if (t) out.add(t);
  }
  return Array.from(out);
}

export default function InfraDiagModePage() {
  const { t } = useTranslation('pages');

  // ── Active windows ──
  const activeWindows = useApi<DiagModeWindow[]>(
    () => diagModeApi.list(),
    ['admin', 'diag-mode', 'active'],
  );

  // Poll every 10 s. We piggy-back on `refetch` rather than configuring
  // refetchInterval in useApi so the cadence stays scoped to this page.
  useEffect(() => {
    const id = window.setInterval(() => activeWindows.refetch(), REFRESH_INTERVAL_MS);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [confirmDisable, setConfirmDisable] = useState<DiagModeWindow | null>(null);

  const disableMutation = useMutation<string, unknown>(
    (thingId: string) => diagModeApi.disable(thingId),
    {
      successMessage: t('infrastructure.diagMode.disableSuccess'),
      errorMessage: t('infrastructure.diagMode.disableFailed'),
      invalidateQueries: [['admin', 'diag-mode', 'active']],
      onSuccess: () => activeWindows.refetch(),
    },
  );

  const handleConfirmDisable = () => {
    const target = confirmDisable;
    setConfirmDisable(null);
    if (target) {
      disableMutation.mutate(target.nodeId).catch(() => undefined);
    }
  };

  // ── Bulk enable form state ──
  const [agentVersion, setAgentVersion] = useState<string>('');
  const [os, setOs] = useState<string>('');
  const [thingIdsRaw, setThingIdsRaw] = useState<string>('');
  const [windowKey, setWindowKey] = useState<string>('1h');
  const [reason, setReason] = useState<string>('');

  const [previewIds, setPreviewIds] = useState<string[] | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);

  const [bulkResult, setBulkResult] = useState<BulkDiagModeResult | null>(null);
  const [bulkError, setBulkError] = useState<string | null>(null);
  const [bulkLoading, setBulkLoading] = useState(false);

  // Pasted thingIds parsed once per change. When non-empty the other filter
  // dropdowns are ignored (matches server behavior).
  const parsedThingIds = useMemo(() => parseThingIds(thingIdsRaw), [thingIdsRaw]);

  const previewCount = previewIds?.length ?? null;

  // Preview action: fetch agent list once and filter client-side.
  const handlePreview = async () => {
    setPreviewLoading(true);
    setPreviewError(null);
    setBulkResult(null);
    setBulkError(null);
    try {
      const resp = await hubApi.listNodes({ type: 'agent', pageSize: 1000 });
      const filtered = applyFilter(
        resp.nodes ?? [],
        parsedThingIds,
        agentVersion,
        os,
      );
      setPreviewIds(filtered.map((n) => n.id));
    } catch (err) {
      setPreviewError(err instanceof Error ? err.message : String(err));
      setPreviewIds(null);
    } finally {
      setPreviewLoading(false);
    }
  };

  // Recompute preview on filter change to avoid stale counts. We lazily
  // re-run only when the user already pulled a preview at least once.
  useEffect(() => {
    if (previewIds !== null) {
      setPreviewIds(null);
      setBulkResult(null);
      setBulkError(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentVersion, os, thingIdsRaw]);

  const validReason = reason.trim().length > 0;
  const validCount = previewCount !== null && previewCount > 0 && previewCount <= MAX_BULK_THINGS;

  const canSubmit = validCount && validReason && !bulkLoading;

  const handleBulkEnable = async () => {
    if (!canSubmit || previewCount === null) return;
    setBulkLoading(true);
    setBulkResult(null);
    setBulkError(null);
    try {
      const ms = WINDOW_OPTIONS.find((w) => w.value === windowKey)?.ms ?? 60 * 60 * 1000;
      const until = new Date(Date.now() + ms).toISOString();
      // When the operator pasted thing_ids we pass them verbatim — that's the
      // exact list the preview displayed, so the server resolves the same set
      // even if a node-list refetch would now return slightly different rows.
      const filter = parsedThingIds.length > 0
        ? { nodeIds: parsedThingIds }
        : { agentVersion: agentVersion || undefined, os: os || undefined };
      const result = await diagModeApi.bulk({
        filter,
        until,
        reason: reason.trim(),
      });
      setBulkResult(result);
      activeWindows.refetch();
    } catch (err) {
      setBulkError(err instanceof Error ? err.message : String(err));
    } finally {
      setBulkLoading(false);
    }
  };

  // Banner state derived from bulkResult.
  const bulkSucceeded = bulkResult && bulkResult.ok && bulkResult.failed === 0;
  const bulkPartial = bulkResult && bulkResult.failed > 0 && bulkResult.failed < bulkResult.total;
  const bulkAllFailed = bulkResult && bulkResult.failed > 0 && bulkResult.failed === bulkResult.total;
  const bulkOkCount = bulkResult ? bulkResult.total - bulkResult.failed : 0;
  const bulkFailedItems = bulkResult ? bulkResult.items.filter((i) => !i.ok) : [];

  // ── Render ──
  const windows = activeWindows.data ?? [];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.diagMode.title')}
        subtitle={t('infrastructure.diagMode.description')}
      />

      {/* ── Active windows ── */}
      <Card>
        <Stack gap="sm">
          <div className={styles.actionRow} style={{ justifyContent: 'space-between' }}>
            <h3 className={styles.sectionTitle}>{t('infrastructure.diagMode.activeWindows')}</h3>
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={() => activeWindows.refetch()}
            >
              {t('infrastructure.diagMode.refresh')}
            </Button>
          </div>
          {activeWindows.error ? (
            <ErrorBanner
              message={activeWindows.error.message}
              onRetry={activeWindows.refetch}
            />
          ) : activeWindows.loading && windows.length === 0 ? (
            <LoadingSpinner />
          ) : windows.length === 0 ? (
            <div className={styles.empty}>
              {t('infrastructure.diagMode.activeEmpty')}
            </div>
          ) : (
            <table className={styles.dataTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.diagMode.colThing')}</th>
                  <th>{t('infrastructure.diagMode.colStarted')}</th>
                  <th>{t('infrastructure.diagMode.colEndsIn')}</th>
                  <th>{t('infrastructure.diagMode.colSetBy')}</th>
                  <th>{t('infrastructure.diagMode.colReason')}</th>
                  <th>{t('infrastructure.diagMode.colActions')}</th>
                </tr>
              </thead>
              <tbody>
                {windows.map((w) => (
                  <tr key={w.id}>
                    <td className={styles.codeCell}>{w.nodeId}</td>
                    <td>{fmtTime(w.startedAt)}</td>
                    <td>{fmtEndsIn(w.endedAt)}</td>
                    <td>{w.setBy ?? '—'}</td>
                    <td>{w.reason ?? '—'}</td>
                    <td>
                      <Button
                        type="button"
                        variant="secondary"
                        size="sm"
                        onClick={() => setConfirmDisable(w)}
                      >
                        {t('infrastructure.diagMode.disable')}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className={styles.previewBanner}>
            {t('infrastructure.diagMode.autoRefresh')}
          </p>
        </Stack>
      </Card>

      {/* ── Bulk enable ── */}
      <Card>
        <Stack gap="md">
          <h3 className={styles.sectionTitle}>{t('infrastructure.diagMode.bulkTitle')}</h3>

          <div className={styles.formGrid}>
            <FormField
              label={t('infrastructure.diagMode.filterAgentVersion')}
              helpText={t('infrastructure.diagMode.filterAgentVersionHelp')}
            >
              <Input
                type="text"
                placeholder={t('infrastructure.diagMode.filterAgentVersionPlaceholder')}
                value={agentVersion}
                onChange={(e) => setAgentVersion(e.target.value)}
                disabled={parsedThingIds.length > 0}
              />
            </FormField>

            <FormField
              label={t('infrastructure.diagMode.filterOs')}
              helpText={t('infrastructure.diagMode.filterOsHelp')}
            >
              <Select
                value={os || ANY_VALUE}
                onValueChange={(v) => setOs(v === ANY_VALUE ? '' : v)}
                disabled={parsedThingIds.length > 0}
                options={[
                  { value: ANY_VALUE, label: t('infrastructure.diagMode.osAny') },
                  { value: 'darwin', label: 'macOS (darwin)' },
                  { value: 'linux', label: 'Linux' },
                  { value: 'windows', label: 'Windows' },
                ]}
              />
            </FormField>

            <FormField
              label={t('infrastructure.diagMode.filterThingIds')}
              helpText={t('infrastructure.diagMode.filterThingIdsHelp')}
              className={styles.formGridFull}
            >
              <Textarea
                rows={3}
                placeholder={t('infrastructure.diagMode.filterThingIdsPlaceholder')}
                value={thingIdsRaw}
                onChange={(e) => setThingIdsRaw(e.target.value)}
              />
            </FormField>

            <FormField label={t('infrastructure.diagMode.window')}>
              <Select
                value={windowKey}
                onValueChange={setWindowKey}
                options={WINDOW_OPTIONS.map((w) => ({
                  value: w.value,
                  label: t(`infrastructure.diagMode.window_${w.value}`),
                }))}
              />
            </FormField>

            <FormField
              label={t('infrastructure.diagMode.reason')}
              required
              error={!validReason && reason.length > 0 ? t('infrastructure.diagMode.reasonRequired') : undefined}
              className={styles.formGridFull}
            >
              <Input
                type="text"
                placeholder={t('infrastructure.diagMode.reasonPlaceholder')}
                value={reason}
                onChange={(e) => setReason(e.target.value)}
              />
            </FormField>
          </div>

          {/* Preview row */}
          <Stack gap="xs">
            <div className={styles.actionRow}>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                loading={previewLoading}
                onClick={handlePreview}
              >
                {t('infrastructure.diagMode.previewButton')}
              </Button>
              {previewCount !== null && (
                <span className={styles.previewBanner}>
                  {previewCount === 0 ? (
                    t('infrastructure.diagMode.previewEmpty')
                  ) : (
                    <>
                      <span className={styles.previewCount}>
                        {t('infrastructure.diagMode.previewCount', { count: previewCount })}
                      </span>
                      {previewCount > MAX_BULK_THINGS && (
                        <span className={styles.validationError} style={{ marginLeft: 'var(--g-space-2)' }}>
                          {t('infrastructure.diagMode.previewOverLimit', { max: MAX_BULK_THINGS })}
                        </span>
                      )}
                    </>
                  )}
                </span>
              )}
            </div>
            {previewError && <ErrorBanner message={previewError} />}
          </Stack>

          <div className={styles.actionRow}>
            <Button
              type="button"
              variant="primary"
              size="md"
              disabled={!canSubmit}
              loading={bulkLoading}
              onClick={handleBulkEnable}
            >
              {t('infrastructure.diagMode.bulkSubmit')}
            </Button>
            {previewCount === null && (
              <span className={styles.previewBanner}>
                {t('infrastructure.diagMode.bulkPreviewFirst')}
              </span>
            )}
          </div>
        </Stack>
      </Card>

      {/* ── Bulk result panels ── */}
      {bulkError && <ErrorBanner message={bulkError} />}

      {bulkSucceeded && (
        <Card className={styles.successCard}>
          <Stack gap="xs">
            <h3 className={styles.sectionTitle}>
              {t('infrastructure.diagMode.bulkSuccessTitle')}
            </h3>
            <p>
              {t('infrastructure.diagMode.bulkSuccessSummary', {
                count: bulkResult.total,
              })}
            </p>
          </Stack>
        </Card>
      )}

      {bulkPartial && bulkResult && (
        <Card className={styles.warningCard}>
          <Stack gap="sm">
            <h3 className={styles.sectionTitle}>
              {t('infrastructure.diagMode.bulkPartialTitle')}
            </h3>
            <p>
              {t('infrastructure.diagMode.bulkPartialSummary', {
                ok: bulkOkCount,
                fail: bulkResult.failed,
              })}
            </p>
            <table className={styles.dataTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.diagMode.colThing')}</th>
                  <th>{t('infrastructure.diagMode.colError')}</th>
                </tr>
              </thead>
              <tbody>
                {bulkFailedItems.map((item) => (
                  <tr key={item.nodeId}>
                    <td className={styles.codeCell}>{item.nodeId}</td>
                    <td>{item.error ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Stack>
        </Card>
      )}

      {bulkAllFailed && bulkResult && (
        <Card className={styles.warningCard}>
          <Stack gap="sm">
            <h3 className={styles.sectionTitle}>
              {t('infrastructure.diagMode.bulkFailedTitle')}
            </h3>
            <p>
              {t('infrastructure.diagMode.bulkFailedSummary', {
                count: bulkResult.failed,
              })}
            </p>
            <table className={styles.dataTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.diagMode.colThing')}</th>
                  <th>{t('infrastructure.diagMode.colError')}</th>
                </tr>
              </thead>
              <tbody>
                {bulkFailedItems.map((item) => (
                  <tr key={item.nodeId}>
                    <td className={styles.codeCell}>{item.nodeId}</td>
                    <td>{item.error ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Stack>
        </Card>
      )}

      {/* ── Disable confirm dialog ── */}
      <AlertDialog
        open={!!confirmDisable}
        onOpenChange={(open) => {
          if (!open) setConfirmDisable(null);
        }}
        title={t('infrastructure.diagMode.disableConfirmTitle')}
        description={t('infrastructure.diagMode.disableConfirmDesc', {
          thingId: confirmDisable?.nodeId ?? '',
        })}
        confirmLabel={t('infrastructure.diagMode.disable')}
        cancelLabel={t('infrastructure.diagMode.cancel')}
        onConfirm={handleConfirmDisable}
        variant="danger"
      />
    </Stack>
  );
}
