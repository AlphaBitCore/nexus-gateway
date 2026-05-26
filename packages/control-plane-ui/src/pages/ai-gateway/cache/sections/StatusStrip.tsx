/**
 * StatusStrip — sticky top-of-page summary for the Cache admin page.
 *
 * Most admins land here to *check state*, not edit. This strip surfaces the
 * three things that change frequently:
 *   - Gateway Cache state: hit count + savings ($)
 *   - Provider Prompt Cache state: hit count + savings ($)
 *   - Freshness rules: active count
 *
 * Plus an emergency-disable dropdown (multi-option):
 *   - Disable semantic cache fleet-wide
 *   - Disable extract cache fleet-wide
 *   - Disable all gateway cache fleet-wide (combo)
 *
 * Each option opens a confirmation dialog before firing the mutation.
 * The dropdown is hidden when neither cache layer is currently enabled.
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import {
  AlertDialog,
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { analyticsApi, type CacheROISummary } from '@/api/services/overview/analytics';
import { timeSensitivePatternsApi } from '@/api/services/cache/timeSensitivePatterns';
import { extractCacheConfigApi, type ExtractCacheConfig } from '@/api/services/cache/extractCacheConfig';
import { useDisableSemanticCacheFleetWide } from '../hooks/useDisableSemanticCacheFleetWide';
import { useDisableExtractCacheFleetWide } from '../hooks/useDisableExtractCacheFleetWide';
import styles from './StatusStrip.module.css';

interface Props {
  /** Current fleet-wide semantic cache enabled state. */
  semanticEnabled: boolean;
  /** Whether the current admin has permission to flip the emergency switches. */
  canDisable: boolean;
}

type DisableAction = 'semantic' | 'extract' | 'all';

function fmtUSD(n: number): string {
  if (n === 0) return '$0.00';
  // Sub-cent savings (rollup oddity or genuinely tiny test traffic) render
  // as "<$0.01" so admins see "essentially nothing" without false zero.
  if (n > 0 && n < 0.01) return '<$0.01';
  if (n < 0 && n > -0.01) return '>−$0.01';
  return '$' + n.toFixed(2);
}

export function StatusStrip({ semanticEnabled, canDisable }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const [pendingAction, setPendingAction] = useState<DisableAction | null>(null);

  const { data: roi } = useApi<CacheROISummary>(
    () => analyticsApi.cacheROI(),
    ['admin', 'analytics', 'cache-roi', 'status-strip'],
  );

  const { data: patternsData } = useApi(
    () => timeSensitivePatternsApi.list(),
    ['admin', 'cache', 'time-sensitive-patterns'],
  );

  const { data: extractCfg } = useApi<ExtractCacheConfig>(
    () => extractCacheConfigApi.getConfig(),
    ['admin', 'extract-cache', 'config'],
  );

  const { disable: disableSemantic, loading: disablingSemantic } =
    useDisableSemanticCacheFleetWide({
      successMessage: t('pages:aiGateway.cache.statusStrip.disableSemanticSuccess'),
      errorMessage: t('pages:aiGateway.cache.statusStrip.disableSemanticError'),
    });
  const { disable: disableExtract, loading: disablingExtract } =
    useDisableExtractCacheFleetWide({
      successMessage: t('pages:aiGateway.cache.statusStrip.disableExtractSuccess'),
      errorMessage: t('pages:aiGateway.cache.statusStrip.disableExtractError'),
    });

  const gatewaySavings = roi?.totalGatewayCacheSavingsUsd ?? null;
  const gatewayHits = roi?.gatewayCacheHitCount ?? null;
  const providerSavings = roi?.totalCacheNetSavingsUsd ?? null;
  const providerHits = roi?.requestsWithCacheHit ?? null;
  const freshnessActive = (patternsData?.patterns ?? []).filter((p) => p.enabled).length;
  const freshnessTotal = patternsData?.patterns.length ?? 0;
  const extractEnabled = extractCfg?.enabled ?? false;
  const anyEnabled = semanticEnabled || extractEnabled;
  const disabling = disablingSemantic || disablingExtract;

  const handleConfirm = async () => {
    const action = pendingAction;
    setPendingAction(null);
    if (!action) return;
    try {
      if (action === 'semantic') {
        await disableSemantic();
      } else if (action === 'extract') {
        await disableExtract();
      } else {
        // 'all' = both. Run in parallel; failures surface via per-mutation toasts.
        await Promise.all([
          semanticEnabled ? disableSemantic() : Promise.resolve(),
          extractEnabled ? disableExtract() : Promise.resolve(),
        ]);
        addToast(t('pages:aiGateway.cache.statusStrip.disableAllSuccess'), 'success');
      }
    } catch {
      // useMutation already surfaces errors via toast options above.
    }
  };

  const confirmTitleKey: Record<DisableAction, string> = {
    semantic: 'pages:aiGateway.cache.statusStrip.confirmSemanticTitle',
    extract: 'pages:aiGateway.cache.statusStrip.confirmExtractTitle',
    all: 'pages:aiGateway.cache.statusStrip.confirmAllTitle',
  };
  const confirmDescKey: Record<DisableAction, string> = {
    semantic: 'pages:aiGateway.cache.statusStrip.confirmSemanticDescription',
    extract: 'pages:aiGateway.cache.statusStrip.confirmExtractDescription',
    all: 'pages:aiGateway.cache.statusStrip.confirmAllDescription',
  };

  return (
    <div className={styles.strip} role="status" aria-label={t('pages:aiGateway.cache.statusStrip.ariaLabel')}>
      <div className={styles.cell}>
        <span className={styles.cellLabel}>{t('pages:aiGateway.cache.statusStrip.gateway')}</span>
        <span className={styles.cellValue}>
          {gatewaySavings !== null
            ? t('pages:aiGateway.cache.statusStrip.savedAmount', { amount: fmtUSD(gatewaySavings) })
            : '—'}
        </span>
        {gatewayHits !== null && (
          <span className={styles.cellSub}>
            {t('pages:aiGateway.cache.statusStrip.hits', { count: gatewayHits })}
          </span>
        )}
      </div>

      <div className={styles.cell}>
        <span className={styles.cellLabel}>{t('pages:aiGateway.cache.statusStrip.provider')}</span>
        <span className={styles.cellValue}>
          {providerSavings !== null
            ? t('pages:aiGateway.cache.statusStrip.savedAmount', { amount: fmtUSD(providerSavings) })
            : '—'}
        </span>
        {providerHits !== null && (
          <span className={styles.cellSub}>
            {t('pages:aiGateway.cache.statusStrip.hits', { count: providerHits })}
          </span>
        )}
      </div>

      <div className={styles.cell}>
        <span className={styles.cellLabel}>{t('pages:aiGateway.cache.statusStrip.freshness')}</span>
        <span className={styles.cellValue}>
          {freshnessTotal > 0
            ? t('pages:aiGateway.cache.statusStrip.activeOfTotal', { active: freshnessActive, total: freshnessTotal })
            : '—'}
        </span>
      </div>

      <div className={styles.spacer} />

      {anyEnabled && (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="danger" size="sm" disabled={!canDisable || disabling}>
              {t('pages:aiGateway.cache.statusStrip.disableMenuTrigger')}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {semanticEnabled && (
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  setPendingAction('semantic');
                }}
              >
                {t('pages:aiGateway.cache.statusStrip.disableSemanticItem')}
              </DropdownMenuItem>
            )}
            {extractEnabled && (
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  setPendingAction('extract');
                }}
              >
                {t('pages:aiGateway.cache.statusStrip.disableExtractItem')}
              </DropdownMenuItem>
            )}
            {semanticEnabled && extractEnabled && (
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  setPendingAction('all');
                }}
              >
                {t('pages:aiGateway.cache.statusStrip.disableAllItem')}
              </DropdownMenuItem>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      )}

      <AlertDialog
        open={pendingAction !== null}
        onOpenChange={(open) => { if (!open) setPendingAction(null); }}
        title={pendingAction ? t(confirmTitleKey[pendingAction]) : ''}
        description={pendingAction ? t(confirmDescKey[pendingAction]) : ''}
        confirmLabel={t('pages:aiGateway.cache.statusStrip.confirmButton')}
        onConfirm={() => void handleConfirm()}
        variant="danger"
        loading={disabling}
      />
    </div>
  );
}
