import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import { useToast } from '@/context/ToastContext';

/**
 * Returns a callback that shows best-effort sync feedback after a config save.
 * Queries the admin API for the number of online nodes of the given node type
 * and displays an informational toast. Failures are silently swallowed so they
 * never interfere with the actual save flow.
 */
export function useSyncFeedback() {
  const { t } = useTranslation();
  const { addToast } = useToast();

  return useCallback(
    async (nodeType: string) => {
      try {
        const data = await hubApi.listNodes({ type: nodeType, status: 'online', pageSize: 1 });
        const online = data.total;
        if (online > 0) {
          addToast(
            t('pages:configSync.syncFeedback', { count: online, type: nodeType }),
            'info',
          );
        }
      } catch {
        // Best-effort — admin API may be unreachable; don't disrupt the save flow.
      }
    },
    [t, addToast],
  );
}
