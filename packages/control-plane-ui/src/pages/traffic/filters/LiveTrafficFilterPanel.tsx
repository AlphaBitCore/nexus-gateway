import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui';
import {
  type LiveTrafficFiltersState,
  type TrafficSourceFilter,
  countLiveTrafficFilters,
} from '../filters/liveTrafficFilters';
import { LiveTrafficBasicFilters } from '../filters/LiveTrafficBasicFilters';
import { LiveTrafficAdvancedFilters } from '../filters/LiveTrafficAdvancedFilters';
import css from './LiveTrafficFilterPanel.module.css';

/** Shared compact field wrapper used by both basic and advanced filter sub-components. */
export function FieldCompact({
  label,
  tip,
  children,
}: {
  label: string;
  tip?: string;
  children: ReactNode;
}) {
  return (
    <div className={css.fieldCompact}>
      <span className={css.fieldCompactLabel} title={tip}>
        {label}
      </span>
      {children}
    </div>
  );
}

interface LiveTrafficFilterPanelProps {
  value: LiveTrafficFiltersState;
  onPatch: (patch: Partial<LiveTrafficFiltersState>) => void;
  onApply: () => void;
  onClear: () => void;
  applyDisabled?: boolean;
  source: TrafficSourceFilter;
  showAdvanced?: boolean;
  onToggleAdvanced?: () => void;
  onRefresh?: () => void;
  refreshing?: boolean;
}

export function LiveTrafficFilterPanel({
  value: v,
  onPatch,
  onApply,
  onClear,
  applyDisabled = false,
  source,
  showAdvanced: showAdvancedProp,
  onToggleAdvanced,
  onRefresh,
  refreshing = false,
}: LiveTrafficFilterPanelProps) {
  const { t } = useTranslation();
  const [showAdvancedLocal, setShowAdvancedLocal] = useState(false);
  const showAdvanced = showAdvancedProp ?? showAdvancedLocal;
  const toggleAdvanced = onToggleAdvanced ?? (() => setShowAdvancedLocal((s) => !s));
  const hasDraftFilters = countLiveTrafficFilters(v) > 0;

  return (
    <div className={css.panelCard}>
      <LiveTrafficBasicFilters value={v} onPatch={onPatch} source={source} />

      {source !== '' && showAdvanced ? (
        <LiveTrafficAdvancedFilters value={v} onPatch={onPatch} source={source} />
      ) : null}

      <div className={css.actionBar}>
        <Button variant="primary" size="sm" disabled={applyDisabled} onClick={onApply}>
          {t('pages:traffic.applyFilters')}
        </Button>
        {onRefresh && (
          <Button variant="secondary" size="sm" onClick={onRefresh} disabled={refreshing} loading={refreshing}>
            {refreshing ? t('pages:traffic.refreshing') : t('pages:traffic.refresh')}
          </Button>
        )}
        {source !== '' && (
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={showAdvanced}
            onClick={toggleAdvanced}
          >
            {showAdvanced ? t('pages:traffic.advancedCollapse') : t('pages:traffic.advancedExpand')}
          </Button>
        )}
        {hasDraftFilters ? (
          <Button variant="ghost" size="sm" aria-label={t('pages:traffic.clearFilters')} onClick={onClear}>
            {t('pages:traffic.clearFilters')}
          </Button>
        ) : null}
      </div>
    </div>
  );
}
