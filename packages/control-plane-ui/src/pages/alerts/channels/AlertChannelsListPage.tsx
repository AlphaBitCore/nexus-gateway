/**
 * AlertChannelsListPage — browse, test, and manage unified alert channels.
 *
 * Channels are the canonical delivery destinations (webhook, Slack, email,
 * PagerDuty) that Hub fires when a rule matches. The list comes from
 * `GET /api/admin/alerts/channels`; actions are thin wrappers over
 * `alertsApi.updateChannel`, `alertsApi.deleteChannel`, and
 * `alertsApi.testChannel`.
 *
 * Full per-channel editing (config blob, severities, sourceTypes) lives on
 * `AlertChannelEditPage`; "Edit" and "New channel" navigate there.
 */
import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type { AlertChannel, AlertSeverity } from '@/api/services';
import {
  PageHeader,
  DataTable,
  Badge,
  Button,
  Stack,
  Card,
  Switch,
  Skeleton,
  ErrorBanner,
  AlertDialog,
} from '@/components/ui';
import type { BadgeProps, DataTableColumn } from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import styles from './AlertChannelsListPage.module.css';

function severityVariant(s: AlertSeverity): BadgeProps['variant'] {
  switch (s) {
    case 'critical':
    case 'high':
      return 'danger';
    case 'medium':
      return 'warning';
    case 'low':
      return 'info';
    default:
      return 'default';
  }
}

interface UpdateEnabledInput {
  id: string;
  enabled: boolean;
}

export function AlertChannelsListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();

  const { data, loading, error, refetch } = useApi<{ channels: AlertChannel[] }>(
    () => alertsApi.listChannels(),
    ['admin', 'alerts', 'channels', 'list'],
  );

  const [deleteTarget, setDeleteTarget] = useState<AlertChannel | null>(null);
  const [testingId, setTestingId] = useState<string | null>(null);

  const { mutate: toggleEnabled, loading: togglingEnabled } = useMutation<
    UpdateEnabledInput,
    AlertChannel
  >(
    ({ id, enabled }) => alertsApi.updateChannel(id, { enabled }),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:alerts.channels.toggleSuccess'),
    },
  );

  const { mutate: deleteChannel, loading: deleting } = useMutation<string, void>(
    (id) => alertsApi.deleteChannel(id),
    {
      onSuccess: () => {
        setDeleteTarget(null);
        refetch();
      },
      successMessage: t('pages:alerts.channels.deleteSuccess'),
    },
  );

  const onEdit = useCallback(
    (row: AlertChannel) => {
      navigate(`/alerts/channels/${encodeURIComponent(row.id)}`);
    },
    [navigate],
  );

  const onNew = useCallback(() => {
    navigate('/alerts/channels/new');
  }, [navigate]);

  const onTest = useCallback(
    async (row: AlertChannel) => {
      setTestingId(row.id);
      try {
        const result = await alertsApi.testChannel(row.id);
        if (result.success) {
          addToast(t('pages:alerts.channels.testSuccess'), 'success');
        } else {
          const reason = result.errorMsg
            || (result.statusCode != null
              ? t('pages:alerts.channels.testFailedHttp', { status: result.statusCode })
              : t('pages:alerts.channels.testFailed'));
          addToast(
            t('pages:alerts.channels.testFailedWithReason', { reason }),
            'error',
          );
        }
      } catch (err) {
        const msg = err instanceof Error ? err.message : 'unknown error';
        addToast(
          t('pages:alerts.channels.testFailedWithReason', { reason: msg }),
          'error',
        );
      } finally {
        setTestingId(null);
      }
    },
    [addToast, t],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const rows = data?.channels ?? [];

  const typeLabel: Record<AlertChannel['type'], string> = {
    webhook: t('pages:alerts.channels.types.webhook'),
    slack: t('pages:alerts.channels.types.slack'),
    email: t('pages:alerts.channels.types.email'),
    pagerduty: t('pages:alerts.channels.types.pagerduty'),
  };

  const severityLabel: Record<AlertSeverity, string> = {
    critical: t('pages:alerts.channels.severities.critical'),
    high: t('pages:alerts.channels.severities.high'),
    medium: t('pages:alerts.channels.severities.medium'),
    low: t('pages:alerts.channels.severities.low'),
    info: t('pages:alerts.channels.severities.info'),
  };

  const columns: DataTableColumn<AlertChannel>[] = [
    {
      key: 'name',
      label: t('pages:alerts.channels.columns.name'),
      render: (r) => <strong>{r.name}</strong>,
    },
    {
      key: 'type',
      label: t('pages:alerts.channels.columns.type'),
      render: (r) => <Badge variant="outline">{typeLabel[r.type] ?? r.type}</Badge>,
    },
    {
      key: 'enabled',
      label: t('pages:alerts.channels.columns.enabled'),
      sortable: false,
      render: (r) => (
        <div onClick={(e) => e.stopPropagation()}>
          <Switch
            checked={r.enabled}
            disabled={togglingEnabled}
            onCheckedChange={(next) => {
              toggleEnabled({ id: r.id, enabled: next });
            }}
          />
        </div>
      ),
    },
    {
      key: 'severities',
      label: t('pages:alerts.channels.columns.severities'),
      sortable: false,
      render: (r) =>
        r.severities.length === 0 ? (
          <span className={styles.dim}>{t('pages:alerts.channels.allSeverities')}</span>
        ) : (
          <div className={styles.chipRow}>
            {r.severities.map((s) => (
              <Badge key={s} variant={severityVariant(s)}>
                {severityLabel[s] ?? s}
              </Badge>
            ))}
          </div>
        ),
    },
    {
      key: 'sourceTypes',
      label: t('pages:alerts.channels.columns.sourceTypes'),
      sortable: false,
      render: (r) =>
        r.sourceTypes.length === 0 ? (
          <span className={styles.dim}>&mdash;</span>
        ) : (
          <div className={styles.chipRow}>
            {r.sourceTypes.map((s) => (
              <Badge key={s} variant="default">
                {s}
              </Badge>
            ))}
          </div>
        ),
    },
    {
      key: 'actions',
      label: t('pages:alerts.channels.columns.actions'),
      sortable: false,
      render: (r) => (
        <Stack direction="horizontal" gap="xs" onClick={(e) => e.stopPropagation()}>
          <Button
            variant="secondary"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              onEdit(r);
            }}
          >
            {t('common:edit')}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            loading={testingId === r.id}
            onClick={(e) => {
              e.stopPropagation();
              void onTest(r);
            }}
          >
            {t('pages:alerts.channels.actions.test')}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              setDeleteTarget(r);
            }}
          >
            {t('common:delete')}
          </Button>
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:alerts.channels.title')}
        subtitle={t('pages:alerts.channels.subtitle')}
        action={
          <Button variant="primary" onClick={onNew}>
            {t('pages:alerts.channels.newChannel')}
          </Button>
        }
      />
      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={columns}
          data={rows}
          onRowClick={onEdit}
          emptyMessage={t('pages:alerts.channels.empty')}
        />
      </Card>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
        title={t('pages:alerts.channels.deleteConfirmTitle')}
        description={
          deleteTarget
            ? t('pages:alerts.channels.deleteConfirmBody', { name: deleteTarget.name })
            : ''
        }
        confirmLabel={t('common:delete')}
        cancelLabel={t('common:cancel')}
        variant="danger"
        loading={deleting}
        onConfirm={() => {
          if (deleteTarget) void deleteChannel(deleteTarget.id);
        }}
      />
    </Stack>
  );
}
