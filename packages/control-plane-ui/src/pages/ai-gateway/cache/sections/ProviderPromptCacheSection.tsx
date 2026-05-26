/**
 * Section 2 — Provider Prompt Cache.
 *
 * Wraps the existing SettingsCacheTab (normaliser, adapter knobs,
 * normalisation rules, active overrides) inside a labelled section.
 * No behaviour change — all prompt-cache logic stays in SettingsCacheTab.
 */
import { useTranslation } from 'react-i18next';
import { Card, Stack } from '@/components/ui';
import { SettingsCacheTab } from '../../../compliance/cache/SettingsCacheTab';
import styles from './ProviderPromptCacheSection.module.css';

export function ProviderPromptCacheSection() {
  const { t } = useTranslation();
  return (
    <Card>
      <h3 id="card-provider-prompt-cache" className={styles.cardHeading}>
        {t('pages:aiGateway.cache.providerPrompt.title')}
      </h3>
      <p className={styles.cardSubtitle}>
        {t('pages:aiGateway.cache.providerPrompt.subtitle')}
      </p>
      <section aria-labelledby="card-provider-prompt-cache">
        <Stack gap="md">
          <SettingsCacheTab />
        </Stack>
      </section>
    </Card>
  );
}
