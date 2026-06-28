/**
 * ExemptionRejectDialog — reject dialog for a PENDING exemption request,
 * collecting a required reject note. Controlled by useExemptionDialogs (the page
 * owns the selected row, note text, busy flag, and reject handler); this
 * component is a thin render of that state.
 */
import { useTranslation } from 'react-i18next';
import { Button, Dialog, FormField, Stack, Textarea } from '@/components/ui';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';

interface ExemptionRejectDialogProps {
  rejectTarget: UnifiedExemptionRow | null;
  setRejectTarget: (row: UnifiedExemptionRow | null) => void;
  rejectNote: string;
  setRejectNote: (note: string) => void;
  rejectBusy: boolean;
  onConfirm: () => void;
}

export function ExemptionRejectDialog({
  rejectTarget,
  setRejectTarget,
  rejectNote,
  setRejectNote,
  rejectBusy,
  onConfirm,
}: ExemptionRejectDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog
      open={rejectTarget !== null}
      onOpenChange={(open) => {
        if (!open) {
          setRejectTarget(null);
          setRejectNote('');
        }
      }}
      title={t('pages:compliance.exemptions.rejectTitle')}
      description={t('pages:compliance.exemptions.rejectDesc')}
    >
      <Stack gap="md">
        <FormField label={t('pages:compliance.exemptions.rejectReasonLabel')}>
          <Textarea
            value={rejectNote}
            onChange={(e) => setRejectNote(e.target.value)}
            placeholder={t('pages:compliance.exemptions.rejectReasonPlaceholder')}
            rows={3}
          />
        </FormField>
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button
            variant="ghost"
            onClick={() => {
              setRejectTarget(null);
              setRejectNote('');
            }}
            disabled={rejectBusy}
          >
            {t('common:cancel', 'Cancel')}
          </Button>
          <Button
            variant="danger"
            onClick={() => void onConfirm()}
            disabled={rejectBusy || rejectNote.trim().length < 2}
          >
            {rejectBusy
              ? t('pages:compliance.exemptions.rejectSubmitting')
              : t('pages:compliance.exemptions.rejectSubmit')}
          </Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
