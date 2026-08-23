import { useTranslation } from 'react-i18next';
import {
  PageHeader, AlertDialog, Breadcrumb,
  Skeleton, ErrorBanner, Card, Stack, Button, FormField,
} from '@/components/ui';
import { useRoutingRuleDetail } from './useRoutingRuleDetail';
import { RoutingRuleReadView } from './RoutingRuleReadView';
import { RoutingRuleEditForm } from './RoutingRuleEditForm';
import { ModelCodeTypeahead } from './ModelCodeTypeahead';
import { SIMULATABLE_ENDPOINT_KINDS } from '../_shared/routing-rule-config';
import styles from './RoutingRuleDetail.module.css';

export function RoutingRuleDetailPage() {
  const { t } = useTranslation();
  const detail = useRoutingRuleDetail();

  const {
    rule, loading, error, refetch,
    isEditing, startEditing, setDeleting, deleting,
    canUpdate, canDelete, canSimulate,
    deleteRule,
    simModelId, setSimModelId, simEndpointType, setSimEndpointType,
    simLoading, simData, runSimulation,
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
          <p className={styles.simWarning} role="note">
            {t('pages:routing.simWarning')}
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
            {/* The endpoint kind is the input a rule's outcome most depends on:
                modelTypes conditions, the modality filter and non-chat auto all
                key off it. Locked to chat, an admin checking an image or
                speech rule was shown what a chat request would do. */}
            <FormField label={t('pages:routing.simEndpointLabel')} helpText={t('pages:routing.simEndpointHelp')}>
              <select
                className={styles.simEndpointSelect}
                value={simEndpointType}
                aria-label={t('pages:routing.simEndpointLabel')}
                onChange={(e) => setSimEndpointType(e.target.value)}
              >
                {SIMULATABLE_ENDPOINT_KINDS.map((kind) => (
                  <option key={kind} value={kind}>{kind}</option>
                ))}
              </select>
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
