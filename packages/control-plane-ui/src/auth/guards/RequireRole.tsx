import { Navigate, useLocation } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import { AuthSessionLoading } from '../context/AuthSessionLoading';
import styles from './RequireRole.module.css';

interface RequireRoleProps {
  children: React.ReactNode;
  allowedActions: string[];
}

export function RequireRole({ children, allowedActions }: RequireRoleProps) {
  const { t } = useTranslation('common');
  const { status, permissions } = useAuth();
  const location = useLocation();

  if (status === 'loading') {
    return <AuthSessionLoading />;
  }
  if (status === 'unauthenticated') {
    return <Navigate to="/login" state={{ from: location }} replace />;
  }
  if (!permissions.length || !allowedActions.some(a => permissions.includes(a))) {
    return (
      <div className={styles.container}>
        <h2 className={styles.heading}>{t('accessDenied')}</h2>
        <p className={styles.message}>{t('accessDeniedMessage')}</p>
      </div>
    );
  }

  return <>{children}</>;
}
