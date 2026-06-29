import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams, useNavigate } from 'react-router-dom';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { hookApi, rulePacksApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  Badge, statusToVariant, AlertDialog,
  Skeleton, ErrorBanner, Button, Stack, Card, Tooltip,
  Tabs, TabsList, TabsTrigger, TabsContent, Textarea,
} from '@/components/ui';
import { HookForm } from '../form/HookForm';
import { HookRulePacksPanel } from '../panels/HookRulePacksPanel';
import type { HookConfig, HookCategory, HookExecutionChain } from '@/api/types';
import { HOOK_CATEGORY } from '@/constants/hooks';
import { formatDate } from '@/lib/format';
import styles from './HookDetail.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

const RULE_PACK_IMPLEMENTATIONS = new Set([
  'content-safety',
  'keyword-filter',
  'pii-detector',
  'rulepack-engine',
]);

function categoryBadgeClass(cat: HookCategory | undefined, styles: Record<string, string>): string {
  switch (cat) {
    case HOOK_CATEGORY.COMPLIANCE: return styles.badgeCategoryCompliance;
    case HOOK_CATEGORY.TRAFFIC_CONTROL: return styles.badgeCategoryTrafficControl;
    case HOOK_CATEGORY.QUALITY: return styles.badgeCategoryQuality;
    case HOOK_CATEGORY.OBSERVABILITY: return styles.badgeCategoryObservability;
    default: return styles.badgeCategoryDefault;
  }
}

function stageBadgeClass(stage: string, styles: Record<string, string>): string {
  if (stage === 'Response') return styles.badgeStageResponse;
  return styles.badgeStageRequest;
}

function stageLabel(hook: HookConfig, t: ReturnType<typeof useTranslation>['t']): string {
  const stage = hook.stage?.toLowerCase();
  if (stage === 'response') return t('pages:hooks.stageResponse', 'Response');
  return t('pages:hooks.stageRequest', 'Request');
}

function defaultHookTestInput(implementationId?: string): string {
  if (implementationId === 'ip-access-filter') {
    return '{\n  "input": {\n    "prompt": "Hello world email: user@example.com",\n    "sourceIp": "127.0.0.1"\n  }\n}';
  }
  return '{\n  "prompt": "Hello world email: user@example.com"\n}';
}

export function HookDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [isEditing, setIsEditing] = useState(false);
  const [showDelete, setShowDelete] = useState(false);
  const [testInput, setTestInput] = useState(defaultHookTestInput());
  const [testInputTouched, setTestInputTouched] = useState(false);
  const [testResult, setTestResult] = useState<string | null>(null);
  const [testLoading, setTestLoading] = useState(false);

  const canUpdate = usePermission('hook:update');
  const canDelete = usePermission('hook:delete');

  const { data: hook, loading, error, refetch } = useApi<HookConfig>(
    () => hookApi.get(id!),
    ['admin', 'hooks', 'detail', id],
  );

  const implId = hook?.implementationId;
  const rulePackConsumer = Boolean(implId && RULE_PACK_IMPLEMENTATIONS.has(implId));

  useEffect(() => {
    if (!hook || testInputTouched) {
      return;
    }
    setTestInput(defaultHookTestInput(hook.implementationId));
  }, [hook, testInputTouched]);

  const {
    data: rulePackInstalls,
    loading: rulePackInstallsLoading,
    error: rulePackInstallsError,
    refetch: refetchRulePackInstalls,
  } = useApi(
    () => rulePacksApi.listInstallsForHook(id!),
    ['admin', 'hooks', 'rule-pack-installs', id],
    { skip: !id || !rulePackConsumer },
  );

  const { data: chain } = useApi<HookExecutionChain>(
    () => hookApi.getExecutionChain(),
    ['admin', 'hooks', 'execution-chain'],
  );

  const { mutate: deleteHook } = useMutation(
    (_: string) => hookApi.delete(_),
    {
      invalidateQueries: [['api', 'admin', 'hooks']],
      onSuccess: () => navigate('/compliance/hooks'),
      successMessage: 'Hook deleted',
    },
  );

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!hook) return null;

  const rulePackInstallList = rulePackInstalls ?? [];
  const enabledRulePackInstalls = rulePackInstallList.filter((i) => i.enabled);

  if (isEditing) {
    return (
      <Stack gap="lg">
        <section className={styles.detailHeader}>
          <div className={styles.headerTitleRow}>
            <Link to="/compliance/hooks" className={styles.backLink} aria-label={t('common:back', 'Back')}>
              <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
                <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
                <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            </Link>
            <div className={styles.headerTextBlock}>
              <h1 className={styles.detailTitle}>{hook.name}</h1>
              <p className={styles.detailSubtitle}>{t('pages:hooks.editHook')}</p>
            </div>
            <div className={styles.headerActions}>
              <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('common:cancel')}</Button>
            </div>
          </div>
        </section>
        <HookForm
          embedded
          hook={hook}
          onClose={() => setIsEditing(false)}
          onSaved={() => {
            void refetch();
            setIsEditing(false);
          }}
        />
      </Stack>
    );
  }

  const stage = stageLabel(hook, t);
  const failBehavior = (hook.failBehavior ?? 'OPEN').toUpperCase();

  // Find pipeline position
  let pipelinePosition: string | null = null;
  if (chain) {
    const allSteps = [...(chain.requestHooks ?? []), ...(chain.responseHooks ?? [])];
    const step = allSteps.find(s => s.hookConfigId === id);
    if (step) {
      const stageSteps = hook.stage === 'response' ? (chain.responseHooks ?? []) : (chain.requestHooks ?? []);
      const idx = stageSteps.findIndex(s => s.hookConfigId === id);
      // i18next v26 quirk: passing `defaultValue` in the SAME options
      // object as interpolation variables causes the engine to skip
      // interpolation under some load-order paths — the user then sees
      // the literal '{{idx}}' template. The key exists in all 3 locale
      // files (en/zh/es pages.json:652), so defaultValue is dead code
      // anyway. Drop it to match the working pattern used everywhere
      // else in this codebase (e.g. LiveTrafficActiveFiltersBar:13).
      pipelinePosition = t('pages:hooks.pipelinePosition', {
        idx: idx + 1,
        total: stageSteps.length,
        stage: stage.toLowerCase(),
        priority: hook.priority,
      });
    }
  }

  const handleTest = async () => {
    setTestLoading(true);
    setTestResult(null);
    try {
      const parsed = JSON.parse(testInput) as unknown;
      const payload =
        parsed !== null &&
        typeof parsed === 'object' &&
        !Array.isArray(parsed) &&
        'input' in (parsed as Record<string, unknown>)
          ? (parsed as { input?: unknown; sampleBody?: unknown; statusCode?: number })
          : { input: parsed };
      const result = await hookApi.test(id!, payload);
      setTestResult(JSON.stringify(result, null, 2));
    } catch (err) {
      setTestResult(err instanceof Error ? err.message : 'Test failed');
    } finally {
      setTestLoading(false);
    }
  };

  return (
    <Stack gap="sm">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link to="/compliance/hooks" className={styles.backLink} aria-label={t('common:back', 'Back')}>
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.detailTitle}>{hook.name}</h1>
            <p className={styles.detailSubtitle}>{t('pages:hooks.hookConfiguration', 'Hook Configuration')}</p>
          </div>
          <Stack direction="horizontal" gap="sm" className={styles.headerActions}>
            {canUpdate && (
              <Button onClick={() => setIsEditing(true)}>{t('common:edit')}</Button>
            )}
            {canDelete && (
              <Button variant="danger" onClick={() => setShowDelete(true)}>{t('common:delete')}</Button>
            )}
          </Stack>
        </div>
      </section>

      <Tabs defaultValue="overview">
        <TabsList aria-label={t('pages:hooks.hookSections')}>
          <TabsTrigger value="overview">{t('pages:hooks.overviewTab')}</TabsTrigger>
          <TabsTrigger value="config">{t('pages:hooks.configurationTab')}</TabsTrigger>
          <TabsTrigger value="pipeline">{t('pages:hooks.pipelineTab')}</TabsTrigger>
          {hook.implementationId && RULE_PACK_IMPLEMENTATIONS.has(hook.implementationId) && (
            <TabsTrigger value="rule-packs">
              {t('pages:hooks.rulePacksTab', 'Rule Packs')}
            </TabsTrigger>
          )}
          <TabsTrigger value="test">{t('pages:hooks.testTab')}</TabsTrigger>
        </TabsList>

        {/* Overview Tab */}
        <TabsContent value="overview">
          <Stack gap="md">
            <section className={styles.detailSection}>
              <div className={styles.detailSectionHeader}>
              <h2 className={styles.widgetTitle}>{t('pages:hooks.details')}</h2>
                <div className={styles.badgesRow}>
                  <span className={clsx(styles.badge, categoryBadgeClass(hook.classification?.category, styles))}>
                    {hook.classification?.categoryLabel ?? '-'}
                  </span>
                  <span className={clsx(styles.badge, stageBadgeClass(stage, styles))}>
                    {stage}
                  </span>
                  <Badge variant={statusToVariant(hook.enabled ? 'enabled' : 'disabled')}>{hook.enabled ? t('common:enabled') : t('common:disabled')}</Badge>
                </div>
              </div>
            <Card>
              <div className={styles.kvGrid}>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.typeLabel')}</span>
                    <Tooltip content={t('pages:hooks.typeTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.typeLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>{hook.type}</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.stageLabel')}</span>
                    <Tooltip content={t('pages:hooks.stageTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.stageLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>{hook.stage}</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.priorityLabel')}</span>
                    <Tooltip content={t('pages:hooks.priorityTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.priorityLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>{hook.priority}</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.timeoutLabel')}</span>
                    <Tooltip content={t('pages:hooks.timeoutTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.timeoutLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>{hook.timeoutMs}ms</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.failBehaviorLabel')}</span>
                    <Tooltip content={t('pages:hooks.failBehaviorTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.failBehaviorLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>
                    <span className={`${styles.failBadge} ${failBehavior === 'CLOSED' ? styles.failClosed : styles.failOpen}`}>
                      {failBehavior}
                    </span>
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:hooks.enabledLabel')}</span>
                    <Tooltip content={t('pages:hooks.enabledTooltip')}>
                      <HelpIconButton aria-label={t('pages:hooks.enabledLabel')} />
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>{hook.enabled ? t('pages:hooks.yes') : t('pages:hooks.no')}</div>
                </div>
                {hook.classification?.implementationLabel && (
                  <div>
                    <div className={styles.kvLabel}>{t('pages:hooks.implementationLabel')}</div>
                    <div className={styles.kvValue}>{hook.classification.implementationLabel}</div>
                  </div>
                )}
                {hook.classification?.dualPhaseCapable && (
                  <div>
                    <div className={styles.kvLabel}>{t('pages:hooks.dualPhaseLabel')}</div>
                    <div className={styles.kvValue}>{t('pages:hooks.dualPhaseValue')}</div>
                  </div>
                )}
                {hook.category && (
                  <div>
                    <div className={styles.kvLabel}>{t('pages:hooks.rawCategoryDb')}</div>
                    <div className={styles.kvValue}>{hook.category}</div>
                  </div>
                )}
                <div>
                  <div className={styles.kvLabel}>{t('pages:hooks.created')}</div>
                  <div className={styles.kvValue}>{formatDate(hook.createdAt)}</div>
                </div>
                {hook.updatedAt && (
                  <div>
                    <div className={styles.kvLabel}>{t('pages:hooks.updated')}</div>
                    <div className={styles.kvValue}>{formatDate(hook.updatedAt)}</div>
                  </div>
                )}
              </div>
            </Card>
            </section>
          </Stack>
        </TabsContent>

        {/* Configuration Tab */}
        <TabsContent value="config">
          <Stack gap="md">
            {rulePackConsumer && rulePackInstallsLoading && (
              <p className={styles.configHintMuted}>{t('common:loading', 'Loading…')}</p>
            )}
            {rulePackConsumer && rulePackInstallsError && (
              <ErrorBanner
                message={rulePackInstallsError.message}
                onRetry={() => { void refetchRulePackInstalls(); }}
              />
            )}
            {rulePackConsumer && !rulePackInstallsLoading && !rulePackInstallsError && enabledRulePackInstalls.length > 0 && (
              <div className={styles.configInfoCallout} role="status">
                <h3 className={styles.configInfoCalloutTitle}>
                  {t('pages:hooks.rulePacks.configPrecedenceTitle', 'What actually runs')}
                </h3>
                <p className={styles.configInfoCalloutBody}>
                  {t(
                    'pages:hooks.rulePacks.configPrecedenceBody',
                    'This hook has at least one enabled rule-pack install. The gateway merges rules from those installs (in install order) and evaluates them through the unified rule-pack engine. Inline pattern lists in the JSON below are ignored for matching.',
                  )}
                </p>
                {hook.implementationId === 'pii-detector' && (
                  <p className={styles.configInfoCalloutBody}>
                    {t(
                      'pages:hooks.rulePacks.configPrecedencePiiExtra',
                      'In-place PII redact (MODIFY) from legacy patternDefinitions is not available while rule packs are enabled; decisions follow pack severities (hard, soft, info) instead.',
                    )}
                  </p>
                )}
                <p className={styles.configInfoCalloutBody}>
                  {t(
                    'pages:hooks.rulePacks.configPrecedenceSeeTab',
                    'Manage installs and per-install overrides on the Rule Packs tab.',
                  )}
                </p>
              </div>
            )}
            {rulePackConsumer && !rulePackInstallsLoading && !rulePackInstallsError && enabledRulePackInstalls.length === 0 && rulePackInstallList.length > 0 && (
              <div className={styles.configInfoCallout} role="status">
                <h3 className={styles.configInfoCalloutTitle}>
                  {t('pages:hooks.rulePacks.configAllInstallsDisabledTitle', 'Rule packs are paused')}
                </h3>
                <p className={styles.configInfoCalloutBody}>
                  {t(
                    'pages:hooks.rulePacks.configAllInstallsDisabledBody',
                    'Every rule-pack install on this hook is disabled. The gateway uses inline patterns from the JSON below (legacy path), same as when no packs are bound.',
                  )}
                </p>
              </div>
            )}
            {rulePackConsumer && !rulePackInstallsLoading && !rulePackInstallsError && rulePackInstallList.length === 0 && (
              <div className={styles.configInfoCallout} role="status">
                <h3 className={styles.configInfoCalloutTitle}>
                  {t('pages:hooks.rulePacks.configNoInstallsTitle', 'Inline configuration is authoritative')}
                </h3>
                <p className={styles.configInfoCalloutBody}>
                  {t(
                    'pages:hooks.rulePacks.configNoInstallsBody',
                    'No rule packs are bound to this hook. Matching uses only the fields in the JSON below. Bind packs on the Rule Packs tab when you want catalog-managed rules instead.',
                  )}
                </p>
              </div>
            )}
            {Boolean(hook.config) && (
              <section className={styles.detailSection}>
                <h2 className={styles.widgetTitle}>{t('pages:hooks.configuration')}</h2>
              <Card padding="none">
                <pre className={styles.configPre}>
                  {JSON.stringify(hook.config, null, 2)}
                </pre>
              </Card>
              </section>
            )}

            {hook.endpoint && (
              <section className={styles.detailSection}>
                <h2 className={styles.widgetTitle}>{t('pages:hooks.webhookEndpoint')}</h2>
              <Card>
                <code className={styles.endpointCode}>
                  {hook.endpoint}
                </code>
              </Card>
              </section>
            )}

            {!hook.config && !hook.endpoint && (
              <Card>
                <p className={styles.emptyConfig}>
                  {t('pages:hooks.noConfigData')}
                </p>
              </Card>
            )}
          </Stack>
        </TabsContent>

        {/* Pipeline Tab */}
        <TabsContent value="pipeline">
          <section className={styles.detailSection}>
            <h2 className={styles.widgetTitle}>{t('pages:hooks.pipelinePositionTitle')}</h2>
          <Card>
            <p className={clsx(styles.pipelineText, pipelinePosition ? styles.pipelineTextActive : styles.pipelineTextMuted)}>
              {pipelinePosition ?? t('pages:hooks.pipelineUnknown')}
            </p>
          </Card>
          </section>
        </TabsContent>

        {/* Rule Packs Tab */}
        {hook.implementationId && RULE_PACK_IMPLEMENTATIONS.has(hook.implementationId) && (
          <TabsContent value="rule-packs">
            <HookRulePacksPanel hookId={hook.id} />
          </TabsContent>
        )}

        {/* Test Tab */}
        <TabsContent value="test">
          <section className={styles.detailSection}>
            <h2 className={styles.widgetTitle}>{t('pages:hooks.testHook')}</h2>
          <Card>
            <p className={styles.testDescription}>
              {t('pages:hooks.testDescription')}
            </p>
            {hook.implementationId && RULE_PACK_IMPLEMENTATIONS.has(hook.implementationId) && (
              <p className={styles.testDescription}>
                {t(
                  'pages:hooks.testRulePackTestNote',
                  'The AI gateway resolves enabled rule-pack installs from the database for this test, matching production behavior.',
                )}
              </p>
            )}
            <Textarea
              value={testInput}
              onChange={(e) => {
                setTestInputTouched(true);
                setTestInput(e.target.value);
              }}
              className={styles.testTextarea}
            />
            <Button
              data-testid="hook-test-run"
              onClick={handleTest}
              disabled={testLoading}
            >
              {testLoading ? t('pages:hooks.running') : t('pages:hooks.testHookButton')}
            </Button>
            {testResult && (
              <pre data-testid="hook-test-verdict" className={styles.configPre}>
                {testResult}
              </pre>
            )}
          </Card>
          </section>
        </TabsContent>
      </Tabs>

      <AlertDialog
        open={showDelete}
        onOpenChange={(open) => { if (!open) setShowDelete(false); }}
        title={t('pages:hooks.deleteHook')}
        description={t('pages:hooks.deleteConfirm', 'Are you sure you want to delete hook "{{name}}"? This action cannot be undone.', { name: hook.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteHook(id!)}
        variant="danger"
      />
    </Stack>
  );
}
