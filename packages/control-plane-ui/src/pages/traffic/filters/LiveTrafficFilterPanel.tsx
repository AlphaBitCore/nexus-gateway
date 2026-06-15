import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Input } from '@/components/ui';
import {
  type LiveTrafficFiltersState,
  type TrafficSourceFilter,
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
}: LiveTrafficFilterPanelProps) {
  const { t } = useTranslation();
  const shellRef = useRef<HTMLDivElement>(null);
  const [showAdvancedLocal, setShowAdvancedLocal] = useState(false);
  const showAdvanced = showAdvancedProp ?? showAdvancedLocal;
  const toggleAdvanced = onToggleAdvanced ?? (() => setShowAdvancedLocal((s) => !s));
  const closeAdvanced = onToggleAdvanced ?? (() => setShowAdvancedLocal(false));
  const advancedFilterLabel = (showAdvanced
    ? t('pages:traffic.advancedCollapse')
    : t('pages:traffic.advancedExpand')
  ).replace(/[▲▼▴▾]/g, '').trim();
  const handleConfirmSearch = () => {
    onApply();
    closeAdvanced();
  };

  useEffect(() => {
    if (!showAdvanced) return;

    const handlePointerDown = (event: PointerEvent) => {
      if (shellRef.current?.contains(event.target as Node)) return;
      closeAdvanced();
    };

    document.addEventListener('pointerdown', handlePointerDown);
    return () => document.removeEventListener('pointerdown', handlePointerDown);
  }, [closeAdvanced, showAdvanced]);

  return (
    <div className={css.filterShell} ref={shellRef}>
      <div className={css.searchBar}>
        <span className={css.searchIcon} aria-hidden="true" />
        <Input
          type="search"
          aria-label={t('pages:traffic.labelTargetHost')}
          placeholder="Search..."
          value={v.targetHost}
          onChange={(e) => onPatch({ targetHost: e.target.value })}
          onKeyDown={(e) => {
            if (e.key === 'Enter') onApply();
          }}
          className={css.searchInput}
        />
        <button
          type="button"
          className={css.advancedFilterButton}
          aria-expanded={showAdvanced}
          onClick={toggleAdvanced}
        >
          {advancedFilterLabel}
        </button>
      </div>

      {showAdvanced ? (
        <div className={css.advancedDropdown}>
          <LiveTrafficBasicFilters value={v} onPatch={onPatch} source={source} />

          {source !== '' ? (
            <LiveTrafficAdvancedFilters value={v} onPatch={onPatch} source={source} />
          ) : null}

          <div className={css.actionBar}>
            <Button variant="secondary" size="sm" aria-label={t('pages:traffic.clearFilters')} onClick={onClear} className={css.resetButton}>
              Reset
            </Button>
            <Button variant="primary" size="sm" disabled={applyDisabled} onClick={handleConfirmSearch} className={css.confirmButton}>
              Confirm Search
            </Button>
          </div>
        </div>
      ) : (
        <div className={css.collapsedActions}>
          <Button variant="primary" size="sm" disabled={applyDisabled} onClick={onApply}>
            {t('pages:traffic.applyFilters')}
          </Button>
        </div>
      )}
    </div>
  );
}
