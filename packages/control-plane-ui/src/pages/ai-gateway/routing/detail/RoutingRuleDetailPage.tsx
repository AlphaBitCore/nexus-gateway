import { useTranslation } from 'react-i18next';
import {
  PageHeader, AlertDialog, Breadcrumb,
  Skeleton, ErrorBanner, Card, Stack, Button, FormField,
} from '@/components/ui';
import { useRoutingRuleDetail } from './useRoutingRuleDetail';
import { RoutingRuleReadView } from './RoutingRuleReadView';
import { RoutingRuleEditForm } from './RoutingRuleEditForm';
import { ModelCodeTypeahead } from './ModelCodeTypeahead';
import styles from './RoutingRuleDetail.module.css';

export function RoutingRuleDetailPage() {
  const { t } = useTranslation();
  const detail = useRoutingRuleDetail();

  const {
    rule, loading, error, refetch,
    isEditing, startEditing, setDeleting, deleting,
    canUpdate, canDelete, canSimulate,
    deleteRule,
    simModelId, setSimModelId, simLoading, simData, runSimulation,
  } = detail;

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!rule) return null;

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:routing.title'), to: '/ai-gateway/routing' },
        { label: rule.name },
      ]} />

      <PageHeader
        title={rule.name}
        subtitle={rule.description || undefined}
        action={
          <Stack direction="horizontal" gap="sm" align="center">
            {canUpdate && !isEditing && (
              <Button variant="secondary" onClick={startEditing}>{t('pages:routing.edit')}</Button>
            )}
            {canDelete && (
              <Button variant="danger" onClick={() => setDeleting(true)}>{t('pages:routing.delete')}</Button>
            )}
          </Stack>
        }
      />

      <Card>
        <h2 className={styles.widgetTitle}>{t('pages:routing.routingRuleInfo')}</h2>
        {isEditing
          ? <RoutingRuleEditForm detail={detail} />
          : <RoutingRuleReadView detail={detail} />}
      </Card>

      {!isEditing && canSimulate && (
        <Card>
          <h2 className={styles.widgetTitle}>{t('pages:routing.routingPreview')}</h2>
          <p className={styles.simDescription}>
            {t('pages:routing.simDescription')}
          </p>
          <div className={styles.simInputRow}>
            <FormField label={t('pages:routing.simModelIdLabel')} helpText={t('pages:routing.simModelIdHelp')}>
              <div className={styles.simInputGroup}>
                <ModelCodeTypeahead
                  value={simModelId}
                  onChange={setSimModelId}
                  ariaLabel={t('pages:routing.simModelIdLabel')}
                  placeholder={t('pages:routing.simModelIdPlaceholder')}
                />
                <Button
                  className={styles.simInlineButton}
                  disabled={simLoading || !simModelId.trim()}
                  onClick={runSimulation}
                >
                  {simLoading ? t('pages:routing.running') : t('pages:routing.runSimulation')}
                </Button>
              </div>
            </FormField>
          </div>
          {simData && (
            <pre className={styles.codeBlockScrollable}>
              {JSON.stringify(simData, null, 2)}
            </pre>
          )}
        </Card>
      )}

      <AlertDialog
        open={deleting}
        onOpenChange={(open) => { if (!open) setDeleting(false); }}
        title={t('pages:routing.deleteRule')}
        description={t('pages:routing.deleteConfirm', { name: rule.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteRule(undefined as never)}
        variant="danger"
      />
    </Stack>
  );
}
