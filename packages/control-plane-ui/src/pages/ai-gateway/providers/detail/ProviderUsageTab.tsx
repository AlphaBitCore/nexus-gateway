import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { BarChart, Bar, XAxis, YAxis, Tooltip as RechartsTooltip, ResponsiveContainer, CartesianGrid, Legend } from 'recharts';
import { useTheme } from '@/theme/useTheme';
import { getSemanticColor, getTooltipStyle, getAxisTickStyle, getGridStroke } from '@nexus-gateway/ui-shared';
import { Button, Card, AnimatedNumber } from '@/components/ui';
import { LatencyMini } from '@/components/charts/LatencyMini';
import { formatUsd, formatTokens } from '@/lib/format';
import type { ProviderDetailState } from './useProviderDetail';
import styles from './ProviderDetail.module.css';

interface ProviderUsageTabProps {
  detail: ProviderDetailState;
}

export function ProviderUsageTab({ detail }: ProviderUsageTabProps) {
  const { t } = useTranslation();
  const { analyticsData, navigate } = detail;
  const { resolvedMode } = useTheme();
  const tooltipStyle = getTooltipStyle(resolvedMode);
  const tickStyle = getAxisTickStyle(resolvedMode);
  const gridStroke = getGridStroke(resolvedMode);

  if (!analyticsData) {
    return (
      <Card className={styles.emptyState}>
        {t('pages:providers.loadingUsageData')}
      </Card>
    );
  }

  const s = analyticsData.summary;
  const byProject = analyticsData.byProject ?? [];
  const byVirtualKey = analyticsData.byVirtualKey ?? [];

  return (
    <div>
      {/* Summary cards */}
      <div style={{ marginBottom: 'var(--g-space-2)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
        {t('pages:providers.cacheWindowNote30d')}
      </div>
      <div className={styles.summaryGrid}>
        <div className={styles.statCard}><div className={styles.statValue}><AnimatedNumber value={s.totalRequests} /></div><div className={styles.statLabel}>{t('pages:providers.totalRequests')}</div></div>
        <div className={styles.statCard}><div className={clsx(styles.statValue, s.errorRate > 0.05 ? styles.colorDanger : styles.colorSuccess)}><AnimatedNumber value={s.errorRate * 100} precision={1} format={(n) => `${n.toFixed(1)}%`} /></div><div className={styles.statLabel}>{t('pages:providers.errorRate')} ({s.errorCount})</div></div>
        <div className={styles.statCard}>
          {/* Phase-aware avg latency card. LatencyMini handles the
              "single value when backend only supplied avgLatencyMs" and
              the "Us + Upstream Total split + mini bar" cases internally,
              so the call site is a one-liner regardless. */}
          <LatencyMini
            size="card"
            title={t('pages:providers.avgLatency')}
            latencyMs={s.avgLatencyMs}
            upstreamTotalMs={(s as { avgUpstreamTotalMs?: number | null }).avgUpstreamTotalMs ?? null}
            upstreamTtfbMs={(s as { avgUpstreamTtfbMs?: number | null }).avgUpstreamTtfbMs ?? null}
          />
        </div>
        <div className={styles.statCard}><div className={styles.statValue}><AnimatedNumber value={s.totalTokens} format={formatTokens} /></div><div className={styles.statLabel}>{t('pages:providers.totalTokens')}</div></div>
        <div className={styles.statCard}><div className={styles.statValue}><AnimatedNumber value={s.totalEstimatedCostUsd} precision={2} format={formatUsd} /></div><div className={styles.statLabel}>{t('pages:providers.estimatedCost')}</div></div>
        <div className={styles.statCard}><div className={clsx(styles.statValue, s.cacheHitRate > 0 ? styles.colorSuccess : styles.colorMuted)}><AnimatedNumber value={s.cacheHitRate * 100} precision={1} format={(n) => `${n.toFixed(1)}%`} /></div><div className={styles.statLabel}>{t('pages:providers.cacheHitRate')} ({s.cacheHitCount})</div></div>
        <div className={styles.statCard}><div className={styles.statValue}><AnimatedNumber value={s.totalPromptTokens} format={formatTokens} /></div><div className={styles.statLabel}>{t('pages:providers.promptTokens')}</div></div>
        <div className={styles.statCard}><div className={styles.statValue}><AnimatedNumber value={s.totalCompletionTokens} format={formatTokens} /></div><div className={styles.statLabel}>{t('pages:providers.completionTokens')}</div></div>
      </div>

      {/* Daily requests chart */}
      <Card padding="lg">
        <div className={styles.sectionTitle}>{t('pages:providers.dailyRequests')}</div>
        {analyticsData.daily.length === 0 ? (
          <div className={styles.emptyState}>{t('pages:providers.noDataLast30Days')}</div>
        ) : (
          <ResponsiveContainer width="100%" height={280}>
            <BarChart data={analyticsData.daily} margin={{ bottom: 48, left: 8, right: 8 }}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="date" tick={tickStyle} interval={0} angle={-28} textAnchor="end" height={56} tickFormatter={(v: string) => v.slice(5)} />
              <YAxis tick={tickStyle} />
              <RechartsTooltip contentStyle={tooltipStyle} cursor={{ fill: 'transparent' }} />
              <Legend />
              <Bar dataKey="requests" name={t('pages:providers.chartRequests')} fill={getSemanticColor(resolvedMode, 'requests')} stackId="daily" />
              <Bar dataKey="errors" name={t('pages:providers.chartErrors')} fill={getSemanticColor(resolvedMode, 'errors')} stackId="daily" />
            </BarChart>
          </ResponsiveContainer>
        )}
      </Card>

      {/* Daily cost chart */}
      {analyticsData.daily.some(d => (d as { estimatedCostUsd?: number }).estimatedCostUsd != null) && (
        <Card padding="lg">
          <div className={styles.sectionTitle}>{t('pages:providers.dailyCost')}</div>
          <ResponsiveContainer width="100%" height={200}>
            <BarChart data={analyticsData.daily} margin={{ bottom: 48, left: 8, right: 8 }}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="date" tick={tickStyle} interval={0} angle={-28} textAnchor="end" height={56} tickFormatter={(v: string) => v.slice(5)} />
              <YAxis tick={tickStyle} tickFormatter={(v: number) => `$${v.toFixed(3)}`} />
              <RechartsTooltip
                contentStyle={tooltipStyle}
                cursor={{ fill: 'transparent' }}
                formatter={(v: unknown) => [`$${Number(v).toFixed(4)}`, t('pages:providers.chartCost')]}
              />
              <Bar dataKey="estimatedCostUsd" name={t('pages:providers.chartCost')} fill={getSemanticColor(resolvedMode, 'cost')} />
            </BarChart>
          </ResponsiveContainer>
        </Card>
      )}

      {/* Project breakdown */}
      <Card>
        <div className={styles.sectionTitle}>{t('pages:providers.usageByProject')}</div>
        {byProject.length === 0 ? (
          <div className={styles.emptyState}>{t('pages:providers.noProjectUsage')}</div>
        ) : (
          <div className={styles.overflowAuto}>
            <table className={styles.table}>
              <thead><tr>{[t('pages:providers.projectTableProject'), t('pages:providers.projectTableRequests'), t('pages:providers.projectTableAvgLatency'), t('pages:providers.projectTablePromptTokens'), t('pages:providers.projectTableCompletionTokens'), t('pages:providers.projectTableTotalTokens'), t('pages:providers.projectTableCost'), ''].map(h => <th key={h || 'open'} className={styles.th}>{h}</th>)}</tr></thead>
              <tbody>
                {byProject.map(p => (
                  <tr key={p.projectId} className={styles.tableRow}>
                    <td className={styles.td}>
                      <div className={styles.fontBold}>{p.projectName ?? p.projectCode ?? t('pages:providers.unknownProject')}</div>
                      {p.projectCode && p.projectName && <div className={styles.monoMuted}>{p.projectCode}</div>}
                      <div className={styles.monoMuted}>{p.projectId}</div>
                    </td>
                    <td className={styles.tdMono}>{p.requestCount.toLocaleString()}</td>
                    <td className={styles.tdMono}>
                      <LatencyMini
                        size="row"
                        latencyMs={p.avgLatencyMs}
                        upstreamTotalMs={(p as { avgUpstreamTotalMs?: number | null }).avgUpstreamTotalMs ?? null}
                        upstreamTtfbMs={(p as { avgUpstreamTtfbMs?: number | null }).avgUpstreamTtfbMs ?? null}
                      />
                    </td>
                    <td className={styles.tdMono}>{formatTokens(p.promptTokens)}</td>
                    <td className={styles.tdMono}>{formatTokens(p.completionTokens)}</td>
                    <td className={styles.tdMono}>{formatTokens(p.totalTokens)}</td>
                    <td className={styles.tdMono}>{formatUsd(p.estimatedCostUsd)}</td>
                    <td className={styles.td}><Button variant="secondary" size="sm" onClick={() => navigate(`/iam/projects/${p.projectId}`)}>{t('pages:providers.open')}</Button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {/* Virtual key breakdown */}
      <Card>
        <div className={styles.sectionTitle}>{t('pages:providers.usageByVirtualKey')}</div>
        {byVirtualKey.length === 0 ? (
          <div className={styles.emptyState}>{t('pages:providers.noVirtualKeyUsage')}</div>
        ) : (
          <div className={styles.overflowAuto}>
            <table className={styles.table}>
              <thead><tr>{[t('pages:providers.virtualKeyTableVirtualKey'), t('pages:providers.virtualKeyTableRequests'), t('pages:providers.virtualKeyTableAvgLatency'), t('pages:providers.virtualKeyTablePromptTokens'), t('pages:providers.virtualKeyTableCompletionTokens'), t('pages:providers.virtualKeyTableTotalTokens'), t('pages:providers.virtualKeyTableCost'), ''].map(h => <th key={h || 'open-vk'} className={styles.th}>{h}</th>)}</tr></thead>
              <tbody>
                {byVirtualKey.map(v => {
                  const vkPathSegment = encodeURIComponent(v.name ?? v.virtualKeyId);
                  return (
                    <tr key={v.virtualKeyId} className={styles.tableRow}>
                      <td className={styles.td}>
                        <div className={styles.vkName}>{v.name ?? v.keyPrefix ?? '\u2014'}</div>
                        {v.keyPrefix && v.name && <div className={styles.monoMuted}>{v.keyPrefix}</div>}
                        <div className={styles.monoMuted}>{v.virtualKeyId}</div>
                      </td>
                      <td className={styles.tdMono}>{v.requestCount.toLocaleString()}</td>
                      <td className={styles.tdMono}>
                        <LatencyMini
                          size="row"
                          latencyMs={v.avgLatencyMs}
                          upstreamTotalMs={(v as { avgUpstreamTotalMs?: number | null }).avgUpstreamTotalMs ?? null}
                          upstreamTtfbMs={(v as { avgUpstreamTtfbMs?: number | null }).avgUpstreamTtfbMs ?? null}
                        />
                      </td>
                      <td className={styles.tdMono}>{formatTokens(v.promptTokens)}</td>
                      <td className={styles.tdMono}>{formatTokens(v.completionTokens)}</td>
                      <td className={styles.tdMono}>{formatTokens(v.totalTokens)}</td>
                      <td className={styles.tdMono}>{formatUsd(v.estimatedCostUsd)}</td>
                      <td className={styles.td}><Button variant="secondary" size="sm" onClick={() => navigate(`/ai-gateway/virtual-keys/${vkPathSegment}`)}>{t('pages:providers.open')}</Button></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {/* Model breakdown */}
      {analyticsData.byModel.length > 0 && (
        <Card>
          <div className={styles.sectionTitle}>{t('pages:providers.usageByModel')}</div>
          <div className={styles.overflowAuto}>
            <table className={styles.table}>
              <thead><tr>{[t('pages:providers.modelUsageTableModel'), t('pages:providers.modelUsageTableRequests'), t('pages:providers.modelUsageTableAvgLatency'), t('pages:providers.modelUsageTablePromptTokens'), t('pages:providers.modelUsageTableCompletionTokens'), t('pages:providers.modelUsageTableTotalTokens'), t('pages:providers.modelUsageTableCost')].map(h => <th key={h} className={styles.th}>{h}</th>)}</tr></thead>
              <tbody>
                {analyticsData.byModel.map(m => (
                  <tr key={m.model} className={styles.tableRow}>
                    <td className={clsx(styles.tdMonoSm, styles.fontBold)}>{m.model}</td>
                    <td className={styles.tdMono}>{m.requestCount.toLocaleString()}</td>
                    <td className={styles.tdMono}>
                      <LatencyMini
                        size="row"
                        latencyMs={m.avgLatencyMs}
                        upstreamTotalMs={(m as { avgUpstreamTotalMs?: number | null }).avgUpstreamTotalMs ?? null}
                        upstreamTtfbMs={(m as { avgUpstreamTtfbMs?: number | null }).avgUpstreamTtfbMs ?? null}
                      />
                    </td>
                    <td className={styles.tdMono}>{formatTokens(m.promptTokens)}</td>
                    <td className={styles.tdMono}>{formatTokens(m.completionTokens)}</td>
                    <td className={styles.tdMono}>{formatTokens(m.totalTokens)}</td>
                    <td className={styles.tdMono}>{formatUsd(m.estimatedCostUsd)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {/* Status code distribution */}
      {analyticsData.byStatus.length > 0 && (
        <Card>
          <div className={styles.sectionTitle}>{t('pages:providers.statusCodeDistribution')}</div>
          <div className={styles.flexWrap}>
            {analyticsData.byStatus.map(st => {
              const isErr = st.statusCode >= 400;
              return (
                <div key={st.statusCode} className={styles.statusCodeCard}>
                  <div className={clsx(styles.statValueSm, isErr ? styles.colorDanger : styles.colorSuccess)}>
                    <AnimatedNumber value={st.count} />
                  </div>
                  <div className={styles.statLabelMono}>{st.statusCode}</div>
                </div>
              );
            })}
          </div>
        </Card>
      )}
    </div>
  );
}
