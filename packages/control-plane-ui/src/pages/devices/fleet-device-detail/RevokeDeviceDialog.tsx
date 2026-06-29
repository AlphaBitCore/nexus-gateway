import { useTranslation } from 'react-i18next';
import { Dialog, Stack, Button } from '@/components/ui';
import styles from '../FleetDeviceDetailPage.module.css';

interface RevokeDeviceDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
  loading: boolean;
}

export function RevokeDeviceDialog({ open, onOpenChange, onConfirm, loading }: RevokeDeviceDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title={t('pages:fleet.revokeDeviceConfirmTitle')} className={styles.fleetDialog}>
      <Stack gap="md">
        <p>{t('pages:fleet.revokeDeviceConfirmBody')}</p>
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>{t('common:cancel')}</Button>
          <Button variant="danger" onClick={onConfirm} loading={loading}>{t('pages:fleet.revokeDevice')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
