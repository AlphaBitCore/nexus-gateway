import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { fleetApi } from '@/api/services';
import {
  PageHeader, Stack, Card, Button, Skeleton, ErrorBanner,
} from '@/components/ui';
import styles from './DeviceAuthSettingsPage.module.css';

export function DeviceAuthSettingsPage() {
  const { t } = useTranslation();
  const [mode, setMode] = useState('mtls-only');

  const { data, loading, error, refetch } = useApi(
    () => fleetApi.getDeviceAuthSettings(),
    ['admin', 'settings', 'device-auth'],
  );

  useEffect(() => {
    if (data) setMode(data.mode);
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () => fleetApi.updateDeviceAuthSettings({ mode }),
    {
      invalidateQueries: [['api', 'admin', 'settings', 'device-auth']],
      onSuccess: () => refetch(),
      successMessage: t('pages:fleet.deviceAuthUpdated', 'Device auth settings updated'),
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const ssoConfigured = data?.ssoConfigured ?? false;
  const ssoProviders = data?.ssoProviders ?? [];
  const localLoginAvailable = data?.localLoginAvailable ?? false;
  const enterpriseDisabled = mode === 'enterprise-login' && !ssoConfigured;
  const localDisabled = mode === 'local-login' && !localLoginAvailable;
  const saveDisabled = enterpriseDisabled || localDisabled;

  return (
    <Stack gap="md">
      <PageHeader title={t('pages:fleet.deviceAuth')} subtitle={t('pages:fleet.deviceAuthSubtitle')} />
      <Card>
        <div className={styles.content}>
          <div className={styles.radioGroup}>
            <div className={styles.radioOption}>
              <label className={styles.radioLabel}>
                <input type="radio" name="authMode" value="mtls-only" checked={mode === 'mtls-only'} onChange={() => setMode('mtls-only')} />
                <span>{t('pages:fleet.authModeMtls')}</span>
              </label>
              <p className={styles.radioDescription}>{t('pages:fleet.authModeDescription')}</p>
            </div>

            <div className={styles.radioOption}>
              <label className={styles.radioLabel}>
                <input type="radio" name="authMode" value="local-login" checked={mode === 'local-login'} onChange={() => setMode('local-login')} />
                <span>{t('pages:fleet.authModeLocal', 'Local Login')}</span>
              </label>
              <p className={styles.radioDescription}>{t('pages:fleet.authModeLocalDesc')}</p>
              <p className={styles.radioAdvisory}>{t('pages:fleet.authModeLocalAdvisory')}</p>
            </div>

            <div className={styles.radioOption}>
              <label className={styles.radioLabel}>
                <input type="radio" name="authMode" value="enterprise-login" checked={mode === 'enterprise-login'} onChange={() => setMode('enterprise-login')} />
                <span>{t('pages:fleet.authModeEnterprise', 'Enterprise Login')}</span>
              </label>
              <p className={styles.radioDescription}>{t('pages:fleet.authModeEnterpriseDesc')}</p>
            </div>
          </div>

          {mode === 'local-login' && !localLoginAvailable && (
            <div className={styles.warning}>
              {t(
                'pages:fleet.localLoginUnavailable',
                'The built-in Nexus Local identity store is missing or disabled. Re-seed the platform or re-enable it before choosing Local Login.',
              )}
            </div>
          )}

          {mode === 'enterprise-login' && (
            <div className={styles.oidcForm}>
              <h4 className={styles.sectionTitle}>{t('pages:fleet.ssoProvidersTitle', 'SSO Identity Providers')}</h4>
              {ssoConfigured ? (
                <Stack gap="sm">
                  <p className={styles.providerHint}>
                    {t('pages:fleet.ssoProvidersHint', 'Agents will sign in via the following identity providers configured under SSO Settings.')}
                  </p>
                  <ul className={styles.providerList}>
                    {ssoProviders.map((p) => (
                      <li key={p.id} className={styles.providerItem}>
                        <span className={styles.providerName}>{p.name}</span>
                        <span className={styles.providerType}>{p.type.toUpperCase()}</span>
                      </li>
                    ))}
                  </ul>
                </Stack>
              ) : (
                <div className={styles.warning}>
                  {t(
                    'pages:fleet.ssoNotConfiguredWarning',
                    'Enterprise Login requires at least one OIDC or SAML provider — add one under SSO Settings. If you do not run an external IdP, choose Local Login above instead.',
                  )}
                </div>
              )}
            </div>
          )}

        </div>
      </Card>
      <div className={styles.actions}>
        <Button className={styles.actionButton} onClick={() => save(undefined as never)} loading={saving} disabled={saveDisabled}>
          {t('common:save', 'Save')}
        </Button>
      </div>
    </Stack>
  );
}
