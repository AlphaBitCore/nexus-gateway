/**
 * Visible reminder: stage-1 primary routing is winner-takes-all by priority.
 */

import { useTranslation } from 'react-i18next';
import { useRoutingFieldHelp } from './routing-rule-field-help';
import styles from './RoutingPrimaryWinnerCallout.module.css';

export function RoutingPrimaryWinnerCallout() {
  const { t } = useTranslation();
  const help = useRoutingFieldHelp();
  return (
    <div className={styles.calloutBox} role="note">
      <strong className={styles.calloutTitle}>{t('pages:routing.primaryWinnerCalloutTitle')} </strong>
      {help.primaryWinnerCallout}
    </div>
  );
}
