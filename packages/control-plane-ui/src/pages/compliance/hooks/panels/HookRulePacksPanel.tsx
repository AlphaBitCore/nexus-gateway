import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type RulePackInstall } from '@/api/services';
import { Button, Card, ErrorBanner, Stack, Switch } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import { BindPackModal } from '../../rule-packs/bind/BindPackModal';
import { OverridesPanel } from '../../rule-packs/overrides/OverridesPanel';

import styles from './HookRulePacksPanel.module.css';

function formatInstalledAt(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

export interface HookRulePacksPanelProps {
  hookId: string;
}

export function HookRulePacksPanel({ hookId }: HookRulePacksPanelProps) {
  const { t } = useTranslation();
  const [bindOpen, setBindOpen] = useState(false);
  const [selectedInstall, setSelectedInstall] = useState<RulePackInstall | null>(null);

  const { data, loading, error, refetch } = useApi<RulePackInstall[]>(
    () => rulePacksApi.listInstallsForHook(hookId),
    ['admin', 'hooks', 'rule-pack-installs', hookId],
  );

  const { mutate: patchInstall, loading: toggling } = useMutation<
    { installId: string; enabled: boolean },
    { installId: string; enabled: boolean }
  >(({ installId, enabled }) => rulePacksApi.patchInstall(installId, enabled), {
    invalidateQueries: [['admin', 'hooks', 'rule-pack-installs', hookId]],
    successMessage: t('pages:hooks.rulePacks.patchInstallSuccess', 'Install updated'),
  });

  const { mutate: uninstall, loading: uninstalling } = useMutation<string, void>(
    (installId) => rulePacksApi.uninstall(installId),
    {
      invalidateQueries: [['admin', 'hooks', 'rule-pack-installs', hookId]],
      successMessage: t('pages:hooks.rulePacks.uninstallSuccess', 'Rule pack uninstalled'),
      onSuccess: () => setSelectedInstall(null),
    },
  );

  function onToggle(install: RulePackInstall, next: boolean) {
    patchInstall({ installId: install.id, enabled: next });
  }

  function onUninstall(install: RulePackInstall) {
    const message = t(
      'pages:hooks.rulePacks.uninstallConfirm',
      'Uninstall "{{name}}" {{version}} from this hook?',
      { name: install.packName, version: install.pinVersion },
    );
     
    if (!window.confirm(message)) return;
    uninstall(install.id);
  }

  return (
    <Stack gap="md">
      <Card>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>
              {t('pages:hooks.rulePacks.installsTitle', 'Installed Rule Packs')}
            </h2>
            <p className={styles.subtitle}>
              {t(
                'pages:hooks.rulePacks.installsSubtitle',
                'Bind rule packs to this hook. Installs feed the unified rule-pack evaluation engine.',
              )}
            </p>
          </div>
          <Button onClick={() => setBindOpen(true)}>
            {t('pages:hooks.rulePacks.bindButton', 'Bind rule pack')}
          </Button>
        </div>

        {loading && <div className={styles.state}>{t('common:loading', 'Loading…')}</div>}
        {error && <ErrorBanner message={error.message} onRetry={refetch} />}

        {!loading && !error && (data ?? []).length === 0 && (
          <div className={styles.state}>
            {t('pages:hooks.rulePacks.installsEmpty', 'No rule packs bound yet.')}
          </div>
        )}

        {!loading && !error && (data ?? []).length > 0 && (
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>{t('pages:hooks.rulePacks.colPackName', 'Pack')}</th>
                  <th>{t('pages:hooks.rulePacks.colVersion', 'Version')}</th>
                  <th>{t('pages:hooks.rulePacks.colInstalledAt', 'Installed')}</th>
                  <th>{t('pages:hooks.rulePacks.colEnabled', 'Enabled')}</th>
                  <th aria-label={t('pages:hooks.rulePacks.colActions', 'Actions')} />
                </tr>
              </thead>
              <tbody>
                {(data ?? []).map((install) => (
                  <tr
                    key={install.id}
                    data-selected={selectedInstall?.id === install.id || undefined}
                  >
                    <td>{install.packName}</td>
                    <td>{install.pinVersion}</td>
                    <td>{formatInstalledAt(install.installedAt)}</td>
                    <td>
                      <Switch
                        checked={install.enabled}
                        onCheckedChange={(next) => onToggle(install, next)}
                        disabled={toggling}
                        aria-label={t('pages:hooks.rulePacks.colEnabled', 'Enabled')}
                      />
                    </td>
                    <td className={styles.rowActions}>
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() =>
                          setSelectedInstall(selectedInstall?.id === install.id ? null : install)
                        }
                      >
                        {selectedInstall?.id === install.id
                          ? t('pages:hooks.rulePacks.hideOverrides', 'Hide overrides')
                          : t('pages:hooks.rulePacks.manageOverrides', 'Manage overrides')}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => onUninstall(install)}
                        disabled={uninstalling}
                      >
                        {t('pages:hooks.rulePacks.uninstallButton', 'Uninstall')}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {selectedInstall && <OverridesPanel installId={selectedInstall.id} />}

      <BindPackModal
        open={bindOpen}
        hookId={hookId}
        onClose={() => setBindOpen(false)}
        onBound={() => {
          setBindOpen(false);
          refetch();
        }}
      />
    </Stack>
  );
}
