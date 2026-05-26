import { useTranslation } from 'react-i18next';
import { Button, Tooltip, Card } from '@/components/ui';
import type { AdminModelsByProvider } from '@/api/types';
import type { FallbackEntry } from '../_shared/routing-rule-config';
import { ProviderModelSelect } from './StrategyConfigSection';
import styles from './RoutingRuleForm.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

export interface FallbackChainSectionProps {
  fallbackEntries: FallbackEntry[];
  addFallback: () => void;
  removeFallback: (idx: number) => void;
  updateFallback: (idx: number, field: keyof FallbackEntry, value: string) => void;
  providerGroups: AdminModelsByProvider[];
}

export function FallbackChainSection({
  fallbackEntries,
  addFallback,
  removeFallback,
  updateFallback,
  providerGroups,
}: FallbackChainSectionProps) {
  const { t } = useTranslation();

  return (
    <Card padding="lg">
      <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
        <div className={styles.sectionTitle}>{t('pages:routing.fallbackChainTitle')}</div>
        <Tooltip content={t('pages:routing.fallbackChainTooltipShort')}>
          <HelpIconButton aria-label={t('pages:routing.ariaHelpFallbackChain')} />
        </Tooltip>
      </div>
      {fallbackEntries.length === 0 ? (
        <div className={styles.emptyHint}>
          {t('pages:routing.noFallbackModels')}
        </div>
      ) : (
        fallbackEntries.map((entry, idx) => (
          <div key={idx} className={`${styles.entryRow} ${styles.fallbackEntrySpacing}`}>
            <span className={styles.indexLabel}>{idx + 1}.</span>
            <ProviderModelSelect
              providerValue={entry.provider}
              modelValue={entry.model}
              onProviderChange={(v) => updateFallback(idx, 'provider', v)}
              onModelChange={(v) => updateFallback(idx, 'model', v)}
              providerGroups={providerGroups}
            />
            <Button variant="danger" size="sm" onClick={() => removeFallback(idx)}>{t('pages:routing.remove')}</Button>
          </div>
        ))
      )}
      <Button variant="secondary" size="sm" onClick={addFallback}>{t('pages:routing.addFallback')}</Button>
    </Card>
  );
}
