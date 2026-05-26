import { type FormEvent, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';

import {
  rulePacksApi,
  type RulePack,
  type RulePackUpdateInput,
} from '@/api/services';
import {
  Button,
  Card,
  ErrorBanner,
  FormField,
  Input,
  Stack,
  Textarea,
} from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import styles from './RulePackCreatePage.module.css';
import {
  draftsToRules,
  emptyRuleDraft,
  parseRules,
  rulesToDrafts,
  serializeRules,
  type RuleDraft,
} from './rulePackRules';

export function RulePackEditPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { id = '' } = useParams<{ id: string }>();
  const [maintainer, setMaintainer] = useState('');
  const [description, setDescription] = useState('');
  const [signature, setSignature] = useState('');
  const [rulesRaw, setRulesRaw] = useState('[]');
  const [rulesMode, setRulesMode] = useState<'json' | 'form'>('json');
  const [ruleDrafts, setRuleDrafts] = useState<RuleDraft[]>([emptyRuleDraft()]);
  const [formError, setFormError] = useState<string | null>(null);

  const { data, loading, error, refetch } = useApi<RulePack>(
    () => rulePacksApi.get(id),
    ['admin', 'rule-packs', 'detail', id],
    { skip: id === '' },
  );

  useEffect(() => {
    if (!data) return;
    setMaintainer(data.maintainer);
    setDescription(data.description ?? '');
    setSignature(data.signature ?? '');
    setRulesRaw(serializeRules(data.rules));
    setRuleDrafts(data.rules.length > 0 ? rulesToDrafts(data.rules) : [emptyRuleDraft()]);
    setFormError(null);
  }, [data]);

  const parsedJson = useMemo(() => parseRules(rulesRaw), [rulesRaw]);
  const parsedForm = useMemo(() => draftsToRules(ruleDrafts), [ruleDrafts]);
  const rulesValidation = rulesMode === 'json' ? parsedJson : parsedForm;

  const { mutate: updatePack, loading: saving } = useMutation<
    { id: string; body: RulePackUpdateInput },
    RulePack
  >(({ id: packId, body }) => rulePacksApi.update(packId, body), {
    invalidateQueries: id
      ? [
          ['admin', 'rule-packs', 'list'],
          ['admin', 'rule-packs', 'detail', id],
        ]
      : [['admin', 'rule-packs', 'list']],
    successMessage: t('pages:hooks.rulePacks.updateSuccess', 'Rule pack updated'),
    onSuccess: () => navigate(`/compliance/rule-packs/${id}`),
  });

  async function onSubmit() {
    if (!id) return;
    setFormError(null);
    if (rulesValidation.error || !rulesValidation.rules) {
      setFormError(rulesValidation.error ?? 'Rules JSON invalid');
      return;
    }
    await updatePack({
      id,
      body: {
        maintainer: maintainer.trim() === '' ? undefined : maintainer.trim(),
        description: description.trim() === '' ? undefined : description.trim(),
        signature: signature.trim() === '' ? undefined : signature.trim(),
        rules: rulesValidation.rules,
      },
    });
  }

  function onFormSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void onSubmit();
  }

  function updateDraft(index: number, key: keyof RuleDraft, value: string) {
    setRuleDrafts((current) =>
      current.map((item, itemIndex) => (itemIndex === index ? { ...item, [key]: value } : item)),
    );
  }

  function addRule() {
    setRuleDrafts((current) => [...current, emptyRuleDraft()]);
  }

  function removeRule(index: number) {
    setRuleDrafts((current) => current.filter((_item, itemIndex) => itemIndex !== index));
  }

  function switchRulesMode(nextMode: 'json' | 'form') {
    if (nextMode === rulesMode) return;
    if (nextMode === 'json') {
      const converted = draftsToRules(ruleDrafts);
      if (converted.error || !converted.rules) {
        setFormError(converted.error ?? 'Rules JSON invalid');
        return;
      }
      setRulesRaw(serializeRules(converted.rules));
      setFormError(null);
      setRulesMode('json');
      return;
    }
    const converted = parseRules(rulesRaw);
    if (converted.error || !converted.rules) {
      setFormError(converted.error ?? 'Rules JSON invalid');
      return;
    }
    setRuleDrafts(converted.rules.length > 0 ? rulesToDrafts(converted.rules) : [emptyRuleDraft()]);
    setFormError(null);
    setRulesMode('form');
  }

  if (loading) return <div>{t('common:loading', 'Loading…')}</div>;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return <div>{t('pages:hooks.rulePacks.notFound', 'Rule pack not found.')}</div>;

  const canSubmit = rulesValidation.rules !== null && rulesValidation.error === null;

  return (
    <Stack gap="lg">
      <div className={styles.header}>
        <h1 className={styles.title}>{t('pages:hooks.rulePacks.editTitle', 'Edit Rule Pack')}</h1>
        <p className={styles.subtitle}>
          {t(
            'pages:hooks.rulePacks.editSubtitle',
            'Update maintainer metadata and rule definitions. Name and version are immutable.',
          )}
        </p>
      </div>

      <Card>
        <form onSubmit={onFormSubmit}>
          <Stack gap="md">
          <div className={styles.row}>
            <FormField label={t('pages:hooks.rulePacks.colName', 'Name')}>
              <Input value={data.name} disabled />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colVersion', 'Version')}>
              <Input value={data.version} disabled />
            </FormField>
          </div>
          <div className={styles.row}>
            <FormField label={t('pages:hooks.rulePacks.colMaintainer', 'Maintainer')}>
              <Input value={maintainer} onChange={(e) => setMaintainer(e.target.value)} />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colSignature', 'Signature')}>
              <Input value={signature} onChange={(e) => setSignature(e.target.value)} />
            </FormField>
          </div>
          <FormField label={t('pages:hooks.rulePacks.colDescription', 'Description')}>
            <Input value={description} onChange={(e) => setDescription(e.target.value)} />
          </FormField>

          <div className={styles.modeSwitch}>
            <Button
              variant={rulesMode === 'form' ? 'primary' : 'secondary'}
              size="sm"
              type="button"
              onClick={() => switchRulesMode('form')}
            >
              {t('pages:hooks.rulePacks.formMode', 'Form mode')}
            </Button>
            <Button
              variant={rulesMode === 'json' ? 'primary' : 'secondary'}
              size="sm"
              type="button"
              onClick={() => switchRulesMode('json')}
            >
              {t('pages:hooks.rulePacks.jsonMode', 'JSON mode')}
            </Button>
          </div>

          {rulesMode === 'json' && (
            <FormField
              label={t('pages:hooks.rulePacks.createRulesLabel', 'Rules (JSON array)')}
              error={parsedJson.error ?? undefined}
              helpText={t(
                'pages:hooks.rulePacks.createRulesHelp',
                'Each rule requires ruleId, category, severity (hard|soft|info), pattern. Optional: flags, description, labels.',
              )}
            >
              <Textarea
                className={styles.textarea}
                value={rulesRaw}
                rows={14}
                onChange={(e) => setRulesRaw(e.target.value)}
              />
            </FormField>
          )}

          {rulesMode === 'form' && (
            <Stack gap="md">
              {ruleDrafts.map((rule, index) => (
                <div key={`draft-${index + 1}`} className={styles.ruleCard}>
                  <div className={styles.ruleCardHeader}>
                    <strong>{t('pages:hooks.rulePacks.ruleItemTitle', 'Rule')} #{index + 1}</strong>
                    <Button variant="ghost" size="sm" type="button" onClick={() => removeRule(index)}>
                      {t('common:delete', 'Delete')}
                    </Button>
                  </div>
                  <div className={styles.row}>
                    <FormField label={t('pages:hooks.rulePacks.colRuleId', 'Rule ID')} required>
                      <Input
                        value={rule.ruleId}
                        onChange={(e) => updateDraft(index, 'ruleId', e.target.value)}
                      />
                    </FormField>
                    <FormField label={t('pages:hooks.rulePacks.colCategory', 'Category')} required>
                      <Input
                        value={rule.category}
                        onChange={(e) => updateDraft(index, 'category', e.target.value)}
                      />
                    </FormField>
                  </div>
                  <div className={styles.row}>
                    <FormField label={t('pages:hooks.rulePacks.colSeverity', 'Severity')} required>
                      <Input
                        value={rule.severity}
                        onChange={(e) => updateDraft(index, 'severity', e.target.value)}
                      />
                    </FormField>
                    <FormField label={t('pages:hooks.rulePacks.colPattern', 'Pattern')} required>
                      <Input
                        value={rule.pattern}
                        onChange={(e) => updateDraft(index, 'pattern', e.target.value)}
                      />
                    </FormField>
                  </div>
                  <div className={styles.row}>
                    <FormField label={t('pages:hooks.rulePacks.colFlags', 'Flags')}>
                      <Input value={rule.flags} onChange={(e) => updateDraft(index, 'flags', e.target.value)} />
                    </FormField>
                    <FormField label={t('pages:hooks.rulePacks.colLabels', 'Labels (comma-separated)')}>
                      <Input value={rule.labels} onChange={(e) => updateDraft(index, 'labels', e.target.value)} />
                    </FormField>
                  </div>
                  <FormField label={t('pages:hooks.rulePacks.colDescription', 'Description')}>
                    <Input
                      value={rule.description}
                      onChange={(e) => updateDraft(index, 'description', e.target.value)}
                    />
                  </FormField>
                </div>
              ))}
              <div>
                <Button variant="secondary" type="button" onClick={addRule}>
                  {t('pages:hooks.rulePacks.addRule', 'Add rule')}
                </Button>
              </div>
            </Stack>
          )}

          {formError && <ErrorBanner message={formError} />}

          <div className={styles.actions}>
            <Button variant="secondary" type="button" onClick={() => navigate(`/compliance/rule-packs/${id}`)}>
              {t('common:cancel', 'Cancel')}
            </Button>
            <Button type="submit" loading={saving} disabled={!canSubmit}>
              {t('common:save', 'Save')}
            </Button>
          </div>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
