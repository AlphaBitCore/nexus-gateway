/**
 * useExemptionDialogs — owns the open/selected/note/busy state for the Delete,
 * Approve, and Reject dialogs of ExemptionsPage, plus their submit handlers.
 * The page threads the hook's setters into the DataTable actions column and the
 * hook's state + handlers into the co-located dialog components.
 */
import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { complianceApi } from '@/api/services/compliance/compliance';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';
import { useToast } from '@/context/ToastContext';

export interface UseExemptionDialogs {
  deleteTarget: UnifiedExemptionRow | null;
  setDeleteTarget: (row: UnifiedExemptionRow | null) => void;
  approveTarget: UnifiedExemptionRow | null;
  setApproveTarget: (row: UnifiedExemptionRow | null) => void;
  rejectTarget: UnifiedExemptionRow | null;
  setRejectTarget: (row: UnifiedExemptionRow | null) => void;
  rejectNote: string;
  setRejectNote: (note: string) => void;
  approveBusy: boolean;
  rejectBusy: boolean;
  handleDelete: () => Promise<void>;
  handleApprove: () => Promise<void>;
  handleReject: () => Promise<void>;
}

export function useExemptionDialogs(refetch: () => void): UseExemptionDialogs {
  const { t } = useTranslation();
  const { addToast } = useToast();

  const [deleteTarget, setDeleteTarget] = useState<UnifiedExemptionRow | null>(null);
  const [approveTarget, setApproveTarget] = useState<UnifiedExemptionRow | null>(null);
  const [rejectTarget, setRejectTarget] = useState<UnifiedExemptionRow | null>(null);
  const [rejectNote, setRejectNote] = useState('');
  const [approveBusy, setApproveBusy] = useState(false);
  const [rejectBusy, setRejectBusy] = useState(false);

  const handleDelete = useCallback(async () => {
    if (!deleteTarget) return;
    try {
      await complianceApi.deleteExemptionGrant(deleteTarget.id);
      addToast(t('pages:compliance.exemptions.deleteSuccess', 'Exemption deleted'), 'success');
      setDeleteTarget(null);
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.deleteError', { error: msg }), 'error');
    }
  }, [deleteTarget, addToast, t, refetch]);

  const handleApprove = useCallback(async () => {
    if (!approveTarget) return;
    setApproveBusy(true);
    try {
      await complianceApi.approveExemption(approveTarget.id);
      addToast(t('pages:compliance.exemptions.approveSuccess'), 'success');
      setApproveTarget(null);
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.approveError', { error: msg }), 'error');
    } finally {
      setApproveBusy(false);
    }
  }, [approveTarget, addToast, t, refetch]);

  const handleReject = useCallback(async () => {
    if (!rejectTarget) return;
    const reason = rejectNote.trim();
    if (reason.length < 2) {
      addToast(t('pages:compliance.exemptions.validation.rejectReasonRequired'), 'error');
      return;
    }
    setRejectBusy(true);
    try {
      await complianceApi.rejectExemption(rejectTarget.id, reason);
      addToast(t('pages:compliance.exemptions.rejectSuccess'), 'success');
      setRejectTarget(null);
      setRejectNote('');
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.rejectError', { error: msg }), 'error');
    } finally {
      setRejectBusy(false);
    }
  }, [rejectTarget, rejectNote, addToast, t, refetch]);

  return {
    deleteTarget,
    setDeleteTarget,
    approveTarget,
    setApproveTarget,
    rejectTarget,
    setRejectTarget,
    rejectNote,
    setRejectNote,
    approveBusy,
    rejectBusy,
    handleDelete,
    handleApprove,
    handleReject,
  };
}
