import clsx from 'clsx';
import { Badge, statusToVariant } from '@/components/ui';
import type { ProviderWizardHook } from './useProviderWizard';
import styles from './ProviderWizard.module.css';
// Reuse the detail page's Models-tab card grid styles so the Review step and
// ProviderModelsTab render models with one visual language.
import cardStyles from '../detail/ProviderDetail.module.css';

function ReviewRow({
  label,
  value,
  mono,
  muted,
  last,
}: {
  label: string;
  value: string;
  mono?: boolean;
  muted?: boolean;
  last?: boolean;
}) {
  return (
    <div className={last ? styles.reviewRowLast : styles.reviewRow}>
      <span className={styles.reviewLabel}>{label}</span>
      <span
        className={
          muted
            ? styles.reviewValueMuted
            : mono
              ? styles.reviewValueMono
              : styles.reviewValue
        }
      >
        {value}
      </span>
    </div>
  );
}

export function StepReview({ wizard }: { wizard: ProviderWizardHook }) {
  const {
    t,
    name,
    displayName,
    baseUrl,
    adapterType,
    skipCredential,
    credName,
    apiKey,
    models,
  } = wizard;

  return (
    <div className={clsx(styles.stepPanelLarge, styles.reviewPanel)}>
      <h2 className={clsx(styles.stepTitle, styles.reviewTitleSpaced)}>{t('pages:providers.review', 'Review')}</h2>

      <div className={styles.reviewGrid}>
        <section className={styles.reviewCard}>
          <h3 className={styles.reviewSectionTitle}>{t('pages:providers.reviewProvider')}</h3>
          <ReviewRow label={t('pages:providers.name')} value={name} />
          {displayName && <ReviewRow label={t('pages:providers.displayName')} value={displayName} />}
          <ReviewRow label={t('pages:providers.baseUrl')} value={baseUrl} mono />
          <ReviewRow
            label={t('pages:providers.adapter')}
            value={t(`pages:providers.adapterOption_${adapterType}`, adapterType)}
            last
          />
        </section>

        <section className={styles.reviewCard}>
          <h3 className={styles.reviewSectionTitle}>{t('pages:providers.reviewCredential')}</h3>
          {skipCredential ? (
            <ReviewRow label={t('pages:providers.status')} value={t('pages:providers.reviewSkipped')} muted last />
          ) : (
            <>
              <ReviewRow label={t('pages:providers.name')} value={credName} />
              <ReviewRow label={t('pages:providers.apiKeyLabel')} value={`${apiKey.slice(0, 6)}${'\u2022'.repeat(Math.min(24, Math.max(4, apiKey.length - 6)))}`} mono last />
            </>
          )}
        </section>

        <section className={styles.reviewModelsSection}>
          <div className={styles.reviewModelsTitleRow}>
            <h3 className={styles.reviewSectionTitle}>
              {t('pages:providers.reviewSelected')} {t('pages:providers.reviewModels')}
            </h3>
            {models.filter((m) => m.selected).length > 0 && (
              <span className={styles.reviewModelsCount}>
                {t('pages:providers.reviewModelCount', { count: models.filter((m) => m.selected).length })}
              </span>
            )}
          </div>
          {models.filter((m) => m.selected).length === 0 ? (
            <ReviewRow label={t('pages:providers.reviewModels')} value={t('pages:providers.reviewNoneOptional')} muted last />
          ) : (
            <>
              <div className={cardStyles.modelCardGrid}>
                {models.filter((m) => m.selected).map((m) => {
                  const inputN = m.inputPrice ? Number(m.inputPrice) : null;
                  const outputN = m.outputPrice ? Number(m.outputPrice) : null;
                  const ctxN = m.maxContextTokens ? Number(m.maxContextTokens) : null;
                  const outTokN = m.maxOutputTokens ? Number(m.maxOutputTokens) : null;
                  return (
                    <div key={m.modelId} className={clsx(cardStyles.modelCard, styles.reviewModelCard)}>
                      <div className={cardStyles.modelCardHeader}>
                        <div>
                          <div className={cardStyles.modelCardName}>{m.name}</div>
                          <div className={cardStyles.modelCardModelId}>{m.modelId}</div>
                        </div>
                        <Badge variant={statusToVariant(m.type)}>{m.type}</Badge>
                      </div>
                      {m.description && <div className={cardStyles.modelCardDesc}>{m.description}</div>}
                      {(inputN != null || outputN != null || ctxN != null || outTokN != null) && (
                        <div className={cardStyles.modelCardStats}>
                          {inputN != null && (
                            <div className={cardStyles.modelCardStat}>
                              <span className={cardStyles.modelCardStatLabel}>{t('pages:providers.modelTableInputPrice')}</span>
                              <span className={cardStyles.modelCardStatValue}>${inputN}</span>
                            </div>
                          )}
                          {outputN != null && (
                            <div className={cardStyles.modelCardStat}>
                              <span className={cardStyles.modelCardStatLabel}>{t('pages:providers.modelTableOutputPrice')}</span>
                              <span className={cardStyles.modelCardStatValue}>${outputN}</span>
                            </div>
                          )}
                          {ctxN != null && (
                            <div className={cardStyles.modelCardStat}>
                              <span className={cardStyles.modelCardStatLabel}>{t('pages:providers.modelTableContext')}</span>
                              <span className={cardStyles.modelCardStatValue}>{ctxN.toLocaleString()}</span>
                            </div>
                          )}
                          {outTokN != null && (
                            <div className={cardStyles.modelCardStat}>
                              <span className={cardStyles.modelCardStatLabel}>{t('pages:providers.modelTableOutput')}</span>
                              <span className={cardStyles.modelCardStatValue}>{outTokN.toLocaleString()}</span>
                            </div>
                          )}
                        </div>
                      )}
                      {m.features.length > 0 && (
                        <div className={cardStyles.modelCardFeatures}>
                          {m.features.map((f) => (
                            <span key={f} className={cardStyles.modelCardFeatureTag}>{f}</span>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </>
          )}
        </section>
      </div>
    </div>
  );
}
