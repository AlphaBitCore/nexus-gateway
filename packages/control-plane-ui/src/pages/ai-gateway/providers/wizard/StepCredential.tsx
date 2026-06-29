import {
  FormField, Input, Button, Stack,
} from '@/components/ui';
import { ProviderConnectivityTestButton } from '../list/ProviderConnectivityTestButton';
import type { ProviderWizardHook } from './useProviderWizard';
import styles from './ProviderWizard.module.css';

export function StepCredential({ wizard }: { wizard: ProviderWizardHook }) {
  const {
    t,
    name,
    adapterType,
    baseUrl,
    credName, setCredName,
    credNameError, credNameChecking,
    apiKey, setApiKey,
    skipCredential, setSkipCredential,
  } = wizard;

  return (
    <div className={styles.stepPanelLarge}>
      <h2 className={styles.stepTitle}>{t('pages:providers.credential', 'Credential')}</h2>
      <p className={styles.credSubtitle}>
        {t('pages:providers.credentialSubtitle')}
      </p>

      {skipCredential ? (
        <div className={styles.credSkipped}>
          <p className={styles.credSkipText}>
            {t('pages:providers.noCredentialYet')}
          </p>
          <Button variant="secondary" onClick={() => setSkipCredential(false)}>{t('pages:providers.addCredentialNow', 'Add credential now')}</Button>
        </div>
      ) : (
        <Stack gap="md" className={styles.credFormStack}>
          <FormField
            label={t('pages:providers.credentialName')}
            required
            error={credNameError ?? undefined}
            helpText={
              credNameChecking
                ? t('pages:providers.nameChecking', 'Checking availability…')
                : credNameError
                  ? undefined
                  : t('pages:providers.credNameHelp', 'Credential names are globally unique.')
            }
          >
            <Input value={credName} onChange={e => setCredName(e.target.value)} placeholder={t('pages:providers.placeholderCredName')} />
          </FormField>
          <FormField label={t('pages:providers.apiKeyLabel')} required>
            <Input value={apiKey} onChange={e => setApiKey(e.target.value)} type="password" placeholder={t('pages:providers.placeholderApiKey')} />
          </FormField>
          <div className={styles.credInfoBanner}>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--color-info)" strokeWidth="2" aria-hidden>
              <rect x="3" y="11" width="18" height="11" rx="2" />
              <path d="M7 11V7a5 5 0 0110 0v4" />
            </svg>
            {t('pages:providers.credentialEncryptionNote')}
          </div>
          <Button variant="secondary" onClick={() => setSkipCredential(true)} className={styles.skipCredentialButton}>
            {t('pages:providers.skipCredential', 'Skip — I will add a credential later')}
          </Button>
        </Stack>
      )}

      <div className={styles.reachabilitySection}>
        <h3 className={styles.reachabilityTitle}>{t('pages:providers.reachability', 'Reachability')}</h3>
        <ProviderConnectivityTestButton
          variant="draft"
          name={name}
          adapterType={adapterType}
          baseUrl={baseUrl}
          apiKey={skipCredential ? '' : apiKey}
        />
      </div>
    </div>
  );
}
