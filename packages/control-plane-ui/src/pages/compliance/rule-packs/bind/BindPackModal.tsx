import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type RulePackInstall, type RulePackMeta } from '@/api/services';
import {
  Button,
  Dialog,
  ErrorBanner,
  FormField,
  MultiSelectDropdown,
  Stack,
  Switch,
} from '@/components/ui';
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
  const [selectedNames, setSelectedNames] = useState<string[]>([]);
  const [enabled, setEnabled] = useState(true);

  const { data, loading, error } = useApi<RulePackMeta[]>(
    () => rulePacksApi.list(),
    ['admin', 'rule-packs', 'bind-modal'],
    { skip: !open },
  );

  // Group packs by family name; each family's versions are sorted newest-first,
  // so [0] is the latest. Multi-bind installs each selected family at its latest
  // version (per-version pinning is a separate, not-yet-implemented concern).
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

  const options = useMemo(
    () =>
      Array.from(packsByName.entries())
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([name, versions]) => ({
          value: name,
          label:
            versions.length > 1
              ? t('pages:hooks.rulePacks.bindOptionMulti', {
                  defaultValue: '{{name}} · latest {{version}} ({{count}} versions)',
                  name,
                  version: versions[0]?.version ?? '',
                  count: versions.length,
                })
              : t('pages:hooks.rulePacks.bindOptionSingle', {
                  defaultValue: '{{name}} · {{version}}',
                  name,
                  version: versions[0]?.version ?? '',
                }),
        })),
    [packsByName, t],
  );

  const { mutate: installPacks, loading: saving, error: saveError } = useMutation(
    async (names: string[]) => {
      // The install endpoint binds one pack per call; bind each selected family
      // at its latest version. Done sequentially so a mid-list failure surfaces
      // with the packs already bound left in place (idempotent re-bind is safe).
      const installs: RulePackInstall[] = [];
      for (const name of names) {
        const latest = packsByName.get(name)?.[0];
        if (!latest) continue;
        installs.push(
          await rulePacksApi.install(hookId, {
            packId: latest.id,
            pinVersion: latest.version,
            enabled,
          }),
        );
      }
      return installs;
    },
    {
      successMessage: t('pages:hooks.rulePacks.bindSuccess', 'Rule packs installed'),
      onSuccess: (installs) => {
        for (const install of installs) onBound(install);
        handleClose();
      },
    },
  );

  function handleClose() {
    setSelectedNames([]);
    setEnabled(true);
    onClose();
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) handleClose();
      }}
      title={t('pages:hooks.rulePacks.bindTitle', 'Bind Rule Packs')}
      description={t(
        'pages:hooks.rulePacks.bindSubtitle',
        'Search and select one or more rule packs to install onto the current hook (each is bound at its latest version).',
      )}
      size="lg"
    >
      <Stack gap="md">
        {loading && <div className={styles.state}>{t('common:loading', 'Loading…')}</div>}
        {error && <ErrorBanner message={error.message} />}
        {saveError && <ErrorBanner message={saveError.message} />}

        {!loading && !error && (
          <>
            <FormField label={t('pages:hooks.rulePacks.bindPack', 'Rule packs')}>
              <MultiSelectDropdown
                label={t('pages:hooks.rulePacks.bindPack', 'Rule packs')}
                options={options}
                value={selectedNames}
                onChange={setSelectedNames}
                searchable
                searchPlaceholder={t(
                  'pages:hooks.rulePacks.bindSearchPlaceholder',
                  'Search rule packs…',
                )}
                emptyLabel={t('pages:hooks.rulePacks.bindEmptyLabel', 'Select rule packs')}
              />
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
                onClick={() => installPacks(selectedNames)}
                loading={saving}
                disabled={selectedNames.length === 0}
              >
                {t('pages:hooks.rulePacks.bindButtonMulti', {
                  defaultValue: 'Bind {{count}} pack(s)',
                  count: selectedNames.length,
                })}
              </Button>
            </div>
          </>
        )}
      </Stack>
    </Dialog>
  );
}
