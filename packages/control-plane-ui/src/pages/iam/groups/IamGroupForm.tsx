import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Dialog, FormField, Input, Button, Stack } from '@/components/ui';
import { useMutation } from '../../../hooks/useMutation';
import { iamApi } from '@/api/services';
import type { IamGroupUpdateInput, IamGroupWriteInput } from '@/api/services';
import type { IamGroup } from '../../../api/types';
import styles from './IamGroupForm.module.css';

interface IamGroupFormProps {
  group?: IamGroup;
  onClose: () => void;
  onSaved: () => void;
}

export function IamGroupForm({ group, onClose, onSaved }: IamGroupFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState(group?.name ?? '');
  const [description, setDescription] = useState(group?.description ?? '');

  const { mutate, loading } = useMutation(
    (data: IamGroupWriteInput | IamGroupUpdateInput) =>
      group ? iamApi.updateGroup(group.id, data) : iamApi.createGroup(data as IamGroupWriteInput),
    {
      onSuccess: () => { onSaved(); onClose(); },
      successMessage: group ? 'Group updated' : 'Group created',
    },
  );

  const handleSubmit = () => {
    mutate({ name, description: description || null });
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={group ? t('pages:iam.editGroup') : t('pages:iam.createGroup')}
    >
      <Stack gap="md">
        <FormField label={t('pages:iam.name')} required>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t('pages:iam.placeholderGroupName')}
          />
        </FormField>
        <FormField label={t('pages:iam.description')}>
          <Input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={t('pages:iam.placeholderOptionalDescription')}
          />
        </FormField>
        <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button
            onClick={handleSubmit}
            disabled={loading || !name}
          >
            {loading ? t('pages:iam.saving') : t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
