import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Dialog, FormField, Input, Button, Stack, Tooltip,
} from '@/components/ui';
import { useMutation } from '../../../hooks/useMutation';
import { iamApi } from '@/api/services';
import type { IamGroupUpdateInput, IamGroupWriteInput } from '@/api/services';
import type { IamGroup, IamPolicy } from '../../../api/types';
import styles from './IamRoleForm.module.css';

interface IamRoleFormProps {
  role?: IamGroup;
  onClose: () => void;
  onSaved: () => void;
}

export function IamRoleForm({ role, onClose, onSaved }: IamRoleFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState(role?.name ?? '');
  const [description, setDescription] = useState(role?.description ?? '');
  const [policies, setPolicies] = useState<IamPolicy[]>([]);
  const [selectedPolicyIds, setSelectedPolicyIds] = useState<Set<string>>(new Set());
  const [policiesLoading, setPoliciesLoading] = useState(true);

  useEffect(() => {
    iamApi.listPolicies().then((res) => {
      setPolicies(res.data);
      setPoliciesLoading(false);
    }).catch(() => {
      setPoliciesLoading(false);
    });
  }, []);

  const togglePolicy = (policyId: string) => {
    setSelectedPolicyIds((prev) => {
      const next = new Set(prev);
      if (next.has(policyId)) {
        next.delete(policyId);
      } else {
        next.add(policyId);
      }
      return next;
    });
  };

  const { mutate: saveRole, loading } = useMutation(
    (data: IamGroupWriteInput | IamGroupUpdateInput) =>
      role ? iamApi.updateGroup(role.id, data) : iamApi.createGroup(data as IamGroupWriteInput),
    {
      onSuccess: async (result) => {
        if (!role && result?.id) {
          // Attach selected policies to the newly created role
          for (const policyId of selectedPolicyIds) {
            await iamApi.addGroupPolicy(result.id, { policyId });
          }
        }
        onSaved();
        onClose();
      },
      successMessage: role ? 'Role updated' : 'Role created',
    },
  );

  const handleSubmit = () => {
    saveRole({ name, description: description || null });
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={role ? t('pages:iam.editRole') : t('pages:iam.createRole')}
    >
      <Stack gap="md">
        <FormField label={t('pages:iam.name')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </FormField>
        <FormField label={t('pages:iam.description')}>
          <Input value={description} onChange={(e) => setDescription(e.target.value)} />
        </FormField>

        {!role && (
          <div className={styles.policySection}>
            <div className={styles.policyLabel}>
              {t('pages:iam.attachPolicies')}
              <Tooltip content="Optional shortcuts: selected policies are linked immediately after the role is created. You can add or remove attachments later from the role detail page.">
                <button type="button" aria-label={t('pages:iam.helpAttachPolicies')} className={styles.helpIconBtn}>&#9432;</button>
              </Tooltip>
            </div>
            <div className={styles.policyList}>
              {policiesLoading ? (
                <div className={styles.policyEmpty}>{t('pages:iam.loadingPolicies')}</div>
              ) : policies.length === 0 ? (
                <div className={styles.policyEmpty}>{t('pages:iam.noPoliciesAvailable')}</div>
              ) : (
                policies.map((policy) => (
                  <label key={policy.id} className={styles.policyItem}>
                    <input
                      type="checkbox"
                      checked={selectedPolicyIds.has(policy.id)}
                      onChange={() => togglePolicy(policy.id)}
                    />
                    <span>{policy.name}</span>
                    <span className={policy.type === 'managed' ? styles.typeBadgeManaged : styles.typeBadgeCustom}>
                      {policy.type}
                    </span>
                  </label>
                ))
              )}
            </div>
          </div>
        )}

        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button onClick={handleSubmit} loading={loading} disabled={!name}>{t('common:save')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
