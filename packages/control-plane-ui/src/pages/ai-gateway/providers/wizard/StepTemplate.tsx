import { ErrorBanner, Input, LoadingSpinner } from '@/components/ui';
import type { ProviderWizardHook } from './useProviderWizard';
import { initials } from './helpers';
import styles from './ProviderWizard.module.css';
import { LinkButton } from '@nexus-gateway/ui-shared';

export function StepTemplate({ wizard }: { wizard: ProviderWizardHook }) {
  const {
    t,
    templates,
    templatesLoading,
    templatesError,
    refetchTemplates,
    templateQuery,
    handleTemplateQueryChange,
    filteredTemplates,
    browseAllTemplates,
    setBrowseAllTemplates,
    templatesForGrid,
    collapsedHiddenCount,
    selectedTemplate,
    isCustom,
    selectFromApiTemplate,
    selectCustom,
  } = wizard;

  return (
    <div className={styles.stepPanel}>
      <div className={styles.stepHeadRow}>
        <div className={styles.stepHeadRowInner}>
          <h2 className={styles.stepSectionTitle}>{t('pages:providers.chooseTemplate', 'Choose template or custom')}</h2>
          <p className={styles.stepSectionHint}>
            {t('pages:providers.chooseTemplateHint')}
          </p>
        </div>
      </div>

      {templatesError && <ErrorBanner message={templatesError.message} onRetry={refetchTemplates} />}

      {templatesLoading && (
        <div className={styles.centeredLoading}><LoadingSpinner /></div>
      )}

      {!templatesLoading && !templatesError && (
        <>
          <div className={styles.templateSearchBox}>
            <span className={styles.templateSearchIcon} aria-hidden />
            <Input
              id="provider-template-search"
              type="search"
              value={templateQuery}
              onChange={(e) => handleTemplateQueryChange(e.target.value)}
              placeholder={t('pages:providers.searchTemplatesPlaceholder')}
              autoComplete="off"
              aria-label={t('pages:providers.searchTemplatesPlaceholder')}
              className={styles.templateSearch}
            />
          </div>
          {templateQuery.trim() && (
            <p className={styles.filterMeta}>
              {t('pages:providers.templatesFilterMeta', { count: filteredTemplates.length, total: templates.length })}
              {filteredTemplates.length === 0 ? t('pages:providers.templatesFilterEmpty') : ''}
            </p>
          )}
          <button
            type="button"
            onClick={selectCustom}
            className={`${isCustom ? styles.customCardSelected : styles.customCard} ${styles.customCardStandalone}`}
          >
            <span className={styles.templateName}>{t('pages:providers.customProvider', 'Custom provider')}</span>
            <span className={styles.customHint}>
              {t('pages:providers.customHint', 'Own base URL and model IDs — private or niche APIs.')}
            </span>
          </button>
          <div className={styles.templateDivider} />
          <div className={styles.templateListOuter}>
            <div className={styles.templateGrid}>
              {filteredTemplates.length === 0 && templateQuery.trim() && templates.length > 0 && (
                <div className={`${styles.spanFull} ${styles.noMatchHint}`}>
                  {t('pages:providers.noTemplatesMatch')}
                </div>
              )}
              {templatesForGrid.map((tpl) => {
                const selected = !isCustom && selectedTemplate === tpl.name;
                return (
                  <button
                    key={tpl.name}
                    type="button"
                    onClick={() => { void selectFromApiTemplate(tpl); }}
                    title={tpl.description}
                    className={selected ? styles.templateCardSelected : styles.templateCard}
                  >
                    <div className={styles.templateCardRow}>
                      <div className={styles.templateAvatar}>
                        {initials(tpl.displayName)}
                      </div>
                      <div className={styles.templateCardInfo}>
                        <div className={styles.templateNameRow}>
                          <span className={styles.templateName}>{tpl.displayName}</span>
                          {tpl.adapterType === 'openai' && tpl.name !== 'openai' && (
                            <span className={styles.templateApiTag}>{t('pages:providers.templateApiTag', 'OpenAI API')}</span>
                          )}
                          <span className={styles.templateModelCount}>{t('pages:providers.modelsCount', { count: tpl.modelCount })}</span>
                        </div>
                        <p className={styles.templateDescription}>{tpl.description}</p>
                      </div>
                    </div>
                  </button>
                );
              })}
            </div>
          </div>
          {!templateQuery.trim() && collapsedHiddenCount > 0 && (
            <div className={styles.browseToggleRow}>
              <LinkButton onClick={() => setBrowseAllTemplates(!browseAllTemplates)}>
                {browseAllTemplates
                  ? t('pages:providers.showFewer', 'Show fewer providers')
                  : t('pages:providers.browseMore', 'Browse more ({{count}} more)', { count: collapsedHiddenCount })}
              </LinkButton>
            </div>
          )}
        </>
      )}
    </div>
  );
}
