import { type FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';

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

import { RuleDraftsEditor } from './RulePackEditPage.RuleDraftsEditor';
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
  const descriptionRef = useRef<HTMLTextAreaElement | null>(null);

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

  useEffect(() => {
    const textarea = descriptionRef.current;
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = `${Math.min(textarea.scrollHeight, 400)}px`;
  }, [description]);

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
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link
            to={`/compliance/rule-packs/${id}`}
            className={styles.backLink}
            aria-label={t('common:back', 'Back')}
          >
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.title}>{t('pages:hooks.rulePacks.editTitle', 'Edit Rule Pack')}</h1>
            <p className={styles.subtitle}>
              {t(
                'pages:hooks.rulePacks.editSubtitle',
                'Update maintainer metadata and rule definitions. Name and version are immutable.',
              )}
            </p>
          </div>
        </div>
      </section>

      <form className={styles.form} onSubmit={onFormSubmit}>
        <Card>
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
            <Textarea
              ref={descriptionRef}
              className={styles.autoGrowTextarea}
              value={description}
              rows={1}
              onChange={(e) => setDescription(e.target.value)}
            />
          </FormField>

          <div className={styles.rulesToolbar}>
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
            {rulesMode === 'form' && (
              <button type="button" onClick={addRule} className={styles.addRuleButton}>
                + {t('pages:hooks.rulePacks.addRule', 'Add rule')}
              </button>
            )}
          </div>

          {rulesMode === 'json' && (
            <FormField
              label={t('pages:hooks.rulePacks.createRulesLabel', 'Rules (JSON array)')}
              error={parsedJson.error ?? undefined}
              helpText={t(
                'pages:hooks.rulePacks.createRulesHelp',
                'Each rule requires ruleId, category, severity (hard|soft|warn), pattern. Optional: flags, description, labels.',
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
            <RuleDraftsEditor
              ruleDrafts={ruleDrafts}
              updateDraft={updateDraft}
              removeRule={removeRule}
            />
          )}

          {formError && <ErrorBanner message={formError} />}
          </Stack>
        </Card>
        <div className={styles.actions}>
          <Button variant="secondary" type="button" onClick={() => navigate(`/compliance/rule-packs/${id}`)}>
            {t('common:cancel', 'Cancel')}
          </Button>
          <Button type="submit" loading={saving} disabled={!canSubmit}>
            {t('common:save', 'Save')}
          </Button>
        </div>
      </form>
    </Stack>
  );
}
