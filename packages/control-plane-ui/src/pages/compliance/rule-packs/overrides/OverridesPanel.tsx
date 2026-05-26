import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type EffectiveRuleSet, type RulePackOverride } from '@/api/services';
import { Button, Card, ErrorBanner, Stack } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import styles from './OverridesPanel.module.css';

type DraftRow = {
  disabled: boolean;
  severityOverride: string;
};

const SEVERITY_OPTIONS = ['', 'info', 'soft', 'hard'] as const;

function buildBaseline(data: EffectiveRuleSet | null): Record<string, DraftRow> {
  const out: Record<string, DraftRow> = {};
  for (const rule of data?.pack.rules ?? []) {
    const key = rule.ruleId || rule.id || '';
    if (!key) continue;
    out[key] = { disabled: false, severityOverride: '' };
  }
  return out;
}

export interface OverridesPanelProps {
  installId: string;
}

export function OverridesPanel({ installId }: OverridesPanelProps) {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<EffectiveRuleSet>(
    () => rulePacksApi.effectiveRules(installId),
    ['admin', 'rule-pack-installs', 'effective', installId],
  );

  const baseline = useMemo(() => buildBaseline(data), [data]);
  const [draft, setDraft] = useState<Record<string, DraftRow>>({});

  const merged = useMemo(() => {
    const next: Record<string, DraftRow> = {};
    for (const [ruleId, row] of Object.entries(baseline)) {
      next[ruleId] = draft[ruleId] ?? row;
    }
    return next;
  }, [baseline, draft]);

  const { mutate: saveOverrides, loading: saving, error: saveError } = useMutation(
    async (overrides: RulePackOverride[]) => rulePacksApi.upsertOverrides(installId, overrides),
    {
      invalidateQueries: [['admin', 'rule-pack-installs', 'effective', installId]],
      successMessage: t('pages:hooks.rulePacks.overridesSaved', 'Overrides saved'),
      onSuccess: () => {
        setDraft({});
      },
    },
  );

  function setDisabled(ruleId: string, value: boolean) {
    setDraft((current) => ({
      ...current,
      [ruleId]: {
        ...(current[ruleId] ?? baseline[ruleId] ?? { disabled: false, severityOverride: '' }),
        disabled: value,
      },
    }));
  }

  function setSeverityOverride(ruleId: string, value: string) {
    setDraft((current) => ({
      ...current,
      [ruleId]: {
        ...(current[ruleId] ?? baseline[ruleId] ?? { disabled: false, severityOverride: '' }),
        severityOverride: value,
      },
    }));
  }

  async function handleSave() {
    const changed: RulePackOverride[] = [];
    for (const [ruleId, row] of Object.entries(merged)) {
      const original = baseline[ruleId] ?? { disabled: false, severityOverride: '' };
      if (row.disabled !== original.disabled || row.severityOverride !== original.severityOverride) {
        changed.push({
          ruleLocalId: ruleId,
          disabled: row.disabled,
          severityOverride: row.severityOverride || undefined,
        });
      }
    }
    await saveOverrides(changed);
  }

  function handleResetAll() {
    setDraft({});
  }

  const changedCount = useMemo(() => {
    let count = 0;
    for (const [ruleId, row] of Object.entries(merged)) {
      const original = baseline[ruleId] ?? { disabled: false, severityOverride: '' };
      if (row.disabled !== original.disabled || row.severityOverride !== original.severityOverride) {
        count += 1;
      }
    }
    return count;
  }, [baseline, merged]);

  if (loading) return <div className={styles.state}>{t('common:loading', 'Loading…')}</div>;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  return (
    <Card>
      <Stack gap="md">
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('pages:hooks.rulePacks.overridesTitle', 'Overrides')}</h2>
            <p className={styles.subtitle}>
              {t(
                'pages:hooks.rulePacks.overridesSubtitle',
                'Adjust disabled state and severity overrides before saving this install.',
              )}
            </p>
            <p className={styles.subtitle}>
              {t(
                'pages:hooks.rulePacks.overridesPerInstallScope',
                'Overrides apply only to this install. Other installs on the same hook, or the same pack bound elsewhere, keep their own override rows.',
              )}
            </p>
          </div>
          <div className={styles.actions}>
            <Button variant="secondary" onClick={handleResetAll} disabled={changedCount === 0}>
              {t('pages:hooks.rulePacks.resetAll', 'Reset all')}
            </Button>
            <Button onClick={handleSave} loading={saving} disabled={changedCount === 0}>
              {t('common:save', 'Save')}
            </Button>
          </div>
        </div>

        {saveError && <ErrorBanner message={saveError.message} />}

        <div className={styles.tableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('pages:hooks.rulePacks.colRuleId', 'Rule ID')}</th>
                <th>{t('pages:hooks.rulePacks.colSeverity', 'Severity')}</th>
                <th>{t('pages:hooks.rulePacks.colDisabled', 'Disabled')}</th>
                <th>{t('pages:hooks.rulePacks.colSeverityOverride', 'Severity override')}</th>
              </tr>
            </thead>
            <tbody>
              {data.pack.rules.map((rule) => {
                const ruleId = rule.ruleId || rule.id || '';
                const row = merged[ruleId] ?? { disabled: false, severityOverride: '' };
                return (
                  <tr key={ruleId}>
                    <td>{ruleId}</td>
                    <td>{rule.severity}</td>
                    <td>
                      <input
                        type="checkbox"
                        checked={row.disabled}
                        onChange={(e) => setDisabled(ruleId, e.target.checked)}
                        aria-label={t('pages:hooks.rulePacks.colDisabled', 'Disabled')}
                      />
                    </td>
                    <td>
                      <select
                        className={styles.select}
                        value={row.severityOverride}
                        onChange={(e) => setSeverityOverride(ruleId, e.target.value)}
                        aria-label={`${ruleId} ${t('pages:hooks.rulePacks.colSeverityOverride', 'Severity override')}`}
                      >
                        {SEVERITY_OPTIONS.map((option) => (
                          <option key={option || 'default'} value={option}>
                            {option || t('pages:hooks.rulePacks.noOverride', 'No override')}
                          </option>
                        ))}
                      </select>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Stack>
    </Card>
  );
}

