import { useTranslation } from 'react-i18next';
import {
  Badge,
  Button,
  Card,
  Stack,
  statusToVariant
} from '@/components/ui';
import type { ProviderDetailState } from './useProviderDetail';
import { ModelFormDrawer } from './ModelFormDrawer';
import styles from './ProviderDetail.module.css';
import { formatTokens } from '@/lib/format';

interface ProviderModelsTabProps {
  detail: ProviderDetailState;
}

export function ProviderModelsTab({ detail }: ProviderModelsTabProps) {
  const { t } = useTranslation();
  const {
    models,
    canUpdate, canDelete, canCreateModel,
    showModelForm, setShowModelForm,
    resetModelForm,
    editingModelId, setEditingModelId,
    startEditingModel,
    setEditingCapabilityJson,
    toggleModelEnabled,
    setDeletingModel,
  } = detail;

  // Create + edit share one right slide-out drawer. The mode is derived from
  // which trigger opened it; the create/update mutations close it on success
  // (they reset showModelForm / editingModelId), so onClose only needs to cover
  // Cancel / Esc / overlay.
  const drawerMode: 'create' | 'edit' = editingModelId ? 'edit' : 'create';
  const drawerOpen = showModelForm || !!editingModelId;
  const closeDrawer = () => {
    if (editingModelId) {
      setEditingModelId(null);
      setEditingCapabilityJson(undefined);
    } else {
      setShowModelForm(false);
      resetModelForm();
    }
  };

  return (
    <div className={styles.modelTabStack}>
      <div className={styles.toolbarStart}>
        {canCreateModel && (
          <Button onClick={() => setShowModelForm(true)}>{t('pages:providers.addModel')}</Button>
        )}
      </div>

      <Card>
      {models.length === 0 ? (
        <div className={styles.emptyState}>{t('pages:providers.noModels')}</div>
      ) : (
        <div className={styles.modelCardGrid}>
          {models.map((m) => (
            <div key={m.id} className={styles.modelCard}>
              <div className={styles.modelCardHeader}>
                <div>
                  <div className={styles.modelCardName}>{m.name}</div>
                  <div className={styles.modelCardModelId}>{m.code}</div>
                  {m.providerModelId !== m.code && (
                    <div className={styles.modelCardModelId}>{t('pages:providers.providerModelId')}: {m.providerModelId}</div>
                  )}
                </div>
                <Stack direction="horizontal" gap="xs" align="center">
                  <Badge variant={statusToVariant(m.type)}>
                    {t(`pages:providers.modelType${m.type.charAt(0).toUpperCase() + m.type.slice(1)}`, m.type)}
                  </Badge>
                  <Badge variant={statusToVariant(m.enabled ? (m.status ?? 'active') : 'disabled')}>
                    {m.enabled
                      ? t(`pages:providers.modelStatus${(m.status ?? 'active').charAt(0).toUpperCase() + (m.status ?? 'active').slice(1)}`, m.status ?? 'active')
                      : t('common:disabled')}
                  </Badge>
                  {m.inputPricePerMillion == null && (
                    <Badge variant="warning">
                      {t('pages:providers.pricingNotSet')}
                    </Badge>
                  )}
                </Stack>
              </div>
              {m.description && <div className={styles.modelCardDesc}>{m.description}</div>}
              <div className={styles.modelCardStats}>
                {m.inputPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableInputPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.inputPricePerMillion}</span>
                  </div>
                )}
                {m.outputPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableOutputPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.outputPricePerMillion}</span>
                  </div>
                )}
                {m.cachedInputReadPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableCachedInputReadPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.cachedInputReadPricePerMillion}</span>
                  </div>
                )}
                {m.cachedInputWritePricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableCachedInputWritePrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.cachedInputWritePricePerMillion}</span>
                  </div>
                )}
                {m.maxContextTokens != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableContext')}</span>
                    <span className={styles.modelCardStatValue}>{formatTokens(m.maxContextTokens)}</span>
                  </div>
                )}
                {m.maxOutputTokens != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableOutput')}</span>
                    <span className={styles.modelCardStatValue}>{formatTokens(m.maxOutputTokens)}</span>
                  </div>
                )}
              </div>
              {m.features?.length > 0 && (
                <div className={styles.modelCardFeatures}>
                  {m.features.map((f) => (
                    <span key={f} className={styles.modelCardFeatureTag}>{f}</span>
                  ))}
                </div>
              )}
              {m.aliases && m.aliases.length > 0 && (
                <div className={styles.modelCardFeatures}>
                  <span className={styles.modelCardStatLabel}>{t('pages:providers.modelAliasesLabel')}:&nbsp;</span>
                  {m.aliases.map((a) => (
                    <span key={a} className={styles.modelCardFeatureTag}>{a}</span>
                  ))}
                </div>
              )}
              {(m.deprecationDate || m.replacedBy) && (
                <div className={styles.modelCardStats}>
                  {m.deprecationDate && (
                    <div className={styles.modelCardStat}>
                      <span className={styles.modelCardStatLabel}>{t('pages:providers.deprecationDate')}</span>
                      <span className={styles.modelCardStatValue}>{m.deprecationDate.split('T')[0]}</span>
                    </div>
                  )}
                  {m.replacedBy && (
                    <div className={styles.modelCardStat}>
                      <span className={styles.modelCardStatLabel}>{t('pages:providers.replacedByModel')}</span>
                      <span className={styles.modelCardStatValue}>{m.replacedBy}</span>
                    </div>
                  )}
                </div>
              )}
              <div className={styles.modelCardActions}>
                {canUpdate && (
                  <Button variant="secondary" size="sm" onClick={() => startEditingModel(m)}>{t('common:edit')}</Button>
                )}
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => toggleModelEnabled({ id: m.id, enabled: !m.enabled })}>
                    {m.enabled ? t('pages:providers.disable') : t('pages:providers.enable')}
                  </Button>
                )}
                {canDelete && (
                  <Button variant="danger" size="sm" onClick={() => setDeletingModel(m)}>{t('common:delete')}</Button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      <ModelFormDrawer detail={detail} mode={drawerMode} open={drawerOpen} onClose={closeDrawer} />
      </Card>
    </div>
  );
}
