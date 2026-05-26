import { useState, useMemo, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { api } from '../../../api/client';
import { complianceApi } from '../../../api/services/compliance/compliance';
import { useToast } from '../../../context/ToastContext';
import type { TrinityLayerStats } from '../../../api/services/compliance/compliance';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Stack, Button, Select, Input,
} from '@/components/ui';
import styles from './ComplianceDashboardPage.module.css';

type RangePreset = '24h' | '7d' | '30d' | 'custom';

const PRESET_KEYS: Array<{ value: string; labelKey: string; fallback: string }> = [
  { value: '24h', labelKey: 'pages:proxy.compliance.presetLast24h', fallback: 'Last 24 hours' },
  { value: '7d', labelKey: 'pages:proxy.compliance.presetLast7d', fallback: 'Last 7 days' },
  { value: '30d', labelKey: 'pages:proxy.compliance.presetLast30d', fallback: 'Last 30 days' },
  { value: 'custom', labelKey: 'pages:proxy.compliance.presetCustom', fallback: 'Custom' },
];

function presetToRange(preset: Exclude<RangePreset, 'custom'>): { start: string; end: string } {
  const end = new Date();
  const start = new Date(end);
  switch (preset) {
    case '24h': start.setHours(start.getHours() - 24); break;
    case '7d': start.setDate(start.getDate() - 7); break;
    case '30d': start.setDate(start.getDate() - 30); break;
  }
  return { start: start.toISOString(), end: end.toISOString() };
}

function toLocalInput(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function fromLocalInput(local: string): string {
  return new Date(local).toISOString();
}

function formatNumber(n: number | null | undefined): string {
  if (n == null) return '—';
  return n.toLocaleString();
}

function formatPct(n: number | null | undefined, decimals = 1): string {
  if (n == null) return '—';
  return `${n.toFixed(decimals)}%`;
}

function formatRate(rate: number | null | undefined): string {
  if (rate == null) return '—';
  return formatPct(rate * 100);
}

function formatLatency(n: number | null | undefined): string {
  if (n == null) return '—';
  return `${Math.round(n)} ms`;
}

// ── Trinity layer card ─────────────────────────────────────────────────────

interface TrinityCardProps {
  label: string;
  layer: TrinityLayerStats | undefined;
  showBump: boolean;
}

function TrinityCard({ label, layer, showBump }: TrinityCardProps) {
  const { t } = useTranslation();
  if (!layer) {
    return (
      <Card>
        <div className={styles.trinityCard}>
          <div className={styles.trinityLayerLabel}>{label}</div>
          <div className={styles.noData}>{t('pages:proxy.compliance.trinityNoData', 'No data')}</div>
        </div>
      </Card>
    );
  }

  const rejectTotal = (layer.decisions?.REJECT_HARD ?? 0) + (layer.decisions?.BLOCK_SOFT ?? 0);
  const blockRate = (layer.blockRate * 100).toFixed(1);

  return (
    <Card>
      <div className={styles.trinityCard}>
        <div className={styles.trinityLayerLabel}>{label}</div>

        <div style={{ display: 'flex', gap: 'var(--g-space-6)', flexWrap: 'wrap' }}>
          <div className={styles.trinityMetric}>
            <div className={styles.trinityMetricLabel}>{t('pages:proxy.compliance.trinityTotal', 'Events')}</div>
            <div className={styles.trinityMetricValue}>{formatNumber(layer.totalEvents)}</div>
          </div>
          <div className={styles.trinityMetric}>
            <div className={styles.trinityMetricLabel}>{t('pages:proxy.compliance.trinityBlocked', 'Blocked')}</div>
            <div className={`${styles.trinityMetricValue} ${rejectTotal > 0 ? styles.trinityBlockRate : ''}`}>
              {formatNumber(rejectTotal)}
              <span style={{ fontSize: 'var(--g-font-size-base)', fontWeight: 'var(--g-font-weight-normal)', marginLeft: 'var(--g-space-1)' }}>
                ({blockRate}%)
              </span>
            </div>
          </div>
          {showBump && layer.coveragePercent !== undefined && (
            <div className={styles.trinityMetric}>
              <div className={styles.trinityMetricLabel}>{t('pages:proxy.compliance.trinityCoverage', 'TLS')}</div>
              <div className={`${styles.trinityMetricValue} ${styles.trinityCoverage}`}>
                {layer.coveragePercent.toFixed(1)}%
              </div>
            </div>
          )}
        </div>

        <div className={styles.trinityStatRow}>
          {layer.decisions && (
            <>
              <span className={`${styles.trinityDecisionBadge} ${styles.approve}`}>
                ✓ {formatNumber(layer.decisions.APPROVE)}
              </span>
              {layer.decisions.MODIFY > 0 && (
                <span className={`${styles.trinityDecisionBadge} ${styles.modify}`}>
                  ~ {formatNumber(layer.decisions.MODIFY)}
                </span>
              )}
              {rejectTotal > 0 && (
                <span className={`${styles.trinityDecisionBadge} ${styles.reject}`}>
                  ✕ {formatNumber(rejectTotal)}
                </span>
              )}
              {layer.decisions.ABSTAIN > 0 && (
                <span className={`${styles.trinityDecisionBadge} ${styles.abstain}`}>
                  ◌ {formatNumber(layer.decisions.ABSTAIN)}
                </span>
              )}
            </>
          )}
        </div>
      </div>
    </Card>
  );
}

// ── Top-blocked table ──────────────────────────────────────────────────────

interface TopTableProps {
  rows: { label: string; count: number }[];
  labelHeader: string;
  emptyLabel: string;
}

function TopTable({ rows, labelHeader, emptyLabel }: TopTableProps) {
  const { t } = useTranslation();
  if (rows.length === 0) {
    return <div className={styles.noData}>{emptyLabel}</div>;
  }
  return (
    <div className={styles.tableWrapper}>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>{labelHeader}</th>
            <th className={styles.tableRight}>{t('pages:proxy.compliance.colCount', 'Count')}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.label}>
              <td><code>{r.label}</code></td>
              <td className={styles.tableRight}>{formatNumber(r.count)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── Main page ──────────────────────────────────────────────────────────────

type TopBlockedTab = 'target' | 'reason' | 'sourceIp';

export function ComplianceDashboardPage() {
  const { t } = useTranslation();

  const presetOptions = useMemo(
    () => PRESET_KEYS.map((p) => ({ value: p.value, label: t(p.labelKey, p.fallback) })),
    [t],
  );

  const initial = useMemo(() => presetToRange('7d'), []);
  const [preset, setPreset] = useState<RangePreset>('7d');
  const [startTime, setStartTime] = useState(initial.start);
  const [endTime, setEndTime] = useState(initial.end);
  const [topTab, setTopTab] = useState<TopBlockedTab>('target');

  const handlePresetChange = useCallback((value: string) => {
    const next = value as RangePreset;
    setPreset(next);
    if (next !== 'custom') {
      const { start, end } = presetToRange(next);
      setStartTime(start);
      setEndTime(end);
    }
  }, []);

  const { data, loading, error, refetch } = useApi(
    () => complianceApi.getOverview(startTime, endTime),
    ['admin', 'compliance-overview', 'proxy', startTime, endTime],
  );

  const { addToast } = useToast();
  const [exporting, setExporting] = useState(false);
  const handleExport = useCallback(async () => {
    setExporting(true);
    try {
      await api.download(
        complianceApi.buildOverviewExportUrl({ startTime, endTime }),
        undefined,
        'compliance-events.csv',
      );
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      addToast(
        t('pages:proxy.compliance.exportFailed', { reason: msg, defaultValue: `Export failed: ${msg}` }),
        'error',
      );
    } finally {
      setExporting(false);
    }
  }, [startTime, endTime, addToast, t]);

  const kpis = data?.kpis;
  const trinity = data?.trinity;
  const hookHealth = data?.hookHealth;
  const topBlocked = data?.topBlocked;

  const topTableRows = useMemo(() => {
    if (!topBlocked) return [];
    switch (topTab) {
      case 'target': return topBlocked.byTarget;
      case 'reason': return topBlocked.byReasonCode;
      case 'sourceIp': return topBlocked.bySourceIp;
    }
  }, [topBlocked, topTab]);

  const topTableHeader = useMemo(() => {
    switch (topTab) {
      case 'target': return t('pages:proxy.compliance.colTargetHost', 'Target Host');
      case 'reason': return t('pages:proxy.compliance.colReasonCode', 'Reason Code');
      case 'sourceIp': return t('pages:proxy.compliance.colSourceIp', 'Source IP');
    }
  }, [topTab, t]);

  return (
    <>
      <PageHeader
        title={t('pages:proxy.compliance.title', 'Compliance Overview')}
        subtitle={t(
          'pages:proxy.compliance.subtitle',
          'Global enforcement health across AI Gateway, Network Proxy, and Agent — Trinity decisions, TLS coverage, and block statistics.',
        )}
      />

      <Stack gap="md">
        {/* Filter bar */}
        <Card>
          <Stack gap="sm">
            <div className={styles.sectionTitle}>
              {t('pages:proxy.compliance.filterTitle', 'Time Range')}
            </div>
            <div className={styles.filterBar}>
              <label className={styles.filterField}>
                {t('pages:proxy.compliance.preset', 'Preset')}
                <Select value={preset} onValueChange={handlePresetChange} options={presetOptions} />
              </label>
              <label className={styles.filterField}>
                {t('pages:proxy.compliance.startTime', 'Start')}
                <Input
                  type="datetime-local"
                  value={toLocalInput(startTime)}
                  onChange={(e) => {
                    setStartTime(fromLocalInput(e.target.value));
                    setPreset('custom');
                  }}
                />
              </label>
              <label className={styles.filterField}>
                {t('pages:proxy.compliance.endTime', 'End')}
                <Input
                  type="datetime-local"
                  value={toLocalInput(endTime)}
                  onChange={(e) => {
                    setEndTime(fromLocalInput(e.target.value));
                    setPreset('custom');
                  }}
                />
              </label>
              <Button variant="ghost" onClick={refetch}>
                {t('common:refresh', 'Refresh')}
              </Button>
              <Button variant="primary" loading={exporting} onClick={handleExport}>
                {t('pages:proxy.compliance.exportCsv', 'Export CSV')}
              </Button>
            </div>
            <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
              {t(
                'pages:proxy.compliance.windowHint',
                'Time window is capped at 366 days. CSV export covers all enforcement layers.',
              )}
            </div>
          </Stack>
        </Card>

        {error && <ErrorBanner message={error.message} onRetry={refetch} />}
        {loading && !data && <LoadingSpinner />}

        {/* KPI row */}
        {kpis && (
          <div className={styles.kpiRow}>
            <div className={styles.kpiCard}>
              <div className={styles.kpiLabel}>{t('pages:proxy.compliance.kpiRequests', 'Total Requests')}</div>
              <div className={styles.kpiValue}>{formatNumber(kpis.totalRequests)}</div>
              <div className={styles.kpiSub}>{t('pages:proxy.compliance.kpiRequestsSub', 'All enforcement layers')}</div>
            </div>
            <div className={styles.kpiCard}>
              <div className={styles.kpiLabel}>{t('pages:proxy.compliance.kpiBlocked', 'Blocked')}</div>
              <div className={`${styles.kpiValue} ${kpis.totalBlocked > 0 ? styles.kpiValueDanger : ''}`}>
                {formatNumber(kpis.totalBlocked)}
              </div>
              <div className={styles.kpiSub}>{formatRate(kpis.overallBlockRate)} {t('pages:proxy.compliance.kpiBlockRate', 'block rate')}</div>
            </div>
            <div className={styles.kpiCard}>
              <div className={styles.kpiLabel}>{t('pages:proxy.compliance.kpiTlsCoverage', 'TLS Coverage')}</div>
              <div className={`${styles.kpiValue} ${kpis.tlsCoveragePercent >= 90 ? styles.kpiValueSuccess : styles.kpiValueWarning}`}>
                {formatPct(kpis.tlsCoveragePercent)}
              </div>
              <div className={styles.kpiSub}>{t('pages:proxy.compliance.kpiTlsSub', 'Network Proxy + Agent')}</div>
            </div>
            <div className={styles.kpiCard}>
              <div className={styles.kpiLabel}>{t('pages:proxy.compliance.kpiHookErrors', 'Hook Errors')}</div>
              <div className={`${styles.kpiValue} ${kpis.hookErrorRate > 0.01 ? styles.kpiValueDanger : ''}`}>
                {formatRate(kpis.hookErrorRate)}
              </div>
              <div className={styles.kpiSub}>{t('pages:proxy.compliance.kpiHookErrorsSub', 'of hook evaluations')}</div>
            </div>
          </div>
        )}

        {/* Enforcement Trinity */}
        <Card>
          <Stack gap="sm">
            <div className={styles.sectionTitle}>
              {t('pages:proxy.compliance.trinityTitle', 'Enforcement Trinity')}
            </div>
            <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)', marginBottom: 'var(--g-space-2)' }}>
              {t('pages:proxy.compliance.trinitySubtitle', 'Per-layer compliance activity for the selected time window')}
            </div>
            <div className={styles.trinityGrid}>
              <TrinityCard
                label={t('pages:proxy.compliance.trinityAIGateway', 'AI Gateway')}
                layer={trinity?.aiGateway}
                showBump={false}
              />
              <TrinityCard
                label={t('pages:proxy.compliance.trinityNetworkProxy', 'Network Proxy')}
                layer={trinity?.complianceProxy}
                showBump
              />
              <TrinityCard
                label={t('pages:proxy.compliance.trinityAgent', 'Agent')}
                layer={trinity?.agent}
                showBump
              />
            </div>
          </Stack>
        </Card>

        {/* Hook Decision Health */}
        <Card>
          <Stack gap="sm">
            <div className={styles.sectionTitle}>
              {t('pages:proxy.compliance.hookHealthTitle', 'Hook Decision Health')}
            </div>
            {hookHealth ? (
              <Stack gap="md">
                <div className={styles.hookStatRow}>
                  <div className={styles.hookStat}>
                    <div className={styles.hookStatLabel}>{t('pages:proxy.compliance.statTotal', 'Total')}</div>
                    <div className={styles.hookStatValue}>{formatNumber(hookHealth.total)}</div>
                  </div>
                  <div className={styles.hookStat}>
                    <div className={styles.hookStatLabel}>{t('pages:proxy.compliance.statAllow', 'Allow')}</div>
                    <div className={styles.hookStatValue}>{formatNumber(hookHealth.byDecision.allow)}</div>
                  </div>
                  <div className={styles.hookStat}>
                    <div className={styles.hookStatLabel}>{t('pages:proxy.compliance.statDeny', 'Deny')}</div>
                    <div className={`${styles.hookStatValue} ${(hookHealth.byDecision.deny ?? 0) > 0 ? styles.kpiValueDanger : ''}`}>
                      {formatNumber(hookHealth.byDecision.deny)}
                    </div>
                  </div>
                  <div className={styles.hookStat}>
                    <div className={styles.hookStatLabel}>{t('pages:proxy.compliance.statError', 'Error')}</div>
                    <div className={`${styles.hookStatValue} ${(hookHealth.byDecision.error ?? 0) > 0 ? styles.kpiValueWarning : ''}`}>
                      {formatNumber(hookHealth.byDecision.error)}
                    </div>
                  </div>
                  <div className={styles.hookStat}>
                    <div className={styles.hookStatLabel}>{t('pages:proxy.compliance.statUnknown', 'Unknown')}</div>
                    <div className={styles.hookStatValue}>{formatNumber(hookHealth.byDecision.unknown)}</div>
                  </div>
                </div>

                {(hookHealth.latencyP50 != null || hookHealth.latencyP95 != null) && (
                  <div className={styles.hookLatencyRow}>
                    <div className={styles.hookLatencyStat}>
                      <div className={styles.hookLatencyLabel}>{t('pages:proxy.compliance.statLatencyP50', 'Latency p50')}</div>
                      <div className={styles.hookLatencyValue}>{formatLatency(hookHealth.latencyP50)}</div>
                    </div>
                    <div className={styles.hookLatencyStat}>
                      <div className={styles.hookLatencyLabel}>{t('pages:proxy.compliance.statLatencyP95', 'Latency p95')}</div>
                      <div className={styles.hookLatencyValue}>{formatLatency(hookHealth.latencyP95)}</div>
                    </div>
                    <div className={styles.hookLatencyStat}>
                      <div className={styles.hookLatencyLabel}>{t('pages:proxy.compliance.statLatencyP99', 'Latency p99')}</div>
                      <div className={styles.hookLatencyValue}>{formatLatency(hookHealth.latencyP99)}</div>
                    </div>
                  </div>
                )}

                {hookHealth.topReasonCodes.length > 0 && (
                  <div>
                    <div className={styles.sectionTitle} style={{ fontSize: 'var(--g-font-size-md)' }}>
                      {t('pages:proxy.compliance.topDenyReasons', 'Top Deny Reason Codes')}
                    </div>
                    <TopTable
                      rows={hookHealth.topReasonCodes}
                      labelHeader={t('pages:proxy.compliance.colReasonCode', 'Reason Code')}
                      emptyLabel={t('pages:proxy.compliance.noDenyEvents', 'No deny events in this window')}
                    />
                  </div>
                )}
              </Stack>
            ) : (
              <div className={styles.noData}>—</div>
            )}
          </Stack>
        </Card>

        {/* Top Blocked */}
        <Card>
          <Stack gap="sm">
            <div className={styles.sectionTitle}>
              {t('pages:proxy.compliance.topBlockedTitle', 'Top Blocked')}
            </div>
            <div className={styles.topBlockedTabs}>
              {(
                [
                  { key: 'target', labelKey: 'pages:proxy.compliance.topTargets', fallback: 'By Target Host' },
                  { key: 'reason', labelKey: 'pages:proxy.compliance.topReasons', fallback: 'By Reason Code' },
                  { key: 'sourceIp', labelKey: 'pages:proxy.compliance.topSources', fallback: 'By Source IP' },
                ] as const
              ).map(({ key, labelKey, fallback }) => (
                <button
                  key={key}
                  type="button"
                  className={`${styles.topBlockedTab} ${topTab === key ? styles.topBlockedTabActive : ''}`}
                  onClick={() => setTopTab(key)}
                >
                  {t(labelKey, fallback)}
                </button>
              ))}
            </div>
            <TopTable
              rows={topTableRows}
              labelHeader={topTableHeader}
              emptyLabel={t('pages:proxy.compliance.emptyNone', 'None')}
            />
          </Stack>
        </Card>
      </Stack>
    </>
  );
}
