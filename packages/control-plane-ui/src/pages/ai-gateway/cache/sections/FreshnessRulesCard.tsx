/**
 * Card C — Freshness rules (fleet-wide rule editor).
 *
 * Pre-cache prompt filter: when the user prompt matches one of these patterns,
 * gateway cache is skipped so the response is fetched fresh from upstream.
 * Default 11 rules cover time-sensitive categories (weather, stock-price,
 * exchange-rate, news, score, crypto-price, oil-price, interest-rate,
 * inflation, exchange-rate, population).
 *
 * Displays the rule list with toggles, Edit + Delete actions per row,
 * an Add Rule modal, and a test box where admins can dry-run a prompt.
 */
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  ErrorBanner,
  FormField,
  Input,
  Skeleton,
  Stack,
  Switch,
  Tooltip,
} from '@/components/ui';
import {
  timeSensitivePatternsApi,
  type TimeSensitivePattern,
} from '@/api/services/cache/timeSensitivePatterns';
import {
  extractCacheConfigApi,
  type ExtractCacheConfig,
} from '@/api/services/cache/extractCacheConfig';
import styles from './FreshnessRulesCard.module.css';

export function FreshnessRulesCard() {
  const { t } = useTranslation();
  const canUpdateExtract = usePermission('extract-cache:update');

  const { data, loading, error, refetch } = useApi(
    () => timeSensitivePatternsApi.list(),
    ['admin', 'cache', 'time-sensitive-patterns'],
  );

  // Read the extract_cache_config singleton — the apply_freshness_rules gate
  // lives on that row but is surfaced here because admins naturally look for
  // it next to the rule editor ("do my rules actually fire?").
  const { data: extractCfg, refetch: refetchExtract } = useApi<ExtractCacheConfig>(
    () => extractCacheConfigApi.getConfig(),
    ['admin', 'extract-cache', 'config'],
  );

  const [testPrompt, setTestPrompt] = useState('');
  const [testResult, setTestResult] = useState<string | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [showAddModal, setShowAddModal] = useState(false);
  const [editingRule, setEditingRule] = useState<TimeSensitivePattern | null>(null);
  const [deletingRule, setDeletingRule] = useState<TimeSensitivePattern | null>(null);

  // Local draft for the apply-freshness-rules toggle so the switch responds
  // immediately; the mutation flushes the change to the singleton row.
  const [applyDraft, setApplyDraft] = useState<boolean | null>(null);
  useEffect(() => {
    if (extractCfg) setApplyDraft(extractCfg.applyFreshnessRules);
  }, [extractCfg]);

  const { mutate: saveApply } = useMutation(
    (next: boolean) => {
      if (!extractCfg) throw new Error('extract config not loaded');
      return extractCacheConfigApi.saveConfig({
        enabled: extractCfg.enabled,
        ttlSeconds: extractCfg.ttlSeconds,
        applyFreshnessRules: next,
      });
    },
    {
      invalidateQueries: [['admin', 'extract-cache', 'config']],
      onSuccess: () => refetchExtract(),
      successMessage: t('pages:aiGateway.cache.freshness.applyToggleSaved'),
      errorMessage: t('pages:aiGateway.cache.freshness.applyToggleError'),
    },
  );

  const handleApplyToggle = (next: boolean) => {
    setApplyDraft(next);
    void saveApply(next);
  };

  const { mutate: toggleRule } = useMutation(
    ({ id, pattern }: { id: string; pattern: TimeSensitivePattern }) =>
      timeSensitivePatternsApi.update(id, pattern),
    {
      invalidateQueries: [['admin', 'cache', 'time-sensitive-patterns']],
      onSuccess: () => refetch(),
      successMessage: t('pages:aiGateway.cache.freshness.ruleSaved'),
      errorMessage: t('pages:aiGateway.cache.freshness.ruleSaveError'),
    },
  );

  const { mutate: deleteRule, loading: deleting } = useMutation(
    (id: string) => timeSensitivePatternsApi.delete(id),
    {
      invalidateQueries: [['admin', 'cache', 'time-sensitive-patterns']],
      onSuccess: () => {
        setDeletingRule(null);
        refetch();
      },
      successMessage: t('pages:aiGateway.cache.freshness.ruleDeleted'),
      errorMessage: t('pages:aiGateway.cache.freshness.ruleDeleteError'),
    },
  );

  const handleToggle = (pattern: TimeSensitivePattern, enabled: boolean) => {
    void toggleRule({ id: pattern.id, pattern: { ...pattern, enabled } });
  };

  const handleTest = async () => {
    if (!testPrompt.trim()) return;
    setTestLoading(true);
    setTestResult(null);
    try {
      const result = await timeSensitivePatternsApi.test(testPrompt.trim());
      if (result.decision === 'match') {
        setTestResult(
          t('pages:aiGateway.cache.freshness.testResultMatch', {
            ruleId: result.matchedRuleId ?? '?',
            keywords: (result.matchedKeywords ?? []).join(', '),
          }),
        );
      } else {
        setTestResult(t('pages:aiGateway.cache.freshness.testResultNoMatch'));
      }
    } catch {
      setTestResult(t('pages:aiGateway.cache.freshness.testResultError'));
    } finally {
      setTestLoading(false);
    }
  };

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const patterns = data?.patterns ?? [];

  return (
    <Card>
      <h3 id="card-freshness" className={styles.cardHeading}>
        {t('pages:aiGateway.cache.freshness.title')}
      </h3>
      <p className={styles.cardSubtitle}>
        {t('pages:aiGateway.cache.freshness.subtitle')}
      </p>

      {/* Apply gate: physically on extract_cache_config.applyFreshnessRules,
          surfaced here because this is where admins ask "do my rules work?". */}
      <div className={styles.applyToggleRow}>
        <span className={styles.applyToggleLabel}>
          {t('pages:aiGateway.cache.freshness.applyToggleLabel')}
          <Tooltip content={t('pages:aiGateway.cache.freshness.applyToggleTooltip')}>
            <span className={styles.applyHelpIcon} aria-label="More info">?</span>
          </Tooltip>
        </span>
        <Switch
          checked={applyDraft ?? false}
          onCheckedChange={handleApplyToggle}
          disabled={!canUpdateExtract || applyDraft === null}
          aria-label={t('pages:aiGateway.cache.freshness.applyToggleLabel')}
        />
      </div>

      <section aria-labelledby="card-freshness">
        <Stack gap="md">
          <table className={styles.rulesTable}>
            <colgroup>
              <col className={styles.colRuleId} />
              <col className={styles.colQuestionMark} />
              <col className={styles.colEntity} />
              <col className={styles.colLanguages} />
              <col className={styles.colStatus} />
              <col className={styles.colActions} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('pages:aiGateway.cache.freshness.colId')}</th>
                <th>{t('pages:aiGateway.cache.freshness.colQuestionMark')}</th>
                <th>{t('pages:aiGateway.cache.freshness.colEntity')}</th>
                <th>{t('pages:aiGateway.cache.freshness.colLanguages')}</th>
                <th>{t('pages:aiGateway.cache.freshness.colEnabled')}</th>
                <th>{t('pages:aiGateway.cache.freshness.colActions')}</th>
              </tr>
            </thead>
            <tbody>
              {patterns.map((p) => (
                <tr key={p.id}>
                  <td>
                    <code>{p.id}</code>
                    <div className={styles.ruleKeywords}>
                      {p.keywords.map((kw) => (
                        <Badge key={kw} variant="default" className={styles.keywordBadge}>
                          {kw}
                        </Badge>
                      ))}
                    </div>
                  </td>
                  <td>
                    {p.requireQuestionMark
                      ? <Badge variant="success">{t('common:yes')}</Badge>
                      : <Badge variant="default">{t('common:no')}</Badge>}
                  </td>
                  <td>
                    {p.requireEntity
                      ? <Badge variant="success">{t('common:yes')}</Badge>
                      : <Badge variant="default">{t('common:no')}</Badge>}
                  </td>
                  <td>
                    {(p.languages ?? []).join(', ') || t('pages:aiGateway.cache.freshness.allLanguages')}
                  </td>
                  <td>
                    <Switch
                      checked={p.enabled}
                      onCheckedChange={(v) => handleToggle(p, v)}
                      aria-label={t('pages:aiGateway.cache.freshness.toggleAriaLabel', { id: p.id })}
                    />
                  </td>
                  <td>
                    <Stack direction="horizontal" gap="sm">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setEditingRule(p)}
                        aria-label={t('pages:aiGateway.cache.freshness.editAriaLabel', { id: p.id })}
                      >
                        {t('common:edit')}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setDeletingRule(p)}
                        aria-label={t('pages:aiGateway.cache.freshness.deleteAriaLabel', { id: p.id })}
                      >
                        {t('common:delete')}
                      </Button>
                    </Stack>
                  </td>
                </tr>
              ))}
              {patterns.length === 0 && (
                <tr>
                  <td colSpan={6} className={styles.empty}>
                    {t('pages:aiGateway.cache.freshness.noRules')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>

          <div>
            <Button variant="secondary" onClick={() => setShowAddModal(true)}>
              {t('pages:aiGateway.cache.freshness.addRule')}
            </Button>
          </div>

          <div className={styles.testBox}>
            <h3 className={styles.testBoxTitle}>
              {t('pages:aiGateway.cache.freshness.testTitle')}
            </h3>
            <p className={styles.cardSubtitle}>
              {t('pages:aiGateway.cache.freshness.testSubtitle')}
            </p>
            <Stack direction="horizontal" gap="sm">
              <Input
                placeholder={t('pages:aiGateway.cache.freshness.testPlaceholder')}
                value={testPrompt}
                onChange={(e) => setTestPrompt(e.target.value)}
                className={styles.testInput}
              />
              <Button
                onClick={() => void handleTest()}
                disabled={testLoading || !testPrompt.trim()}
              >
                {testLoading
                  ? t('pages:aiGateway.cache.freshness.testRunning')
                  : t('pages:aiGateway.cache.freshness.testRun')}
              </Button>
            </Stack>
            {testResult && (
              <p className={styles.testResult} data-testid="test-result">
                {testResult}
              </p>
            )}
          </div>
        </Stack>
      </section>

      {(showAddModal || editingRule) && (
        <RuleModal
          initial={editingRule}
          onClose={() => {
            setShowAddModal(false);
            setEditingRule(null);
          }}
          onSaved={() => {
            setShowAddModal(false);
            setEditingRule(null);
            refetch();
          }}
        />
      )}

      <AlertDialog
        open={deletingRule !== null}
        onOpenChange={(open) => { if (!open) setDeletingRule(null); }}
        title={t('pages:aiGateway.cache.freshness.deleteTitle')}
        description={t('pages:aiGateway.cache.freshness.deleteDescription', { id: deletingRule?.id ?? '' })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deletingRule) void deleteRule(deletingRule.id); }}
        variant="danger"
        loading={deleting}
      />
    </Card>
  );
}

interface RuleModalProps {
  initial: TimeSensitivePattern | null;
  onClose: () => void;
  onSaved: () => void;
}

function RuleModal({ initial, onClose, onSaved }: RuleModalProps) {
  const { t } = useTranslation();
  const isEdit = initial !== null;
  const [id, setId] = useState(initial?.id ?? '');
  const [keywordsRaw, setKeywordsRaw] = useState((initial?.keywords ?? []).join(','));
  const [requireQuestionMark, setRequireQuestionMark] = useState(initial?.requireQuestionMark ?? false);
  const [requireEntity, setRequireEntity] = useState(initial?.requireEntity ?? false);
  const [languagesRaw, setLanguagesRaw] = useState((initial?.languages ?? ['en', 'zh']).join(','));
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [error, setError] = useState('');

  const { mutate: saveRule, loading: saving } = useMutation(
    async (pattern: TimeSensitivePattern): Promise<void> => {
      if (isEdit) {
        await timeSensitivePatternsApi.update(pattern.id, pattern);
      } else {
        await timeSensitivePatternsApi.create(pattern);
      }
    },
    {
      invalidateQueries: [['admin', 'cache', 'time-sensitive-patterns']],
      onSuccess: onSaved,
      successMessage: isEdit
        ? t('pages:aiGateway.cache.freshness.ruleSaved')
        : t('pages:aiGateway.cache.freshness.ruleCreated'),
      errorMessage: isEdit
        ? t('pages:aiGateway.cache.freshness.ruleSaveError')
        : t('pages:aiGateway.cache.freshness.ruleCreateError'),
    },
  );

  const handleSave = () => {
    setError('');
    const keywords = keywordsRaw.split(',').map((k) => k.trim()).filter(Boolean);
    const languages = languagesRaw.split(',').map((l) => l.trim()).filter(Boolean);
    if (!id.trim()) { setError(t('pages:aiGateway.cache.freshness.errorIdRequired')); return; }
    if (keywords.length === 0) { setError(t('pages:aiGateway.cache.freshness.errorKeywordsRequired')); return; }
    void saveRule({ id: id.trim(), keywords, requireQuestionMark, requireEntity, languages, enabled });
  };

  const titleKey = isEdit
    ? 'pages:aiGateway.cache.freshness.editRuleModalTitle'
    : 'pages:aiGateway.cache.freshness.addRuleModalTitle';

  return (
    <div className={styles.modalOverlay} role="dialog" aria-modal="true" aria-labelledby="rule-modal-title">
      <div className={styles.modal}>
        <h3 id="rule-modal-title" className={styles.modalTitle}>
          {t(titleKey)}
        </h3>
        <Stack gap="md">
          <FormField label={t('pages:aiGateway.cache.freshness.fieldId')}>
            <Input
              value={id}
              onChange={(e) => setId(e.target.value)}
              placeholder="my-custom-rule"
              disabled={isEdit}
            />
          </FormField>
          <FormField
            label={t('pages:aiGateway.cache.freshness.fieldKeywords')}
            helpText={t('pages:aiGateway.cache.freshness.fieldKeywordsHint')}
          >
            <Input value={keywordsRaw} onChange={(e) => setKeywordsRaw(e.target.value)} placeholder="trending,viral,breaking" />
          </FormField>
          <FormField label={t('pages:aiGateway.cache.freshness.fieldRequireQuestionMark')}>
            <Switch checked={requireQuestionMark} onCheckedChange={setRequireQuestionMark} />
          </FormField>
          <FormField label={t('pages:aiGateway.cache.freshness.fieldRequireEntity')}>
            <Switch checked={requireEntity} onCheckedChange={setRequireEntity} />
          </FormField>
          <FormField
            label={t('pages:aiGateway.cache.freshness.fieldLanguages')}
            helpText={t('pages:aiGateway.cache.freshness.fieldLanguagesHint')}
          >
            <Input value={languagesRaw} onChange={(e) => setLanguagesRaw(e.target.value)} placeholder="en,zh" />
          </FormField>
          <FormField label={t('pages:aiGateway.cache.freshness.fieldEnabled')}>
            <Switch checked={enabled} onCheckedChange={setEnabled} />
          </FormField>
          {error && <p className={styles.fieldError}>{error}</p>}
          <Stack direction="horizontal" gap="sm">
            <Button onClick={handleSave} disabled={saving}>
              {saving ? t('common:saving') : t('common:save')}
            </Button>
            <Button variant="secondary" onClick={onClose} disabled={saving}>
              {t('common:cancel')}
            </Button>
          </Stack>
        </Stack>
      </div>
    </div>
  );
}
