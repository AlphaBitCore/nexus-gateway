import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Tooltip } from '@/components/ui';
import { MultiSelectDropdown } from '@/components/ui/MultiSelectDropdown';
import { ProviderMultiSelect } from '@/components/ui/ProviderMultiSelect';
import { ProjectMultiSelect } from '@/components/ui/ProjectMultiSelect';
import type { AdminModelsByProvider } from '@/api/types';
import { TagInput } from '../form/TagInput';
import { MODEL_TYPE_OPTIONS } from '../_shared/routing-rule-config';
import styles from './MatchConditionExtraFields.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

export interface MatchConditionExtraFieldsProps {
  providerGroups: AdminModelsByProvider[];
  selectedProviderIds: string[];
  onChangeProviders: (ids: string[]) => void;
  projectIds: string[];
  onChangeProjectIds: (ids: string[]) => void;
  requestedModelLiterals: string[];
  onChangeRequestedModelLiterals: (next: string[]) => void;
  modelTypes: string[];
  onChangeModelTypes: (next: string[]) => void;
  virtualKeys: string[];
  onChangeVirtualKeys: (next: string[]) => void;
}

export function MatchConditionExtraFields({
  providerGroups,
  selectedProviderIds,
  onChangeProviders,
  projectIds,
  onChangeProjectIds,
  requestedModelLiterals,
  onChangeRequestedModelLiterals,
  modelTypes,
  onChangeModelTypes,
  virtualKeys,
  onChangeVirtualKeys,
}: MatchConditionExtraFieldsProps) {
  const { t } = useTranslation();

  const modelTypeOptions = useMemo(
    () =>
      MODEL_TYPE_OPTIONS.map((v) => ({
        value: v,
        label: t(`pages:routing.modelType.${v}`),
      })),
    [t],
  );

  return (
    <>
      <div className={styles.fieldGroup}>
        <div className={styles.labelRow}>
          <label className={styles.label}>{t('pages:routing.matchRequestedModelLiteralsLabel')}</label>
          <Tooltip content={t('pages:routing.matchRequestedModelLiteralsTooltip')}>
            <HelpIconButton aria-label={t('pages:routing.helpMatchRequestedModelLiterals')} />
          </Tooltip>
        </div>
        <TagInput
          value={requestedModelLiterals}
          onChange={onChangeRequestedModelLiterals}
          placeholder={t('pages:routing.matchRequestedModelLiteralsPlaceholder')}
          ariaLabel={t('pages:routing.matchRequestedModelLiteralsLabel')}
        />
      </div>
      <div className={styles.fieldGroup}>
        <div className={styles.labelRow}>
          <label className={styles.label}>{t('pages:routing.matchModelTypesLabel')}</label>
          <Tooltip content={t('pages:routing.matchModelTypesTooltip')}>
            <HelpIconButton aria-label={t('pages:routing.helpMatchModelTypes')} />
          </Tooltip>
        </div>
        <MultiSelectDropdown
          label=""
          options={modelTypeOptions}
          value={modelTypes}
          onChange={onChangeModelTypes}
          emptyLabel={t('pages:routing.matchModelTypesPlaceholder')}
        />
      </div>
      <div className={styles.fieldGroup}>
        <div className={styles.labelRow}>
          <label className={styles.label}>{t('pages:routing.providersOptional')}</label>
          <Tooltip content={t('pages:routing.matchProvidersTooltip')}>
            <HelpIconButton aria-label={t('pages:routing.helpMatchProviders')} />
          </Tooltip>
        </div>
        <ProviderMultiSelect
          label=""
          providerGroups={providerGroups}
          value={selectedProviderIds}
          onChange={onChangeProviders}
        />
      </div>
      <div className={styles.fieldGroup}>
        <div className={styles.labelRow}>
          <label className={styles.label}>{t('pages:routing.projectsLabel')}</label>
          <Tooltip content={t('pages:routing.matchProjectsTooltip')}>
            <HelpIconButton aria-label={t('pages:routing.helpMatchProjects')} />
          </Tooltip>
        </div>
        <ProjectMultiSelect
          label=""
          value={projectIds}
          onChange={onChangeProjectIds}
          emptyLabel={t('pages:routing.placeholderProjects')}
        />
      </div>
      <div className={styles.fieldGroup}>
        <div className={styles.labelRow}>
          <label className={styles.label}>{t('pages:routing.matchVirtualKeysLabel')}</label>
          <Tooltip content={t('pages:routing.matchVirtualKeysTooltip')}>
            <HelpIconButton aria-label={t('pages:routing.helpMatchVirtualKeys')} />
          </Tooltip>
        </div>
        <TagInput
          value={virtualKeys}
          onChange={onChangeVirtualKeys}
          placeholder={t('pages:routing.matchVirtualKeysPlaceholder')}
          ariaLabel={t('pages:routing.matchVirtualKeysLabel')}
        />
      </div>
    </>
  );
}
