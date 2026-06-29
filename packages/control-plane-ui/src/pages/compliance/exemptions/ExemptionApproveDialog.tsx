/**
 * ExemptionApproveDialog — confirmation AlertDialog for approving a PENDING
 * exemption request. Controlled by useExemptionDialogs (the page owns the
 * selected row, busy flag, and approve handler); this component is a thin
 * render of that state.
 */
import { useTranslation } from 'react-i18next';
import { AlertDialog } from '@/components/ui';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';

interface ExemptionApproveDialogProps {
  approveTarget: UnifiedExemptionRow | null;
  setApproveTarget: (row: UnifiedExemptionRow | null) => void;
  approveBusy: boolean;
  onConfirm: () => void;
}

export function ExemptionApproveDialog({
  approveTarget,
  setApproveTarget,
  approveBusy,
  onConfirm,
}: ExemptionApproveDialogProps) {
  const { t } = useTranslation();
  return (
    <AlertDialog
      open={approveTarget !== null}
      onOpenChange={(open) => {
        if (!open) setApproveTarget(null);
      }}
      title={t('pages:compliance.exemptions.approveTitle')}
      description={
        approveTarget
          ? t('pages:compliance.exemptions.approveDesc', {
              sourceIp: approveTarget.sourceIp,
              targetHost: approveTarget.targetHost,
            })
          : ''
      }
      confirmLabel={t('pages:compliance.exemptions.approveConfirm')}
      cancelLabel={t('common:cancel', 'Cancel')}
      onConfirm={() => {
        void onConfirm();
      }}
      loading={approveBusy}
      variant="default"
    />
  );
}
