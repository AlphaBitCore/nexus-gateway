import { useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { providerApi, type ProviderConnectivityResult } from '@/api/services';
import styles from './ProviderConnectivityTestButton.module.css';

export type { ProviderConnectivityResult };

export interface CredentialOption {
  id: string;
  name: string;
  enabled: boolean;
}

type Props =
  | { variant: 'existing'; providerId: string; credentials?: CredentialOption[] }
  | {
      variant: 'draft';
      name: string;
      adapterType: string;
      baseUrl: string;
      apiKey: string;
    };

/**
 * Runs a real GET to upstream `/v1/models` (same probe as gateway admin API).
 */
export function ProviderConnectivityTestButton(props: Props) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<ProviderConnectivityResult | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);

  const enabledCredentials = props.variant === 'existing' && props.credentials
    ? props.credentials.filter(c => c.enabled)
    : [];
  const showCredentialSelector = enabledCredentials.length > 1;

  const [selectedCredId, setSelectedCredId] = useState<string>('');

  const draftBlocked = props.variant === 'draft' && (!props.name.trim() || !props.baseUrl.trim());

  const run = async () => {
    setLoading(true);
    setLocalError(null);
    setResult(null);
    try {
      if (props.variant === 'existing') {
        const credId = showCredentialSelector && selectedCredId ? selectedCredId : undefined;
        const r = await providerApi.testExisting(props.providerId, credId);
        setResult(r);
      } else {
        const r = await providerApi.testConnection({
          name: props.name.trim(),
          adapterType: props.adapterType,
          baseUrl: props.baseUrl.trim(),
          apiKey: props.apiKey,
        });
        setResult(r);
      }
    } catch (e) {
      setLocalError(e instanceof Error ? e.message : 'Request failed');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div>
      {showCredentialSelector && (
        <div className={styles.credentialSelectorRow}>
          <label htmlFor="credential-selector" className={styles.credentialSelectorLabel}>
            {t('pages:providers.credentialLabel', 'Credential')}
          </label>
          <select
            id="credential-selector"
            value={selectedCredId}
            onChange={e => setSelectedCredId(e.target.value)}
            className={styles.credentialSelector}
            disabled={loading}
          >
            <option value="">{t('pages:providers.credentialAny', 'Any (provider default)')}</option>
            {enabledCredentials.map(c => (
              <option key={c.id} value={c.id}>{c.name}</option>
            ))}
          </select>
        </div>
      )}
      <button
        type="button"
        onClick={run}
        disabled={loading || draftBlocked}
        className={loading || draftBlocked ? styles.testButtonDisabled : styles.testButton}
      >
        {loading ? t('pages:providers.testing', 'Testing…') : t('pages:providers.testConnection')}
      </button>
      <p className={styles.helpText}>
        <Trans
          i18nKey="pages:providers.connectivityHelp"
          defaults="Sends a real GET to the provider base URL + <0>/v1/models</0> using vault or typed credentials (hello-world style reachability check)."
          components={[<code key="0" className={styles.helpCode} />]}
        />
      </p>
      {localError && (
        <p className={styles.errorText}>{localError}</p>
      )}
      {result && (
        <div className={result.success ? styles.resultBoxOk : styles.resultBoxError}>
          <div className={result.success ? styles.resultStatusOk : styles.resultStatusError}>
            {result.success
              ? t('pages:providers.reachable', 'Reachable — {{ms}} ms', { ms: result.latencyMs ?? 0 }) + (result.statusCode != null ? ` (HTTP ${result.statusCode})` : '')
              : t('pages:providers.unreachable', 'Unreachable or rejected — {{ms}} ms', { ms: result.latencyMs ?? 0 }) + (result.statusCode != null ? ` (HTTP ${result.statusCode})` : '')}
          </div>
          {result.error && (
            <div className={styles.resultMessage}>{result.error}</div>
          )}
          <div className={styles.resultUrl}>
            {result.endpoint}
          </div>
        </div>
      )}
    </div>
  );
}
