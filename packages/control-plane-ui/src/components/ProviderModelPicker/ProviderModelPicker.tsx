import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Select, FormField, Stack } from '@/components/ui';
import type { AdminModelsByProvider } from '@/api/types';

/**
 * Cascading Provider → Model selector keyed by UUID identifiers.
 *
 * Identity contract: `providerId` is the Provider row UUID; `modelId`
 * is the Model row UUID. Switching provider clears modelId so the
 * (provider, model) pair is always valid by construction.
 *
 * NOT the right component for the routing-rule editor — that surface
 * stores provider names + wire model IDs for portability across DBs.
 * See `StrategyConfigSection.tsx`'s local `ProviderModelSelect` instead.
 *
 * Presentational only: caller fetches the grouped model list and passes it
 * through. Optional `endpointType` filter narrows both dropdowns to a
 * specific model type (e.g. "embedding" for the semantic cache picker).
 */
export interface ProviderModelPickerProps {
  /** Grouped provider/model list. Caller fetches; the picker filters. */
  providerGroups: AdminModelsByProvider[];
  /** Current Provider UUID. `null` when nothing is selected. */
  providerId: string | null;
  /** Current Model UUID. `null` when nothing is selected. */
  modelId: string | null;
  /**
   * Fires on either provider or model change. Picker auto-clears
   * modelId when providerId changes so the (provider, model) pair
   * is always valid by construction.
   */
  onChange: (next: { providerId: string | null; modelId: string | null }) => void;
  /** Disables both selects. */
  disabled?: boolean;
  /**
   * Optional `Model.type` filter. When set, the Provider dropdown lists only
   * providers that own at least one model of this type, and the Model
   * dropdown is narrowed to that same type.
   */
  endpointType?: string;
  /** Override the Provider field label. Defaults to i18n "Provider". */
  providerLabel?: string;
  /** Override the Model field label. Defaults to i18n "Model". */
  modelLabel?: string;
  /** Help-text rendered under the picker. Caller-owned i18n string. */
  helpText?: string;
  /**
   * Layout mode for the two selects.
   * - `'vertical'` (default): provider stacked above model.
   * - `'horizontal'`: single row (wraps on mobile). Use when vertical scroll matters.
   */
  layout?: 'vertical' | 'horizontal';
  /**
   * Extra elements (e.g. a probe button + result pills) rendered in the
   * SAME inline row as the provider+model selects. Only honoured in
   * horizontal layout. Use when a caller-owned action belongs on the
   * picker line rather than below it.
   */
  appendInline?: React.ReactNode;
}

/**
 * Convenience for the standalone case: AI-Guard wants only providers
 * that have at least one configured model so the cascading picker is
 * always completable. Exported so the unit test can exercise the
 * filter logic without driving Radix's Portal-based dropdown.
 */
export function filterCompletableProviders(
  groups: AdminModelsByProvider[],
  endpointType: string | undefined,
): AdminModelsByProvider[] {
  return groups.filter((g) => {
    if (!g.provider) return false;
    const models = g.models ?? [];
    if (models.length === 0) return false;
    if (endpointType) {
      return models.some((m) => m.type === endpointType);
    }
    return true;
  });
}

export function ProviderModelPicker({
  providerGroups,
  providerId,
  modelId,
  onChange,
  disabled,
  endpointType,
  providerLabel,
  modelLabel,
  helpText,
  layout = 'vertical',
  appendInline,
}: ProviderModelPickerProps) {
  const { t } = useTranslation();

  const visibleGroups = useMemo(
    () => filterCompletableProviders(providerGroups, endpointType),
    [providerGroups, endpointType],
  );

  const providerOptions = useMemo(
    () =>
      visibleGroups
        .map((g) => ({
          value: g.provider!.id,
          label: g.provider!.displayName?.trim() || g.provider!.name,
        }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [visibleGroups],
  );

  const selectedGroup = useMemo(
    () => visibleGroups.find((g) => g.provider?.id === providerId),
    [visibleGroups, providerId],
  );

  const modelOptions = useMemo(
    () =>
      (selectedGroup?.models ?? [])
        .filter((m) => !endpointType || m.type === endpointType)
        .map((m) => ({
          value: m.id,
          label: `${m.name} (${m.providerModelId})`,
        }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [selectedGroup, endpointType],
  );

  const noProvider = !providerId;
  const noModels = !!providerId && modelOptions.length === 0;

  const providerLabelStr = providerLabel ?? t('common:providerModelPicker.provider', 'Provider');
  const modelLabelStr = modelLabel ?? t('common:providerModelPicker.model', 'Model');

  // horizontal: inline labels; vertical: FormField stack (label above select).
  const isHorizontal = layout === 'horizontal';

  const providerSelect = (
    <Select
      value={providerId ?? ''}
      onValueChange={(v) =>
        // Reset modelId on every provider change — never persist a mismatched
        // pair. Empty string from the Select maps back to null on the wire.
        onChange({ providerId: v === '' ? null : v, modelId: null })
      }
      options={providerOptions}
      placeholder={t('common:providerModelPicker.selectProvider', 'Select a configured provider…')}
      disabled={disabled}
    />
  );

  const modelSelect = (
    <Select
      value={modelId ?? ''}
      onValueChange={(v) => onChange({ providerId, modelId: v === '' ? null : v })}
      options={modelOptions}
      disabled={disabled || noProvider || noModels}
      placeholder={
        noProvider
          ? t('common:providerModelPicker.selectProviderFirst', 'Select a provider first')
          : noModels
            ? t('common:providerModelPicker.noModelsForProvider', 'No models configured for this provider')
            : t('common:providerModelPicker.selectModel', 'Select a model…')
      }
    />
  );

  const horizontalRowStyle: React.CSSProperties = {
    display: 'flex',
    flexWrap: 'wrap',
    alignItems: 'center',
    gap: 'var(--g-space-2) var(--g-space-5)',
  };
  const inlineFieldStyle: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 'var(--g-space-2)',
  };
  const inlineLabelStyle: React.CSSProperties = {
    fontSize: '0.875rem',
    fontWeight: 600,
    color: 'var(--color-text-primary)',
    whiteSpace: 'nowrap',
  };

  return (
    <Stack gap="sm">
      {isHorizontal ? (
        <div style={horizontalRowStyle}>
          <label style={inlineFieldStyle}>
            <span style={inlineLabelStyle}>{providerLabelStr}</span>
            {providerSelect}
          </label>
          <label style={inlineFieldStyle}>
            <span style={inlineLabelStyle}>{modelLabelStr}</span>
            {modelSelect}
          </label>
          {appendInline}
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-2)' }}>
          <FormField label={providerLabelStr}>{providerSelect}</FormField>
          <FormField label={modelLabelStr}>{modelSelect}</FormField>
        </div>
      )}
      {helpText !== undefined && helpText !== '' && (
        <p style={{ fontSize: 'var(--font-size-sm)', color: 'var(--color-text-muted)' }}>
          {helpText}
        </p>
      )}
    </Stack>
  );
}
