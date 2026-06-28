import clsx from 'clsx';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Tooltip } from '@/components/ui';
import { MultiSelectDropdown } from '@/components/ui/MultiSelectDropdown';
import type { AdminModelsByProvider } from '@/api/types';
import styles from './RoutingRuleDetail.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

/* ── KvRow ──────────────────────────────────────────────────────── */

export function KvRow({
  label,
  helpTitle,
  helpBody,
  children,
}: {
  label: string;
  helpTitle?: string;
  helpBody?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div>
      <div className={styles.kvLabelRow}>
        <div className={styles.kvLabelInline}>{label}</div>
        {helpTitle != null && helpBody != null ? (
          <Tooltip content={helpBody}>
            <HelpIconButton aria-label={`Help: ${helpTitle}`} />
          </Tooltip>
        ) : null}
      </div>
      <div className={styles.kvValue}>{children}</div>
    </div>
  );
}

/* ── MatchModelSelector ─────────────────────────────────────────── */

/** Searchable multi-select for Match Conditions models. Options are grouped
 *  by provider; models already referenced elsewhere (primary/fallback
 *  targets) are filtered out via `excludeModels`. */
export function MatchModelSelector({
  selected,
  onChange,
  providerGroups,
  excludeModels,
  className,
}: {
  selected: string[];
  onChange: (v: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  excludeModels: Set<string>;
  className?: string;
}) {
  const { t } = useTranslation();

  const options = useMemo(() => {
    const out: { value: string; label: string; group: string }[] = [];
    for (const g of providerGroups) {
      const providerLabel = g.provider?.displayName?.trim() || g.provider?.name || '';
      for (const m of g.models ?? []) {
        if (excludeModels.has(m.id) && !selected.includes(m.id)) continue;
        out.push({
          value: m.id,
          label: `${m.name} (${m.providerModelId})`,
          group: providerLabel,
        });
      }
    }
    return out;
  }, [providerGroups, excludeModels, selected]);

  return (
    <MultiSelectDropdown
      label=""
      options={options}
      value={selected}
      onChange={onChange}
      emptyLabel={t('pages:routing.addModelToMatch')}
      searchable
      searchPlaceholder={t('common:searchModels')}
      className={className}
    />
  );
}

/* ── ProviderModelSelect ────────────────────────────────────────── */

/** Provider+Model cascading selector */
export function ProviderModelSelect({
  providerValue,
  modelValue,
  onProviderChange,
  onModelChange,
  providerGroups,
  style,
  className,
}: {
  providerValue: string;
  modelValue: string;
  onProviderChange: (v: string) => void;
  onModelChange: (v: string) => void;
  providerGroups: AdminModelsByProvider[];
  style?: React.CSSProperties;
  className?: string;
}) {
  const { t } = useTranslation();
  const modelsForProvider = providerGroups.find(g => g.provider?.name === providerValue)?.models ?? [];

  return (
    <div className={clsx(styles.providerModelRow, className)} style={style}>
      <select
        value={providerValue}
        onChange={e => {
          onProviderChange(e.target.value);
          onModelChange('');
        }}
        className={clsx(styles.nativeSelect, styles.flexGrow1)}
      >
        <option value="">{t('pages:routing.selectProvider')}</option>
        {providerGroups.map(g => (
          <option key={g.provider?.id} value={g.provider?.name}>
            {g.provider?.displayName?.trim() || g.provider?.name} ({g?.models?.length})
          </option>
        ))}
      </select>
      <select
        value={modelValue}
        onChange={e => onModelChange(e.target.value)}
        className={clsx(styles.nativeSelect, styles.flexGrow1)}
        disabled={!providerValue}
      >
        <option value="">{providerValue ? t('pages:routing.selectModel') : t('pages:routing.selectProviderFirst')}</option>
        {modelsForProvider.map(m => (
          <option key={m.id} value={m.providerModelId}>
            {m.name} ({m.providerModelId})
          </option>
        ))}
      </select>
    </div>
  );
}

/* ── formatProviderMatchLine ────────────────────────────────────── */

export function formatProviderMatchLine(groups: AdminModelsByProvider[], ids: string[]): string {
  if (ids.length === 0) return '—';
  return ids
    .map((pid) => {
      const g = groups.find((x) => x.provider?.id === pid);
      return g ? (g.provider?.displayName?.trim() || g.provider?.name) : pid;
    })
    .join(', ');
}
