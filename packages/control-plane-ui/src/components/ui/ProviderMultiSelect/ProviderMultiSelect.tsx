import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { MultiSelectDropdown } from '@/components/ui/MultiSelectDropdown';
import type { AdminModelsByProvider } from '@/api/types';

export interface ProviderMultiSelectProps {
  label: string;
  value: string[];
  onChange: (next: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  emptyLabel?: string;
  disabled?: boolean;
  /** Toggle inline search input (on by default). */
  searchable?: boolean;
  className?: string;
}

/**
 * Multi-select of provider ids. Data is passed in by the parent (no
 * duplicate fetch); options filter to enabled providers only.
 */
export function ProviderMultiSelect({
  label,
  value,
  onChange,
  providerGroups,
  emptyLabel,
  disabled,
  searchable = true,
  className,
}: ProviderMultiSelectProps) {
  const { t } = useTranslation();

  const options = useMemo(
    () =>
      providerGroups
        .filter((g) => g.provider?.enabled)
        .map((g) => ({
          value: g.provider.id,
          label: g.provider.displayName ?? g.provider.name,
        })),
    [providerGroups],
  );

  return (
    <MultiSelectDropdown
      label={label}
      options={options}
      value={value}
      onChange={onChange}
      disabled={disabled}
      emptyLabel={emptyLabel ?? t('common:selectProviders')}
      searchable={searchable}
      searchPlaceholder={t('common:searchProviders')}
      className={className}
    />
  );
}
