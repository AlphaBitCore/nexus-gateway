import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';

import { rulePacksApi, type RulePack, type RulePackMatch } from '@/api/services';
import { Button, Card, ErrorBanner, FormField, Stack, Textarea } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import styles from './RulePackDetail.module.css';

type SortKey = 'ruleId' | 'category' | 'severity';

function formatCreatedAt(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function compareText(a: string | undefined, b: string | undefined): number {
  return (a ?? '').localeCompare(b ?? '');
}

export function RulePackDetail() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { id = '' } = useParams<{ id: string }>();
  const [sortKey, setSortKey] = useState<SortKey>('ruleId');
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('asc');
  const [content, setContent] = useState('');
  const [running, setRunning] = useState(false);
  const [dryRunError, setDryRunError] = useState<string | null>(null);
  const [matches, setMatches] = useState<RulePackMatch[]>([]);

  const { data, loading, error, refetch } = useApi<RulePack>(
    () => rulePacksApi.get(id),
    ['admin', 'rule-packs', 'detail', id],
    { skip: id === '' },
  );

  const { mutate: deletePack, loading: deleting } = useMutation<string, void>(
    (packId) => rulePacksApi.delete(packId),
    {
      invalidateQueries: [['admin', 'rule-packs', 'list']],
      successMessage: t('pages:hooks.rulePacks.deleteSuccess', 'Rule pack deleted'),
      onSuccess: () => navigate('/compliance/rule-packs'),
    },
  );

  function onDelete() {
    if (!data) return;
    const message = t(
      'pages:hooks.rulePacks.deleteConfirm',
      'Delete rule pack "{{name}}" {{version}}? Installs referencing this pack must be removed first.',
      { name: data.name, version: data.version },
    );
     
    if (!window.confirm(message)) return;
    deletePack(data.id);
  }

  const sortedRules = useMemo(() => {
    const rules = [...(data?.rules ?? [])];
    rules.sort((left, right) => {
      const multiplier = sortDir === 'asc' ? 1 : -1;
      if (sortKey === 'category') return compareText(left.category, right.category) * multiplier;
      if (sortKey === 'severity') return compareText(left.severity, right.severity) * multiplier;
      return compareText(left.ruleId, right.ruleId) * multiplier;
    });
    return rules;
  }, [data?.rules, sortDir, sortKey]);

  function toggleSort(next: SortKey) {
    if (sortKey === next) {
      setSortDir((current) => (current === 'asc' ? 'desc' : 'asc'));
      return;
    }
    setSortKey(next);
    setSortDir('asc');
  }

  async function onDryRun() {
    if (!id || content.trim() === '') return;
    setRunning(true);
    setDryRunError(null);
    try {
      const result = await rulePacksApi.dryRun(id, content);
      setMatches(result.matches);
    } catch (err) {
      setMatches([]);
      setDryRunError(err instanceof Error ? err.message : String(err));
    } finally {
      setRunning(false);
    }
  }

  if (loading) {
    return <div className={styles.state}>{t('common:loading', 'Loading…')}</div>;
  }

  if (error) {
    return (
      <ErrorBanner
        message={error.message}
        onRetry={refetch}
      />
    );
  }

  if (!data) {
    return <div className={styles.state}>{t('pages:hooks.rulePacks.notFound', 'Rule pack not found.')}</div>;
  }

  return (
    <Stack gap="lg">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link
            to="/compliance/rule-packs"
            className={styles.backLink}
            aria-label={t('common:back', 'Back')}
          >
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.title}>{data.name}</h1>
            <p className={styles.subtitle}>
              {t(
                'pages:hooks.rulePacks.detailSubtitle',
                'Inspect authored rules, metadata, and test sample content before binding this pack to a hook.',
              )}
            </p>
          </div>
          <div className={styles.headerActions}>
            <Button variant="secondary" onClick={() => navigate(`/compliance/rule-packs/${data.id}/edit`)}>
              {t('common:edit', 'Edit')}
            </Button>
            <Button variant="danger" onClick={onDelete} loading={deleting}>
              {t('common:delete', 'Delete')}
            </Button>
          </div>
        </div>
      </section>

      <Card>
        <div className={styles.metaGrid}>
          <div>
            <div className={styles.metaLabel}>{t('pages:hooks.rulePacks.colVersion', 'Version')}</div>
            <div className={styles.metaValue}>{data.version}</div>
          </div>
          <div>
            <div className={styles.metaLabel}>{t('pages:hooks.rulePacks.colMaintainer', 'Maintainer')}</div>
            <div className={styles.metaValue}>{data.maintainer}</div>
          </div>
          <div>
            <div className={styles.metaLabel}>{t('pages:hooks.rulePacks.colCreatedAt', 'Created')}</div>
            <div className={styles.metaValue}>{formatCreatedAt(data.createdAt)}</div>
          </div>
          <div>
            <div className={styles.metaLabel}>{t('pages:hooks.rulePacks.ruleCount', 'Rules')}</div>
            <div className={styles.metaValue}>{data.rules.length}</div>
          </div>
        </div>
      </Card>

      <section className={styles.flatSection}>
        <div className={styles.sectionHeader}>
          <h2 className={styles.sectionTitle}>{t('pages:hooks.rulePacks.rulesTitle', 'Rules')}</h2>
          <div className={styles.sortHint}>
            {t('pages:hooks.rulePacks.sortHint', 'Click Category / Severity / Rule ID to sort')}
          </div>
        </div>
        <div className={styles.tableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>
                  <button type="button" className={styles.sortButton} onClick={() => toggleSort('ruleId')}>
                    {t('pages:hooks.rulePacks.colRuleId', 'Rule ID')}
                  </button>
                </th>
                <th>
                  <button type="button" className={styles.sortButton} onClick={() => toggleSort('category')}>
                    {t('pages:hooks.rulePacks.colCategory', 'Category')}
                  </button>
                </th>
                <th>
                  <button type="button" className={styles.sortButton} onClick={() => toggleSort('severity')}>
                    {t('pages:hooks.rulePacks.colSeverity', 'Severity')}
                  </button>
                </th>
                <th>{t('pages:hooks.rulePacks.colPattern', 'Pattern')}</th>
              </tr>
            </thead>
            <tbody>
              {sortedRules.map((rule) => (
                <tr key={rule.id ?? rule.ruleId}>
                  <td>{rule.ruleId}</td>
                  <td>{rule.category}</td>
                  <td>{rule.severity}</td>
                  <td className={styles.patternCell}>{rule.pattern}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className={styles.flatSection}>
        <Stack gap="md" className={styles.dryRunStack}>
          <div className={`${styles.sectionHeader} ${styles.dryRunHeader}`}>
            <h2 className={styles.sectionTitle}>{t('pages:hooks.rulePacks.dryRunTitle', 'Try Content')}</h2>
            <div className={styles.sortHint}>
              {t('pages:hooks.rulePacks.dryRunSubtitle', 'Run sample text through this pack without persisting anything.')}
            </div>
          </div>

          <FormField label={t('pages:hooks.rulePacks.dryRunContent', 'Content')}>
            <Textarea
              value={content}
              rows={6}
              onChange={(e) => setContent(e.target.value)}
              placeholder={t('pages:hooks.rulePacks.dryRunPlaceholder', 'Paste text to test against the selected pack')}
            />
          </FormField>

          <div>
            <Button onClick={onDryRun} loading={running} disabled={running || content.trim() === ''}>
              {t('pages:hooks.rulePacks.dryRunRun', 'Run')}
            </Button>
          </div>

          {dryRunError && <ErrorBanner message={dryRunError} />}

          {matches.length > 0 && (
            <div className={styles.tableWrap}>
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>{t('pages:hooks.rulePacks.colRuleId', 'Rule ID')}</th>
                    <th>{t('pages:hooks.rulePacks.colCategory', 'Category')}</th>
                    <th>{t('pages:hooks.rulePacks.colSeverity', 'Severity')}</th>
                    <th>{t('pages:hooks.rulePacks.colMatchedText', 'Matched text')}</th>
                  </tr>
                </thead>
                <tbody>
                  {matches.map((match) => (
                    <tr key={`${match.ruleId}-${match.matchedText ?? ''}`}>
                      <td>{match.ruleId}</td>
                      <td>{match.category}</td>
                      <td>{match.severity}</td>
                      <td className={styles.patternCell}>{match.matchedText ?? '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {!running && !dryRunError && matches.length === 0 && (
            <div className={styles.state}>
              {t('pages:hooks.rulePacks.dryRunEmpty', 'No dry-run results yet.')}
            </div>
          )}
        </Stack>
      </section>
    </Stack>
  );
}

