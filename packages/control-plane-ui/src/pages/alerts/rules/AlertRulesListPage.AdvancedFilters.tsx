/**
 * AlertRulesListPage advanced-filters panel — the collapsible enabled /
 * severity / sourceType draft filter form rendered inside the search box.
 *
 * Extracted from AlertRulesListPage to keep the page under the file-size
 * ratchet. Behavior, markup, i18n keys, and design tokens are unchanged; the
 * parent owns all draft state and the confirm/reset handlers.
 */
import type { Dispatch, SetStateAction } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui';
import type { AlertSeverity } from '@/api/services';
import styles from './AlertRulesListPage.module.css';

const ALL_SEVERITIES: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];

interface AlertRulesAdvancedFiltersProps {
  draftEnabledFilter: '' | 'true' | 'false';
  setDraftEnabledFilter: Dispatch<SetStateAction<'' | 'true' | 'false'>>;
  draftSeverityFilter: '' | AlertSeverity;
  setDraftSeverityFilter: Dispatch<SetStateAction<'' | AlertSeverity>>;
  draftSourceTypeFilter: string;
  setDraftSourceTypeFilter: Dispatch<SetStateAction<string>>;
  severityLabel: Record<AlertSeverity, string>;
  sourceTypeOptions: string[];
  onReset: () => void;
  onConfirm: () => void;
}

export function AlertRulesAdvancedFilters({
  draftEnabledFilter,
  setDraftEnabledFilter,
  draftSeverityFilter,
  setDraftSeverityFilter,
  draftSourceTypeFilter,
  setDraftSourceTypeFilter,
  severityLabel,
  sourceTypeOptions,
  onReset,
  onConfirm,
}: AlertRulesAdvancedFiltersProps) {
  const { t } = useTranslation();
  return (
    <div className={styles.advancedPanel}>
      <div className={styles.advancedGrid}>
        <div className={styles.filterField}>
          <span className={styles.filterLabel}>{t('pages:alerts.rules.filterEnabledAria')}</span>
          <select
            aria-label={t('pages:alerts.rules.filterEnabledAria')}
            value={draftEnabledFilter}
            onChange={(e) => setDraftEnabledFilter(e.target.value as '' | 'true' | 'false')}
            className={styles.filterSelect}
          >
            <option value="">{t('pages:alerts.rules.filterEnabledAll')}</option>
            <option value="true">{t('pages:alerts.rules.filterEnabledOnly')}</option>
            <option value="false">{t('pages:alerts.rules.filterDisabledOnly')}</option>
          </select>
        </div>
        <div className={styles.filterField}>
          <span className={styles.filterLabel}>{t('pages:alerts.rules.filterSeverityAria')}</span>
          <select
            aria-label={t('pages:alerts.rules.filterSeverityAria')}
            value={draftSeverityFilter}
            onChange={(e) => setDraftSeverityFilter(e.target.value as '' | AlertSeverity)}
            className={styles.filterSelect}
          >
            <option value="">{t('pages:alerts.rules.filterSeverityAll')}</option>
            {ALL_SEVERITIES.map((s) => (
              <option key={s} value={s}>{severityLabel[s]}</option>
            ))}
          </select>
        </div>
        <div className={styles.filterField}>
          <span className={styles.filterLabel}>{t('pages:alerts.rules.filterSourceTypeAria')}</span>
          <select
            aria-label={t('pages:alerts.rules.filterSourceTypeAria')}
            value={draftSourceTypeFilter}
            onChange={(e) => setDraftSourceTypeFilter(e.target.value)}
            className={styles.filterSelect}
          >
            <option value="">{t('pages:alerts.rules.filterSourceTypeAll')}</option>
            {sourceTypeOptions.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </div>
      </div>
      <div className={styles.advancedFooter}>
        <Button variant="secondary" className={styles.advancedFooterButton} onClick={onReset}>
          {t('common:reset')}
        </Button>
        <Button className={styles.advancedFooterButton} onClick={onConfirm}>
          {t('common:confirmSearch')}
        </Button>
      </div>
    </div>
  );
}
