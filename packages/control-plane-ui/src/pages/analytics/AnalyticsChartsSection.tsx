import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui';
import { getSemanticColor, type ChartColorMode } from '@nexus-gateway/ui-shared';
import type { CostData, UsageData } from '../../api/types';
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, PieChart, Pie, Cell } from 'recharts';
import styles from './AnalyticsPage.module.css';
import { formatTokens } from '@/lib/format';
import { topNWithOther, PIE_SLICE_CAP } from '@/lib/chartData';

interface AnalyticsChartsSectionProps {
  groupBy: string;
  costData: { data: CostData[] } | null;
  usageData: { data: UsageData[] } | null;
  pieColors: readonly string[];
  tooltipStyle: Record<string, string | number>;
  resolvedMode: ChartColorMode;
}

export function AnalyticsChartsSection({
  groupBy,
  costData,
  usageData,
  pieColors,
  tooltipStyle,
  resolvedMode,
}: AnalyticsChartsSectionProps) {
  const { t } = useTranslation();

  return (
    <div className={styles.costUsageSection}>
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:traffic.sectionCostUsage')}</h2>
        <p className={styles.sectionSubtitle}>{t('pages:traffic.sectionCostUsageSubtitle', { groupBy })}</p>
      </div>

      <div className={styles.chartsGrid}>
        <Card className={styles.chartCard}>
          <h3 className={styles.chartTitle}>{t('pages:traffic.chartCostBy', { axis: groupBy })}</h3>
          {costData?.data && costData.data.length > 0 ? (() => {
            // Pre-compute percentages from the totalCostUsd column so
            // every slice carries its own pre-rounded share. We round
            // each slice down to 1 decimal then assign the residual to
            // the largest slice — guarantees the rendered numbers sum
            // to exactly 100.0 even in the face of rounding.
            const enrichedAll = costData.data.map((d) => ({
              ...d,
              displayGroup: d.groupLabel || d.group,
              cost: d.totalCostUsd ?? 0,
            }));
            // Cap the long-tail at top-N + "Other" so pies stay
            // readable when groupBy = model / user / project blows
            // out to 20+ categories.
            const enriched = topNWithOther(
              enrichedAll,
              PIE_SLICE_CAP,
              (r) => r.cost,
              (totalCost, droppedCount) => ({
                ...enrichedAll[0],
                group: '__other__',
                displayGroup: `${t('common:other')} (${droppedCount})`,
                groupLabel: t('common:other'),
                cost: totalCost,
                totalCostUsd: totalCost,
              }),
            );
            const total = enriched.reduce((s, d) => s + d.cost, 0);
            type PieRow = (typeof enriched)[number] & { percent: number };
            const withPct: PieRow[] = total > 0
              ? enriched.map((d) => ({ ...d, percent: Math.floor((d.cost / total) * 1000) / 10 }))
              : enriched.map((d) => ({ ...d, percent: 0 }));
            if (total > 0 && withPct.length > 0) {
              const drift = +(100 - withPct.reduce((s, d) => s + d.percent, 0)).toFixed(1);
              if (drift !== 0) {
                let largestIdx = 0;
                for (let i = 1; i < withPct.length; i++) {
                  if (withPct[i].cost > withPct[largestIdx].cost) largestIdx = i;
                }
                withPct[largestIdx].percent = +(withPct[largestIdx].percent + drift).toFixed(1);
              }
            }
            return (
              <ResponsiveContainer width="100%" height={300}>
                <PieChart>
                  <Pie
                    data={withPct}
                    dataKey="cost"
                    nameKey="displayGroup"
                    cx="50%" cy="50%" outerRadius={90}
                    label={(props) => {
                      const p = props as { displayGroup?: string; percent?: number };
                      const pct = typeof p.percent === 'number' ? p.percent.toFixed(1) : '0.0';
                      return `${p.displayGroup ?? ''} ${pct}%`;
                    }}
                  >
                    {withPct.map((_, i) => <Cell key={i} fill={pieColors[i % pieColors.length]} />)}
                  </Pie>
                  <Tooltip
                    contentStyle={tooltipStyle}
                    cursor={{ fill: 'transparent' }}
                    formatter={(value, _name, item) => {
                      const num = typeof value === 'number' ? value : Number(value ?? 0);
                      const pct = (item?.payload as { percent?: number } | undefined)?.percent;
                      return [`$${num.toFixed(4)} (${(pct ?? 0).toFixed(1)}%)`, _name as string];
                    }}
                  />
                </PieChart>
              </ResponsiveContainer>
            );
          })() : (
            <div className={styles.emptyChart}>{t('pages:traffic.noDataForPeriod')}</div>
          )}
        </Card>

        <Card className={styles.chartCard}>
          <h3 className={styles.chartTitle}>{t('pages:traffic.chartTokenUsageBy', { axis: groupBy })}</h3>
          {usageData?.data && usageData.data.length > 0 ? (
            <ResponsiveContainer width="100%" height={300}>
              <BarChart data={usageData.data.map((d) => ({ ...d, displayGroup: d.groupLabel || d.group }))}>
                <XAxis dataKey="displayGroup" tick={{ fontSize: 12 }} />
                <YAxis tick={{ fontSize: 12 }} tickFormatter={(v) => formatTokens(Number(v))} />
                <Tooltip
                  contentStyle={tooltipStyle}
                  cursor={{ fill: 'transparent' }}
                  formatter={(value, name) => [formatTokens(Number(value)), name as string]}
                />
                <Bar dataKey="totalPromptTokens" name={t('pages:traffic.chartPrompt')} fill={getSemanticColor(resolvedMode, 'prompt')} stackId="tokens" />
                <Bar dataKey="totalCompletionTokens" name={t('pages:traffic.chartCompletion')} fill={getSemanticColor(resolvedMode, 'completion')} stackId="tokens" />
              </BarChart>
            </ResponsiveContainer>
          ) : (
            <div className={styles.emptyChart}>{t('pages:traffic.noDataForPeriod')}</div>
          )}
        </Card>
      </div>
    </div>
  );
}
