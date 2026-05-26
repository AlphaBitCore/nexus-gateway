import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type RulePackInstall, type RulePackMeta } from '@/api/services';
import { Button, Dialog, ErrorBanner, FormField, Stack, Switch } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import styles from './BindPackModal.module.css';

export interface BindPackModalProps {
  open: boolean;
  hookId: string;
  onClose: () => void;
  onBound: (install: RulePackInstall) => void;
}

export function BindPackModal({ open, hookId, onClose, onBound }: BindPackModalProps) {
  const { t } = useTranslation();
  const [selectedName, setSelectedName] = useState('');
  const [selectedVersion, setSelectedVersion] = useState('');
  const [enabled, setEnabled] = useState(true);

  const { data, loading, error } = useApi<RulePackMeta[]>(
    () => rulePacksApi.list(),
    ['admin', 'rule-packs', 'bind-modal'],
    { skip: !open },
  );

  const packsByName = useMemo(() => {
    const grouped = new Map<string, RulePackMeta[]>();
    for (const pack of data ?? []) {
      const existing = grouped.get(pack.name) ?? [];
      existing.push(pack);
      grouped.set(pack.name, existing);
    }
    for (const entries of grouped.values()) {
      entries.sort((left, right) => right.version.localeCompare(left.version));
    }
    return grouped;
  }, [data]);

  const packNames = useMemo(() => Array.from(packsByName.keys()).sort(), [packsByName]);
  const versions = selectedName ? packsByName.get(selectedName) ?? [] : [];

  const { mutate: installPack, loading: saving, error: saveError } = useMutation(
    async (input: { packId: string; pinVersion: string; enabled: boolean }) =>
      rulePacksApi.install(hookId, input),
    {
      successMessage: t('pages:hooks.rulePacks.bindSuccess', 'Rule pack installed'),
      onSuccess: (install) => {
        handleClose();
        onBound(install);
      },
    },
  );

  function handleClose() {
    setSelectedName('');
    setSelectedVersion('');
    setEnabled(true);
    onClose();
  }

  function choosePack(name: string) {
    setSelectedName(name);
    const nextVersions = packsByName.get(name) ?? [];
    setSelectedVersion(nextVersions[0]?.version ?? '');
  }

  async function handleSubmit() {
    const selectedPack = versions.find((item) => item.version === selectedVersion);
    if (!selectedPack) return;
    await installPack({
      packId: selectedPack.id,
      pinVersion: selectedPack.version,
      enabled,
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) handleClose();
      }}
      title={t('pages:hooks.rulePacks.bindTitle', 'Bind Rule Pack')}
      description={t(
        'pages:hooks.rulePacks.bindSubtitle',
        'Choose a pack family, pin a version, and install it onto the current hook.',
      )}
      size="lg"
    >
      <Stack gap="md">
        {loading && <div className={styles.state}>{t('common:loading', 'Loading…')}</div>}
        {error && <ErrorBanner message={error.message} />}
        {saveError && <ErrorBanner message={saveError.message} />}

        {!loading && !error && (
          <>
            <div>
              <div className={styles.label}>
                {t('pages:hooks.rulePacks.bindPack', 'Pack')}
              </div>
              <div className={styles.packList}>
                {packNames.map((name) => (
                  <button
                    key={name}
                    type="button"
                    className={styles.packButton}
                    data-selected={selectedName === name || undefined}
                    onClick={() => choosePack(name)}
                  >
                    <span>{name}</span>
                    <small>{(packsByName.get(name) ?? []).length} versions</small>
                  </button>
                ))}
              </div>
            </div>

            <FormField label={t('pages:hooks.rulePacks.colVersion', 'Version')}>
              <select
                aria-label={t('pages:hooks.rulePacks.colVersion', 'Version')}
                className={styles.select}
                value={selectedVersion}
                onChange={(e) => setSelectedVersion(e.target.value)}
                disabled={selectedName === ''}
              >
                <option value="">
                  {t('pages:hooks.rulePacks.bindChooseVersion', 'Select a version')}
                </option>
                {versions.map((pack) => (
                  <option key={pack.id} value={pack.version}>
                    {pack.version}
                  </option>
                ))}
              </select>
            </FormField>

            <label className={styles.enabledRow}>
              <Switch
                checked={enabled}
                onCheckedChange={setEnabled}
                aria-label={t('pages:hooks.rulePacks.bindEnabled', 'Enabled')}
              />
              <span>{t('pages:hooks.rulePacks.bindEnabled', 'Enabled')}</span>
            </label>

            <div className={styles.actions}>
              <Button variant="secondary" onClick={handleClose}>
                {t('common:cancel', 'Cancel')}
              </Button>
              <Button
                onClick={handleSubmit}
                loading={saving}
                disabled={selectedName === '' || selectedVersion === ''}
              >
                {t('pages:hooks.rulePacks.bindButton', 'Bind')}
              </Button>
            </div>
          </>
        )}
      </Stack>
    </Dialog>
  );
}

