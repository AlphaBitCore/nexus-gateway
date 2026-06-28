import { useTranslation } from 'react-i18next';
import { Dialog, Stack, Button } from '@/components/ui';
import styles from '../FleetDeviceDetailPage.module.css';

interface RotateCertDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
  loading: boolean;
}

export function RotateCertDialog({ open, onOpenChange, onConfirm, loading }: RotateCertDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title={t('pages:fleet.rotateCertConfirmTitle')} className={styles.fleetDialog}>
      <Stack gap="md">
        <p>{t('pages:fleet.rotateCertConfirmBody')}</p>
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>{t('common:cancel')}</Button>
          <Button onClick={onConfirm} loading={loading}>{t('pages:fleet.rotateCert')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
