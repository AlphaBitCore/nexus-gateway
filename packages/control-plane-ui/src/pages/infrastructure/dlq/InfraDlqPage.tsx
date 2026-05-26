/**
 * InfraDlqPage — Dead-letter queue admin view.
 *
 * Reads traffic_event_dlq rows from /api/admin/observability/dlq, lets
 * operators inspect why each row landed there (lastError + delivery count),
 * and exposes a single per-row retry button that POSTs back to
 * /api/admin/observability/dlq/:id/retry. Retry republishes the original
 * payload to its MQ subject + deletes the DLQ row on success.
 *
 * Pagination: opaque cursor returned by Hub (newest-first). The next
 * button is enabled while nextCursor is non-empty.
 *
 * IAM: page-load requires admin:observability-dlq.read; retry button is
 * hidden when the user lacks admin:observability-dlq.manage (the
 * shellRouteConfig allowedActions also gates the route mount).
 */
import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { dlqApi } from '@/api/services/infrastructure/dlq/dlq';
import type { DlqListResponse, DlqRow } from '@/api/services/infrastructure/dlq/dlq';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, Stack, Card, Button, Badge, Input, LoadingSpinner, ErrorBanner,
} from '@/components/ui';

const DEFAULT_LIMIT = 50;

function fmtTime(iso: string): string {
  try { return new Date(iso).toLocaleString(); } catch { return iso; }
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export default function InfraDlqPage() {
  const { t } = useTranslation('pages');
  const canRetry = usePermission('observability-dlq:manage');

  const [subject, setSubject] = useState('');
  const [data, setData] = useState<DlqListResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [retrying, setRetrying] = useState<string | null>(null);
  const [cursorStack, setCursorStack] = useState<string[]>([]);

  const load = useCallback(async (cursor?: string) => {
    setLoading(true);
    setError(null);
    try {
      const resp = await dlqApi.list({
        subject: subject.trim() || undefined,
        limit: DEFAULT_LIMIT,
        cursor,
      });
      setData(resp);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, [subject]);

  useEffect(() => { void load(undefined); }, [load]);

  const onSearch = () => {
    setCursorStack([]);
    void load(undefined);
  };

  const onNext = () => {
    if (!data?.nextCursor) return;
    setCursorStack((s) => [...s, data.nextCursor!]);
    void load(data.nextCursor);
  };

  const onPrev = () => {
    if (cursorStack.length === 0) {
      void load(undefined);
      return;
    }
    const newStack = cursorStack.slice(0, -1);
    setCursorStack(newStack);
    const prevCursor = newStack[newStack.length - 1];
    void load(prevCursor);
  };

  const onRetry = async (row: DlqRow) => {
    if (!confirm(t('infrastructure.dlq.retryConfirm', { msgId: row.msgId }))) return;
    setRetrying(row.id);
    try {
      await dlqApi.retry(row.id);
      // Reload current page after retry — the retried row is now gone,
      // but newer rows may have arrived. Reset cursor to current top of
      // stack so the operator sees a fresh slice.
      const currentCursor = cursorStack[cursorStack.length - 1];
      await load(currentCursor);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setRetrying(null);
    }
  };

  return (
    <Stack gap="md">
      <PageHeader
        title={t('infrastructure.dlq.title')}
        subtitle={t('infrastructure.dlq.description')}
      />

      <Card>
        <Stack direction="horizontal" gap="sm" align="center">
          <Input
            placeholder={t('infrastructure.dlq.subjectPlaceholder')}
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') onSearch(); }}
            style={{ flex: 1 }}
          />
          <Button onClick={onSearch} variant="primary" disabled={loading}>
            {t('infrastructure.dlq.search')}
          </Button>
        </Stack>
      </Card>

      {error && <ErrorBanner message={error} onDismiss={() => setError(null)} />}

      {loading && !data ? (
        <LoadingSpinner />
      ) : (
        <Card>
          <table className="cp-table" style={{ width: '100%' }}>
            <thead>
              <tr>
                <th>{t('infrastructure.dlq.colInsertedAt')}</th>
                <th>{t('infrastructure.dlq.colSubject')}</th>
                <th>{t('infrastructure.dlq.colMsgId')}</th>
                <th>{t('infrastructure.dlq.colDeliveries')}</th>
                <th>{t('infrastructure.dlq.colSize')}</th>
                <th>{t('infrastructure.dlq.colLastError')}</th>
                <th>{t('infrastructure.dlq.colActions')}</th>
              </tr>
            </thead>
            <tbody>
              {(data?.rows ?? []).map((r) => (
                <tr key={r.id}>
                  <td>{fmtTime(r.dlqInsertedAt)}</td>
                  <td><code>{r.subject}</code></td>
                  <td><code>{r.msgId}</code></td>
                  <td><Badge variant="warning">{r.deliveryCount}</Badge></td>
                  <td>{fmtSize(r.payloadSize)}</td>
                  <td title={r.lastError ?? ''} style={{ maxWidth: 360, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {r.lastError ?? '—'}
                  </td>
                  <td>
                    {canRetry && (
                      <Button
                        size="sm"
                        variant="danger"
                        disabled={retrying === r.id}
                        onClick={() => onRetry(r)}
                      >
                        {retrying === r.id ? t('infrastructure.dlq.retrying') : t('infrastructure.dlq.retry')}
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
              {(data?.rows ?? []).length === 0 && !loading && (
                <tr>
                  <td colSpan={7} style={{ textAlign: 'center', padding: 'var(--spacing-lg)' }}>
                    {t('infrastructure.dlq.empty')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>

          <Stack direction="horizontal" gap="sm" align="center" style={{ marginTop: 'var(--spacing-sm)' }}>
            <Button variant="secondary" onClick={onPrev} disabled={cursorStack.length === 0 || loading}>
              {t('infrastructure.dlq.prev')}
            </Button>
            <Button variant="secondary" onClick={onNext} disabled={!data?.nextCursor || loading}>
              {t('infrastructure.dlq.next')}
            </Button>
          </Stack>
        </Card>
      )}
    </Stack>
  );
}
