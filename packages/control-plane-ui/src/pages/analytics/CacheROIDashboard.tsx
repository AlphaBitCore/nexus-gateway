import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { analyticsApi, type CacheROISummary, type CacheROIByAdapter } from '@/api/services/overview/analytics';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import { useApi } from '@/hooks/useApi';
import {
  Card,
  Stack,
  ErrorBanner,
  Skeleton,
  PageHeader,
  Badge,
  Button,
  Tooltip,
  AnimatedNumber,
} from '@/components/ui';
import { useTheme } from '@/theme/useTheme';
import { getSemanticColor, getTooltipStyle } from '@nexus-gateway/ui-shared';
import {
  LineChart, XAxis, YAxis, Tooltip as RechartsTooltip, ResponsiveContainer,
  Line, CartesianGrid, Legend, ReferenceLine,
} from 'recharts';
import styles from './CacheROIDashboard.module.css';
import { formatTokens } from '@/lib/format';

type TimeRange = '7d' | '30d' | '90d';

function buildParams(range: TimeRange): { start: string; end: string } {
  const now = new Date();
  const days = range === '7d' ? 7 : range === '30d' ? 30 : 90;
  return {
    start: new Date(now.getTime() - days * 86_400_000).toISOString(),
    end: now.toISOString(),
  };
}

function fmtUSD(v: number): string {
  if (v === 0) return '$0.00';
  if (Math.abs(v) < 0.01) return `$${v.toFixed(6)}`;
  return `$${v.toFixed(4)}`;
}

function netSavingsColor(v: number): string {
  return v >= 0 ? 'var(--color-success)' : 'var(--color-danger)';
}

// Jobs that make up the full rollup pipeline, in dependency order.
const ROLLUP_JOBS = ['rollup-5m', 'merge-1h', 'merge-1d', 'merge-1mo'] as const;

interface SummaryCardProps {
  label: React.ReactNode;
  value: React.ReactNode;
  sub?: string;
  valueColor?: string;
}

function SummaryCard({ label, value, sub, valueColor }: SummaryCardProps) {
  return (
    <Card className={styles.summaryCard}>
      <div className={styles.summaryInner}>
        <div>
          <div className={styles.summaryLabel}>{label}</div>
          <div className={styles.summaryValue} style={valueColor ? { color: valueColor } : undefined}>{value}</div>
        </div>
        {sub ? <div className={styles.summarySub}>{sub}</div> : <div />}
      </div>
    </Card>
  );
}

interface SectionBlockProps {
  title: string;
  badge: string;
  accentColor: string;
  description: string;
  children: React.ReactNode;
}

function SectionBlock({ title, badge, accentColor, description, children }: SectionBlockProps) {
  return (
    <div className={styles.section}>
      <div className={styles.sectionHead} style={{ borderLeftColor: accentColor }}>
        <span className={styles.sectionTitle}>{title}</span>
        <span className={styles.sectionBadge} style={{ color: accentColor, borderColor: accentColor }}>
          {badge}
        </span>
        <span className={styles.sectionDesc}>{description}</span>
      </div>
      {children}
    </div>
  );
}

function LayerTag({ label, tooltip }: { label: string; tooltip: string }) {
  return (
    <Tooltip content={tooltip} side="top">
      <span className={styles.layerTag}>{label}</span>
    </Tooltip>
  );
}


export function CacheROIDashboard() {
  const { t } = useTranslation();
  const { resolvedMode } = useTheme();
  const [range, setRange] = useState<TimeRange>('30d');
  const [triggering, setTriggering] = useState(false);
  const [triggered, setTriggered] = useState(false);

  const params = buildParams(range);

  const { data, loading, error, refetch } = useApi<CacheROISummary>(
    () => analyticsApi.cacheROI(params),
    ['admin', 'analytics', 'cache-roi', range],
  );

  const tooltipStyle = getTooltipStyle(resolvedMode);
  const savingsColor = getSemanticColor(resolvedMode, 'cacheHits');
  const writeColor = getSemanticColor(resolvedMode, 'cost');
  const netColor = getSemanticColor(resolvedMode, 'requests');
  const gatewayColor = getSemanticColor(resolvedMode, 'gatewaySavings');
  const promptCacheColor = getSemanticColor(resolvedMode, 'promptCache');
  const totalNetColor = getSemanticColor(resolvedMode, 'totalSavings');

  const daily = (data?.daily ?? []).map(d => ({
    ...d,
    totalNetSavingsUsd: (d.gatewayCacheSavingsUsd ?? 0) + d.cacheNetSavingsUsd,
  }));

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const d = data!;

  const combinedSavingsUsd = (d.totalGatewayCacheSavingsUsd ?? 0) + d.totalCacheNetSavingsUsd;
  const totalHits = (d.gatewayCacheHitCount ?? 0) + d.requestsWithCacheHit;

  const readMultiplier = d.totalCacheCreationTokens > 0
    ? d.totalCacheReadTokens / d.totalCacheCreationTokens
    : null;

  const gws = d.totalGatewayCacheSavingsUsd ?? 0;
  const netSavingsAll = gws + d.totalCacheNetSavingsUsd;
  const grossCostAll  = (d.totalEstimatedCostUsd ?? 0) + netSavingsAll;
  const savingsRate = grossCostAll > 0 ? (netSavingsAll / grossCostAll) * 100 : null;
  const avgSavingsPerHit = totalHits > 0 ? combinedSavingsUsd / totalHits : null;
  const roiMultiplier = d.totalCacheWriteCostUsd > 0
    ? combinedSavingsUsd / d.totalCacheWriteCostUsd
    : null;

  const handleTriggerRollup = async () => {
    setTriggering(true);
    try {
      await Promise.all(ROLLUP_JOBS.map(id => hubApi.triggerJob(id)));
      setTriggered(true);
      // Auto-refetch after 90 s to pick up newly computed rollup data.
      setTimeout(() => {
        setTriggered(false);
        refetch();
      }, 90_000);
    } finally {
      setTriggering(false);
    }
  };

  const activeAdapters: CacheROIByAdapter[] = (d.byAdapter ?? []).filter(
    a => a.gatewayCacheHitCount > 0 || (a.gatewayCacheSavingsUsd ?? 0) > 0 ||
         a.requestsWithCacheHit > 0 || a.cacheWriteCostUsd > 0 || a.cacheReadSavingsUsd > 0,
  );

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:analytics.cacheRoi.title')}
        subtitle={t('pages:analytics.cacheRoi.subtitle')}
        action={
          <div className={styles.rangeButtons}>
            {(['7d', '30d', '90d'] as TimeRange[]).map(r => (
              <Button
                key={r}
                onClick={() => setRange(r)}
                size="sm"
                variant={range === r ? 'primary' : 'secondary'}
                aria-pressed={range === r}
              >
                {r}
              </Button>
            ))}
          </div>
        }
      />

      {/* Rollup not-ready banner — shown when data is served from raw traffic_event */}
      {d.dataSource === 'direct' && (
        <div className={styles.rollupBanner}>
          <span className={styles.rollupBannerText}>
            {t('pages:analytics.cacheRoi.rollupNotReady')}
          </span>
          {triggered ? (
            <span className={styles.successText}>
              {t('pages:analytics.cacheRoi.rollupTriggered')}
            </span>
          ) : (
            <Button size="sm" variant="secondary" onClick={handleTriggerRollup} disabled={triggering}>
              {triggering
                ? t('pages:analytics.cacheRoi.triggering')
                : t('pages:analytics.cacheRoi.triggerRollup')}
            </Button>
          )}
        </div>
      )}

      {/* Hero strip — cross-layer efficiency KPIs */}
      <div className={styles.heroGrid}>
        <SummaryCard
          label={
            <>
              {t('pages:analytics.cacheRoi.combinedSavings')}
              <LayerTag label={t('pages:analytics.cacheRoi.sectionGateway') + ' + ' + t('pages:analytics.cacheRoi.sectionProvider')} tooltip={t('pages:analytics.cacheRoi.tooltipCombinedSavings')} />
            </>
          }
          value={<AnimatedNumber value={combinedSavingsUsd} precision={4} format={fmtUSD} />}
          sub={t('pages:analytics.cacheRoi.overDays', { days: d.periodDays })}
          valueColor={netSavingsColor(combinedSavingsUsd)}
        />
        <SummaryCard
          label={
            <>
              {t('pages:analytics.cacheRoi.savingsRate')}
              <LayerTag label={t('pages:analytics.cacheRoi.sectionGateway') + ' + ' + t('pages:analytics.cacheRoi.sectionProvider')} tooltip={t('pages:analytics.cacheRoi.tooltipCombinedSavings')} />
              <Tooltip content={t('pages:analytics.cacheRoi.savingsRateFormula')} side="top">
                <span className={styles.questionBadge}>?</span>
              </Tooltip>
            </>
          }
          value={savingsRate != null ? <AnimatedNumber value={savingsRate} format={(n) => `${n.toFixed(0)}%`} /> : '—'}
          sub={savingsRate != null ? t('pages:analytics.cacheRoi.savingsRateSub') : undefined}
          valueColor={savingsRate != null ? (savingsRate >= 50 ? 'var(--color-success)' : 'var(--color-warning)') : undefined}
        />
        <SummaryCard
          label={
            <>
              {t('pages:analytics.cacheRoi.roiMultiplier')}
              <LayerTag label={t('pages:analytics.cacheRoi.sectionGateway') + ' + ' + t('pages:analytics.cacheRoi.sectionProvider')} tooltip={t('pages:analytics.cacheRoi.tooltipCombinedSavings')} />
            </>
          }
          value={roiMultiplier !== null ? <AnimatedNumber value={roiMultiplier} precision={1} format={(n) => `${n.toFixed(1)}×`} /> : '—'}
          sub={roiMultiplier !== null ? t('pages:analytics.cacheRoi.roiMultiplierSub') : undefined}
          valueColor={roiMultiplier !== null && roiMultiplier >= 1 ? 'var(--color-success)' : undefined}
        />
        {avgSavingsPerHit !== null && (
          <SummaryCard
            label={t('pages:analytics.cacheRoi.avgSavingsPerHit')}
            value={<AnimatedNumber value={avgSavingsPerHit} precision={4} format={fmtUSD} />}
            sub={t('pages:analytics.cacheRoi.avgSavingsPerHitSub')}
          />
        )}
      </div>

      {/* Gateway Cache section (removed misleading L1–L3 badge — only one gateway cache exists). */}
      <SectionBlock
        title={t('pages:analytics.cacheRoi.sectionGateway')}
        badge=""
        accentColor={gatewayColor}
        description={t('pages:analytics.cacheRoi.sectionGatewayDesc')}
      >
        <div className={styles.sectionCards2}>
          <SummaryCard
            label={t('pages:analytics.cacheRoi.gatewaySavings')}
            value={<AnimatedNumber value={d.totalGatewayCacheSavingsUsd ?? 0} precision={4} format={fmtUSD} />}
            sub={t('pages:analytics.cacheRoi.gatewaySavingsSub')}
            valueColor={(d.totalGatewayCacheSavingsUsd ?? 0) > 0 ? 'var(--color-success)' : undefined}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.gatewayCacheHits')}
            value={<AnimatedNumber value={d.gatewayCacheHitCount ?? 0} />}
            sub={t('pages:analytics.cacheRoi.gatewayCacheHitsSub')}
          />
        </div>
      </SectionBlock>

      {/* Provider Prompt Cache section (removed misleading L4 badge). */}
      <SectionBlock
        title={t('pages:analytics.cacheRoi.sectionProvider')}
        badge=""
        accentColor={promptCacheColor}
        description={t('pages:analytics.cacheRoi.sectionProviderDesc')}
      >
        <div className={styles.sectionCards4}>
          <SummaryCard
            label={t('pages:analytics.cacheRoi.netSavings')}
            value={<AnimatedNumber value={d.totalCacheNetSavingsUsd} precision={4} format={fmtUSD} />}
            sub={t('pages:analytics.cacheRoi.overDays', { days: d.periodDays })}
            valueColor={netSavingsColor(d.totalCacheNetSavingsUsd)}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.readSavings')}
            value={<AnimatedNumber value={d.totalCacheReadSavingsUsd} precision={4} format={fmtUSD} />}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.writeCost')}
            value={<AnimatedNumber value={d.totalCacheWriteCostUsd} precision={4} format={fmtUSD} />}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.cacheHits')}
            value={<AnimatedNumber value={d.requestsWithCacheHit} />}
            sub={t('pages:analytics.cacheRoi.requests')}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.readTokens')}
            value={<AnimatedNumber value={d.totalCacheReadTokens} format={formatTokens} />}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.creationTokens')}
            value={<AnimatedNumber value={d.totalCacheCreationTokens} format={formatTokens} />}
          />
          {readMultiplier !== null && (
            <SummaryCard
              label={t('pages:analytics.cacheRoi.readMultiplier')}
              value={<AnimatedNumber value={readMultiplier} precision={1} format={(n) => `${n.toFixed(1)}×`} />}
              sub={t('pages:analytics.cacheRoi.readMultiplierSub')}
              valueColor={readMultiplier >= 1 ? 'var(--color-success)' : undefined}
            />
          )}
          <SummaryCard
            label={t('pages:analytics.cacheRoi.stripCount')}
            value={<AnimatedNumber value={d.totalNormalisedStripCount} />}
            sub={`${formatTokens(d.totalNormalisedStripBytes)} B`}
          />
          <SummaryCard
            label={t('pages:analytics.cacheRoi.markersInjected')}
            value={<AnimatedNumber value={d.totalMarkersInjected} />}
          />
        </div>
      </SectionBlock>

      {/* Daily savings chart */}
      <Card>
        <h3 className={styles.cardHeading}>
          {t('pages:analytics.cacheRoi.dailyChart')}
        </h3>
        {daily.length === 0 ? (
          <div className={styles.noData}>
            {t('pages:analytics.cacheRoi.noData')}
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={280}>
            <LineChart data={daily} margin={{ top: 4, right: 16, bottom: 4, left: 0 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border-subtle)" />
              <XAxis dataKey="date" tick={{ fontSize: 'var(--g-font-size-xs)' }} />
              <YAxis tickFormatter={v => `$${Number(v).toFixed(3)}`} tick={{ fontSize: 'var(--g-font-size-xs)' }} />
              <RechartsTooltip
                contentStyle={tooltipStyle}
                formatter={(v, name) => [fmtUSD(Number(v ?? 0)), String(name)]}
              />
              <ReferenceLine y={0} stroke="var(--color-border)" strokeDasharray="2 2" />
              <Legend />
              <Line
                type="monotone"
                dataKey="gatewayCacheSavingsUsd"
                name={t('pages:analytics.cacheRoi.dailyGatewaySavings')}
                stroke={gatewayColor}
                dot={{ r: 3 }}
                strokeWidth={2}
              />
              <Line
                type="monotone"
                dataKey="cacheReadSavingsUsd"
                name={t('pages:analytics.cacheRoi.readSavings')}
                stroke={savingsColor}
                dot={{ r: 3 }}
                strokeWidth={2}
              />
              <Line
                type="monotone"
                dataKey="cacheWriteCostUsd"
                name={t('pages:analytics.cacheRoi.writeCost')}
                stroke={writeColor}
                dot={{ r: 3 }}
                strokeWidth={2}
              />
              <Line
                type="monotone"
                dataKey="cacheNetSavingsUsd"
                name={t('pages:analytics.cacheRoi.netSavings')}
                stroke={netColor}
                dot={{ r: 3 }}
                strokeWidth={2}
                strokeDasharray="4 2"
              />
              <Line
                type="monotone"
                dataKey="totalNetSavingsUsd"
                name={t('pages:analytics.cacheRoi.dailyTotalNetSavings')}
                stroke={totalNetColor}
                dot={{ r: 3 }}
                strokeWidth={2.5}
              />
            </LineChart>
          </ResponsiveContainer>
        )}
      </Card>

      {/* Adapter breakdown table */}
      {activeAdapters.length > 0 && (
        <Card>
          <h3 className={styles.cardHeading}>
            {t('pages:analytics.cacheRoi.byAdapter')}
          </h3>
          <div className={styles.tableWrapper}>
            <table className={styles.tableRoot}>
              <thead>
                {/* Group header row */}
                <tr>
                  <th rowSpan={2} className={styles.tableThGroupLeft}>
                    {t('pages:analytics.cacheRoi.colAdapter')}
                  </th>
                  <th colSpan={2} className={styles.tableThGroupSecondary}>
                    {t('pages:analytics.cacheRoi.colGroupTokens')}
                  </th>
                  <th colSpan={2} className={styles.tableThGroupAccent} style={{ color: gatewayColor, borderBottomColor: gatewayColor }}>
                    {t('pages:analytics.cacheRoi.colGroupGatewayCache')}
                  </th>
                  <th colSpan={6} className={styles.tableThGroupAccent} style={{ color: promptCacheColor, borderBottomColor: promptCacheColor }}>
                    {t('pages:analytics.cacheRoi.colGroupProviderPromptCache')}
                  </th>
                  <th rowSpan={2} className={styles.tableThSavingsRate}>
                    {t('pages:analytics.cacheRoi.colSavingsRate')}
                  </th>
                </tr>
                {/* Column name row */}
                <tr className={styles.tableHeaderRow}>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colInputTokens')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colOutputTokens')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colGatewayHits')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colGatewaySavings')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colNetSavings')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colReadSavings')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colWriteCost')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colCacheHits')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colReadTokens')}</th>
                  <th className={styles.tableThSecondary}>{t('pages:analytics.cacheRoi.colCreationTokens')}</th>
                </tr>
              </thead>
              <tbody>
                {activeAdapters.map(row => {
                  const rowGws = row.gatewayCacheSavingsUsd ?? 0;
                  const netSavings = rowGws + row.cacheNetSavingsUsd;
                  const grossCost = (row.estimatedCostUsd ?? 0) + netSavings;
                  const rate = grossCost > 0 ? (netSavings / grossCost) * 100 : null;
                  const hasGateway = row.gatewayCacheHitCount > 0 || rowGws > 0;
                  const hasPrompt = row.requestsWithCacheHit > 0 || row.cacheWriteCostUsd > 0;
                  return (
                    <tr key={row.adapter} className={styles.tableBodyRow}>
                      <td className={styles.tableCell}><Badge variant="info">{row.adapter}</Badge></td>
                      <td className={styles.tableCellMono}>{row.promptTokens > 0 ? formatTokens(row.promptTokens) : '—'}</td>
                      <td className={styles.tableCellMono}>{row.completionTokens > 0 ? formatTokens(row.completionTokens) : '—'}</td>
                      <td className={styles.tableCellMono} style={{ color: hasGateway ? gatewayColor : 'var(--color-text-muted)', fontWeight: hasGateway ? 600 : undefined }}>
                        {row.gatewayCacheHitCount > 0 ? row.gatewayCacheHitCount.toLocaleString() : '—'}
                      </td>
                      <td className={styles.tableCellMono} style={{ color: hasGateway ? 'var(--color-success)' : 'var(--color-text-muted)', fontWeight: hasGateway ? 600 : undefined }}>
                        {hasGateway ? fmtUSD(rowGws) : '—'}
                      </td>
                      <td className={styles.tableCellMono} style={{ color: netSavingsColor(row.cacheNetSavingsUsd), fontWeight: 'var(--g-font-weight-semibold)' }}>
                        {hasPrompt ? fmtUSD(row.cacheNetSavingsUsd) : '—'}
                      </td>
                      <td className={styles.tableCellMono} style={hasPrompt ? undefined : { color: 'var(--color-text-muted)' }}>
                        {hasPrompt ? fmtUSD(row.cacheReadSavingsUsd) : '—'}
                      </td>
                      <td className={styles.tableCellMono} style={hasPrompt ? undefined : { color: 'var(--color-text-muted)' }}>
                        {hasPrompt ? fmtUSD(row.cacheWriteCostUsd) : '—'}
                      </td>
                      <td className={styles.tableCellRight} style={hasPrompt ? undefined : { color: 'var(--color-text-muted)' }}>
                        {hasPrompt ? row.requestsWithCacheHit.toLocaleString() : '—'}
                      </td>
                      <td className={styles.tableCellRight} style={hasPrompt ? undefined : { color: 'var(--color-text-muted)' }}>
                        {hasPrompt ? formatTokens(row.cacheReadTokens) : '—'}
                      </td>
                      <td className={styles.tableCellRight} style={hasPrompt ? undefined : { color: 'var(--color-text-muted)' }}>
                        {hasPrompt ? formatTokens(row.cacheCreationTokens) : '—'}
                      </td>
                      <td
                        className={styles.tableCellMono}
                        style={{ color: rate != null ? (rate >= 100 ? 'var(--color-success)' : rate >= 50 ? 'var(--color-warning)' : 'var(--color-error)') : 'var(--color-text-muted)' }}
                        title={rate == null ? t('pages:analytics.cacheRoi.savingsRateNullTooltip') : undefined}
                      >
                        {rate != null ? `${rate.toFixed(0)}%` : '—'}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </Stack>
  );
}
