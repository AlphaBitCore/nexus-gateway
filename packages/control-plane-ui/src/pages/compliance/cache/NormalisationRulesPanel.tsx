import { Fragment, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { cacheApi, type CacheAdaptersList } from '@/api/services/system/cache';
import { Badge, Card, ErrorBanner, Input, Skeleton, Stack, Switch } from '@/components/ui';
import styles from './SettingsCacheTab.module.css';

/**
 * Bundled rule metadata, mirroring `packages/shared/normaliser/bundled.go`
 * one-for-one. Order in this array is the order rules appear on screen.
 * The DB only stores admin overrides (enabled, dry_run_always) keyed by rule_id;
 * regex / body_path / default-enabled live in Go.
 */
const BUNDLED_RULES: {
  id: string;
  adapterType: string;
  defaultEnabled: boolean;
  keyNormalizeSafe: boolean;
  /** i18n key under `pages:settings.promptCache.ruleDesc.*`. */
  descriptionKey: string;
}[] = [
  // Anthropic family — surgical billing-nonce strips (opt-in, off by default).
  { id: 'claude-code-cch-strip',           adapterType: 'anthropic',    defaultEnabled: false, keyNormalizeSafe: true, descriptionKey: 'claudeCodeCchStrip' },
  { id: 'bedrock-claude-cch-strip',        adapterType: 'bedrock',      defaultEnabled: false, keyNormalizeSafe: true, descriptionKey: 'bedrockClaudeCchStrip' },
];

/**
 * Display order of adapter groups in the rules panel. Anthropic family first
 * (it has the most operationally-interesting rules — defaults are off, so
 * admins look here first), then OpenAI-compat alphabetised.
 */
const ADAPTER_GROUP_ORDER: string[] = [
  'anthropic',
  'bedrock',
];

/**
 * Lists every bundled normalisation rule across every adapter family in one
 * flat table, grouped by adapter. Reads all Tier-2 rows in a single batch
 * call and writes per-rule toggles back through the per-adapter PUT route.
 */
export function NormalisationRulesPanel() {
  const { t } = useTranslation();
  const [query, setQuery] = useState('');

  const { data, loading, error, refetch } = useApi<CacheAdaptersList>(
    () => cacheApi.listAdapters(),
    ['admin', 'cache', 'adapters', 'list'],
  );

  const rowsByAdapter = useMemo(() => {
    const q = query.trim().toLowerCase();
    const filtered = q
      ? BUNDLED_RULES.filter(r => r.id.toLowerCase().includes(q) || r.adapterType.toLowerCase().includes(q))
      : BUNDLED_RULES;
    const grouped: Record<string, typeof BUNDLED_RULES> = {};
    for (const r of filtered) {
      (grouped[r.adapterType] ||= []).push(r);
    }
    return grouped;
  }, [query]);

  const adaptersInOrder = ADAPTER_GROUP_ORDER.filter(a => rowsByAdapter[a]?.length);

  const setRule = async (adapterType: string, ruleId: string, enabled: boolean) => {
    // Merge into the existing adapter row so unrelated knobs are not wiped.
    const existing = data?.items?.[adapterType] ?? {};
    const overrides = existing.rules ?? {};
    const nextRules = { ...overrides, [ruleId]: { ...overrides[ruleId], enabled } };
    await cacheApi.putAdapter(adapterType, { ...existing, rules: nextRules });
    refetch();
  };

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.rulesTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.rulesSubtitle')}</p>

        <div className={styles.rulesFilterBar}>
          <Input
            className={styles.rulesFilterInput}
            placeholder={t('pages:settings.promptCache.rulesFilterPlaceholder')}
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
          <span className={styles.subtitle}>
            {t('pages:settings.promptCache.rulesCount', {
              shown: Object.values(rowsByAdapter).reduce((n, list) => n + list.length, 0),
              total: BUNDLED_RULES.length,
            })}
          </span>
        </div>

        <table className={styles.rulesTable}>
          <colgroup>
            <col className={styles.colRuleId} />
            <col className={styles.colKeySafe} />
            <col className={styles.colStatus} />
          </colgroup>
          <thead>
            <tr>
              <th>{t('pages:settings.promptCache.ruleId')}</th>
              <th>{t('pages:settings.promptCache.ruleKeySafe')}</th>
              <th>{t('pages:settings.promptCache.ruleStatus')}</th>
            </tr>
          </thead>
          <tbody>
            {adaptersInOrder.map(adapter => {
              const rules = rowsByAdapter[adapter];
              const adapterRow = data?.items?.[adapter];
              const overrides = adapterRow?.rules ?? {};
              return (
                <Fragment key={adapter}>
                  <tr className={styles.adapterGroup}>
                    <td colSpan={3}>{t(`pages:settings.promptCache.adapterGroup.${adapter}`, adapter)}</td>
                  </tr>
                  {rules.map(rule => {
                    const ovr = overrides[rule.id]?.enabled;
                    const effective = ovr ?? rule.defaultEnabled;
                    return (
                      <tr key={rule.id}>
                        <td>
                          <code>{rule.id}</code>
                          <div className={styles.ruleDescription}>
                            {t(`pages:settings.promptCache.ruleDesc.${rule.descriptionKey}`)}
                          </div>
                        </td>
                        <td>
                          {rule.keyNormalizeSafe
                            ? <Badge variant="success">{t('common:yes')}</Badge>
                            : <Badge variant="default">{t('common:no')}</Badge>}
                        </td>
                        <td>
                          <Switch checked={effective} onCheckedChange={v => setRule(rule.adapterType, rule.id, v)} />
                        </td>
                      </tr>
                    );
                  })}
                </Fragment>
              );
            })}
            {adaptersInOrder.length === 0 && (
              <tr>
                <td colSpan={3} className={styles.empty}>
                  {t('pages:settings.promptCache.rulesNoMatch')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </Stack>
    </Card>
  );
}
