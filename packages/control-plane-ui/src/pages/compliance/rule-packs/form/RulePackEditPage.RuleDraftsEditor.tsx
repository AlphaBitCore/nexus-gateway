import { useTranslation } from 'react-i18next';

import { FormField, Input, Select, Stack } from '@/components/ui';
import { PatternPerfButton } from '@/components/PatternPerfButton';

import { SEVERITY_OPTIONS } from './severityOptions';
import styles from './RulePackCreatePage.module.css';
import { type RuleDraft } from './rulePackRules';

interface RuleDraftsEditorProps {
  ruleDrafts: RuleDraft[];
  updateDraft: (index: number, key: keyof RuleDraft, value: string) => void;
  removeRule: (index: number) => void;
}

export function RuleDraftsEditor({ ruleDrafts, updateDraft, removeRule }: RuleDraftsEditorProps) {
  const { t } = useTranslation();

  return (
    <Stack gap="md">
      {ruleDrafts.map((rule, index) => (
        <details key={`draft-${index + 1}`} className={styles.ruleCard} open>
          <summary className={styles.ruleCardHeader}>
            <span className={styles.ruleTitle}>
              <span className={styles.ruleChevron} aria-hidden />
              <strong>{t('pages:hooks.rulePacks.ruleItemTitle', 'Rule')} #{index + 1}</strong>
            </span>
            <button
              type="button"
              className={styles.deleteRuleButton}
              onClick={(event) => {
                event.preventDefault();
                event.stopPropagation();
                removeRule(index);
              }}
            >
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden>
                <path d="M3.5 4.5h9" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
                <path d="M6.5 2.5h3l.5 1h2v1h-8v-1h2l.5-1Z" fill="currentColor" />
                <path d="M5 6v6.5c0 .55.45 1 1 1h4c.55 0 1-.45 1-1V6" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
              </svg>
              {t('common:delete', 'Delete')}
            </button>
          </summary>
          <div className={styles.ruleFieldsGrid}>
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
            <FormField
              label={t('pages:hooks.rulePacks.colSeverity', 'Severity')}
              required
              tooltip={t(
                'pages:hooks.rulePacks.severityTip',
                'Severity is a classification signal, not the enforced action. The bound hook’s onMatch Action policy decides whether a match blocks, redacts, or is flagged.',
              )}
            >
              <Select
                value={rule.severity}
                onValueChange={(value) => updateDraft(index, 'severity', value)}
                options={SEVERITY_OPTIONS.map((opt) => ({
                  value: opt.value,
                  label: t(opt.labelKey, opt.fallback),
                }))}
              />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colPattern', 'Pattern')} required>
              <Input
                value={rule.pattern}
                onChange={(e) => updateDraft(index, 'pattern', e.target.value)}
              />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colFlags', 'Flags')}>
              <Input value={rule.flags} onChange={(e) => updateDraft(index, 'flags', e.target.value)} />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.perfTest.label', 'Vectorscan performance')}>
              <PatternPerfButton pattern={rule.pattern} flags={rule.flags} />
            </FormField>
            <FormField label={t('pages:hooks.rulePacks.colLabels', 'Labels (comma-separated)')}>
              <Input value={rule.labels} onChange={(e) => updateDraft(index, 'labels', e.target.value)} />
            </FormField>
            <FormField
              label={t('pages:hooks.rulePacks.colDescription', 'Description')}
              className={styles.ruleDescriptionField}
            >
              <Input
                value={rule.description}
                onChange={(e) => updateDraft(index, 'description', e.target.value)}
              />
            </FormField>
          </div>
        </details>
      ))}
    </Stack>
  );
}
