import { useTranslation } from 'react-i18next';
import { Dialog, Stack, Button, Input } from '@/components/ui';
import styles from '../FleetDeviceDetailPage.module.css';

interface DiagModeDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  diagPreset: '30m' | '2h' | '8h' | null;
  diagReason: string;
  onReasonChange: (reason: string) => void;
  onConfirm: () => void;
}

export function DiagModeDialog({
  open,
  onOpenChange,
  diagPreset,
  diagReason,
  onReasonChange,
  onConfirm,
}: DiagModeDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title={t('pages:fleet.diagMode')} className={styles.fleetDialog}>
      <Stack gap="md">
        <p>
          {diagPreset === '30m' && t('pages:fleet.diagModeEnable30m')}
          {diagPreset === '2h' && t('pages:fleet.diagModeEnable2h')}
          {diagPreset === '8h' && t('pages:fleet.diagModeEnable8h')}
        </p>
        <label className={styles.kvLabel}>{t('pages:fleet.diagModeReasonLabel')}</label>
        <Input
          placeholder={t('pages:fleet.diagModeReasonPlaceholder')}
          value={diagReason}
          onChange={(e) => onReasonChange(e.target.value)}
        />
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>{t('common:cancel')}</Button>
          <Button onClick={onConfirm}>{t('pages:fleet.diagMode')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
