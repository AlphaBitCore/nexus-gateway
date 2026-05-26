import { type FormEvent, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';

import {
  rulePacksApi,
  type RulePack,
  type RulePackCreateInput,
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

const DEFAULT_RULES_JSON = serializeRules([
  {
    ruleId: 'example-email',
    category: 'pii',
    severity: 'hard',
    pattern: '[\\w.+-]+@[\\w-]+\\.[\\w.-]+',
    description: 'Blocks email addresses',
    labels: ['pii:email'],
  },
]);

export function RulePackCreatePage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [version, setVersion] = useState('');
  const [maintainer, setMaintainer] = useState('');
  const [description, setDescription] = useState('');
  const [rulesRaw, setRulesRaw] = useState(DEFAULT_RULES_JSON);
  const [rulesMode, setRulesMode] = useState<'json' | 'form'>('json');
  const [ruleDrafts, setRuleDrafts] = useState<RuleDraft[]>(
    () => rulesToDrafts(parseRules(DEFAULT_RULES_JSON).rules ?? []),
  );
  const [formError, setFormError] = useState<string | null>(null);

  const parsedJson = useMemo(() => parseRules(rulesRaw), [rulesRaw]);
  const parsedForm = useMemo(() => draftsToRules(ruleDrafts), [ruleDrafts]);
  const rulesValidation = rulesMode === 'json' ? parsedJson : parsedForm;

  const { mutate: createPack, loading: creating } = useMutation<RulePackCreateInput, RulePack>(
    (body) => rulePacksApi.create(body),
    {
      invalidateQueries: [['admin', 'rule-packs', 'list']],
      successMessage: t('pages:hooks.rulePacks.createSuccess', 'Rule pack created'),
      onSuccess: () => navigate('/compliance/rule-packs'),
    },
  );

  async function onSubmit() {
    setFormError(null);
    if (name.trim() === '') {
      setFormError(t('pages:hooks.rulePacks.createNameRequired', 'Name is required'));
      return;
    }
    if (version.trim() === '') {
      setFormError(t('pages:hooks.rulePacks.createVersionRequired', 'Version is required'));
      return;
    }
    if (maintainer.trim() === '') {
      setFormError(t('pages:hooks.rulePacks.createMaintainerRequired', 'Maintainer is required'));
      return;
    }
    if (rulesValidation.error || !rulesValidation.rules) {
      setFormError(rulesValidation.error ?? 'Rules JSON invalid');
      return;
    }
    await createPack({
      name: name.trim(),
      version: version.trim(),
      maintainer: maintainer.trim(),
      description: description.trim() === '' ? undefined : description.trim(),
      rules: rulesValidation.rules,
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
    setRuleDrafts(rulesToDrafts(converted.rules));
    setFormError(null);
    setRulesMode('form');
  }

  const canSubmit =
    name.trim() !== '' &&
    version.trim() !== '' &&
    maintainer.trim() !== '' &&
    rulesValidation.rules !== null &&
    rulesValidation.error === null;

  return (
    <Stack gap="lg">
      <div className={styles.header}>
        <h1 className={styles.title}>{t('pages:hooks.rulePacks.createTitle', 'Create Rule Pack')}</h1>
        <p className={styles.subtitle}>
          {t(
            'pages:hooks.rulePacks.createSubtitle',
            'Author a new rule pack with metadata and regex rules. Use Import YAML for pack authors who already produce YAML artifacts.',
          )}
        </p>
      </div>

      <Card>
        <form onSubmit={onFormSubmit}>
          <Stack gap="md">
          <div className={styles.row}>
            <FormField label={t('pages:hooks.rulePacks.colName', 'Name')} required>
              <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="acme/rules" />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colVersion', 'Version')} required>
              <Input value={version} onChange={(e) => setVersion(e.target.value)} placeholder="v1.0.0" />
            </FormField>
          </div>
          <FormField label={t('pages:hooks.rulePacks.colMaintainer', 'Maintainer')} required>
            <Input value={maintainer} onChange={(e) => setMaintainer(e.target.value)} placeholder="customer" />
          </FormField>
          <FormField
            label={t('pages:hooks.rulePacks.colDescription', 'Description')}
            helpText={t(
              'pages:hooks.rulePacks.createDescriptionHelp',
              'Optional. Shown to operators browsing the catalog.',
            )}
          >
            <Input value={description} onChange={(e) => setDescription(e.target.value)} />
          </FormField>
          <Stack gap="sm">
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
          </Stack>

          {formError && <ErrorBanner message={formError} />}

          <div className={styles.actions}>
            <Button variant="secondary" type="button" onClick={() => navigate('/compliance/rule-packs')}>
              {t('common:cancel', 'Cancel')}
            </Button>
            <Button type="submit" loading={creating} disabled={!canSubmit}>
              {t('pages:hooks.rulePacks.createButton', 'Create pack')}
            </Button>
          </div>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
