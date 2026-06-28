/**
 * ExemptionDeleteDialog — confirmation AlertDialog for deleting a
 * pre-activation exemption grant. Controlled by useExemptionDialogs (the page
 * owns the selected row + delete handler); this component is a thin render of
 * that state.
 */
import { useTranslation } from 'react-i18next';
import { AlertDialog } from '@/components/ui';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';

interface ExemptionDeleteDialogProps {
  deleteTarget: UnifiedExemptionRow | null;
  setDeleteTarget: (row: UnifiedExemptionRow | null) => void;
  onConfirm: () => void;
}

export function ExemptionDeleteDialog({
  deleteTarget,
  setDeleteTarget,
  onConfirm,
}: ExemptionDeleteDialogProps) {
  const { t } = useTranslation();
  return (
    <AlertDialog
      open={deleteTarget !== null}
      onOpenChange={(open) => {
        if (!open) setDeleteTarget(null);
      }}
      title={t('pages:compliance.exemptions.deleteTitle', 'Delete exemption?')}
      description={
        deleteTarget
          ? t('pages:compliance.exemptions.deleteDesc', {
              sourceIp: deleteTarget.sourceIp,
              targetHost: deleteTarget.targetHost,
            })
          : ''
      }
      confirmLabel={t('common:delete', 'Delete')}
      cancelLabel={t('common:cancel', 'Cancel')}
      onConfirm={onConfirm}
      variant="danger"
    />
  );
}
