/**
 * Card — Recent admin negative-feedback ("Mark as bad cache hit").
 *
 * Surfaces the in-process ring buffer of L2-semantic-cache feedback entries
 * the Traffic Audit Drawer collects when an admin clicks "Mark as bad
 * cache hit". Without this panel admins had no way to audit which hits
 * were flagged or by whom — only the side effect (poison list entry) was
 * visible.
 *
 * Source: GET /api/admin/cache/semantic-feedback?limit=100 — Control Plane
 * process-scoped buffer, capped server-side at 1000 entries. Restarting CP
 * clears the buffer; the poison list itself lives in Redis and is durable.
 */
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { Badge, Button, Card, ErrorBanner, Skeleton, Stack } from '@/components/ui';
import {
  semanticFeedbackApi,
  type FeedbackEntry,
  type ListFeedbackResponse,
} from '@/api/services/cache/semanticFeedback';
import { formatDateTime } from '@/lib/format';

export function RecentFeedbackCard() {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<ListFeedbackResponse>(
    () => semanticFeedbackApi.listFeedback(100),
    ['admin', 'semantic-feedback', 'list'],
  );

  return (
    <Card>
      <Stack gap="md">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
          <div>
            <h3>{t('pages:aiGateway.cache.recentFeedback.title')}</h3>
            <p style={{ color: 'var(--g-color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}>
              {t('pages:aiGateway.cache.recentFeedback.subtitle')}
            </p>
          </div>
          <Button variant="secondary" onClick={() => refetch()} disabled={loading}>
            {t('common:refresh')}
          </Button>
        </div>

        {loading && <Skeleton.Box width="100%" height={120} />}
        {error && <ErrorBanner message={error.message} />}
        {!loading && !error && (data?.entries.length ?? 0) === 0 && (
          <div style={{ color: 'var(--g-color-text-muted)', padding: 'var(--g-space-md)' }}>
            {t('pages:aiGateway.cache.recentFeedback.empty')}
          </div>
        )}
        {!loading && (data?.entries.length ?? 0) > 0 && (
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={cellHead}>{t('pages:aiGateway.cache.recentFeedback.colWhen')}</th>
                <th style={cellHead}>{t('pages:aiGateway.cache.recentFeedback.colActor')}</th>
                <th style={cellHead}>{t('pages:aiGateway.cache.recentFeedback.colVKScope')}</th>
                <th style={cellHead}>{t('pages:aiGateway.cache.recentFeedback.colReason')}</th>
              </tr>
            </thead>
            <tbody>
              {data!.entries
                .slice()
                .reverse()
                .map((entry: FeedbackEntry) => (
                  <tr key={`${entry.createdAt}-${entry.entryKey}`}>
                    <td style={cellBody}>{formatDateTime(entry.createdAt)}</td>
                    <td style={cellBody}>{entry.actorId || '—'}</td>
                    <td style={cellBody}>
                      {entry.vkScope ? (
                        <Badge variant="default">{entry.vkScope}</Badge>
                      ) : (
                        <span style={{ color: 'var(--g-color-text-muted)' }}>—</span>
                      )}
                    </td>
                    <td style={cellBody}>{entry.reason}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
      </Stack>
    </Card>
  );
}

const cellHead = {
  textAlign: 'left' as const,
  padding: 'var(--g-space-sm)',
  fontSize: 'var(--g-font-size-sm)',
  color: 'var(--g-color-text-muted)',
  borderBottom: '1px solid var(--g-color-border-subtle)',
};

const cellBody = {
  padding: 'var(--g-space-sm)',
  fontSize: 'var(--g-font-size-sm)',
  borderBottom: '1px solid var(--g-color-border-subtle)',
};
