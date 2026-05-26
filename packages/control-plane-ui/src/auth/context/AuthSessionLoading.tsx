import { LoadingSpinner } from '@/components/ui';
import styles from './AuthSessionLoading.module.css';

/**
 * Full-viewport placeholder while session / role is resolved (whoami).
 * Shared by RequireAuth and RequireRole so RBAC-gated routes do not flash empty content.
 */
export function AuthSessionLoading() {
  return (
    <div className={styles.viewport} role="status" aria-live="polite" aria-busy="true">
      <LoadingSpinner size="lg" />
    </div>
  );
}
