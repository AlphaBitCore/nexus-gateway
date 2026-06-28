import { useTranslation } from 'react-i18next';
import type { DiagModeWindow } from '@/api/services/infrastructure/diag/diagmode';
import {
  Stack, Card, Button, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import { fmtEndsIn, fmtTime } from './diagModeHelpers';
import styles from './InfraDiagModePage.module.css';

interface ActiveWindowsSectionProps {
  windows: DiagModeWindow[];
  error: Error | null;
  loading: boolean;
  refetch: () => void;
  setConfirmDisable: (w: DiagModeWindow) => void;
}

export function ActiveWindowsSection({
  windows,
  error,
  loading,
  refetch,
  setConfirmDisable,
}: ActiveWindowsSectionProps) {
  const { t } = useTranslation('pages');

  return (
    /* ── Active windows ── */
    <section className={styles.contentSection}>
      <div className={styles.actionRow} style={{ justifyContent: 'space-between' }}>
        <h3 className={styles.sectionTitle}>{t('infrastructure.diagMode.activeWindows')}</h3>
        <div className={styles.activeWindowActions}>
          <span className={styles.previewBanner}>
            {t('infrastructure.diagMode.autoRefresh')}
          </span>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={() => refetch()}
          >
            {t('infrastructure.diagMode.refresh')}
          </Button>
        </div>
      </div>
      <Card>
        <Stack gap="sm">
          {error ? (
            <ErrorBanner
              message={error.message}
              onRetry={refetch}
            />
          ) : loading && windows.length === 0 ? (
            <LoadingSpinner />
          ) : windows.length === 0 ? (
            <div className={styles.empty}>
              {t('infrastructure.diagMode.activeEmpty')}
            </div>
          ) : (
            <table className={styles.dataTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.diagMode.colThing')}</th>
                  <th>{t('infrastructure.diagMode.colStarted')}</th>
                  <th>{t('infrastructure.diagMode.colEndsIn')}</th>
                  <th>{t('infrastructure.diagMode.colSetBy')}</th>
                  <th>{t('infrastructure.diagMode.colReason')}</th>
                  <th>{t('infrastructure.diagMode.colActions')}</th>
                </tr>
              </thead>
              <tbody>
                {windows.map((w) => (
                  <tr key={w.id}>
                    <td className={styles.codeCell}>{w.nodeId}</td>
                    <td>{fmtTime(w.startedAt)}</td>
                    <td>{fmtEndsIn(w.endedAt)}</td>
                    <td>{w.setBy ?? '—'}</td>
                    <td>{w.reason ?? '—'}</td>
                    <td>
                      <Button
                        type="button"
                        variant="secondary"
                        size="sm"
                        onClick={() => setConfirmDisable(w)}
                      >
                        {t('infrastructure.diagMode.disable')}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Stack>
      </Card>
    </section>
  );
}
