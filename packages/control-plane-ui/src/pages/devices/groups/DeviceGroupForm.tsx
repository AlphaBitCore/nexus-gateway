import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import type { QueryKey } from '@tanstack/react-query';

import { Dialog, Button, Stack } from '@/components/ui';
import { useMutation } from '@/hooks/useMutation';
import { deviceGroupsApi, type DeviceGroup } from '@/api/services';

import { DeviceGroupBasicsFields } from './DeviceGroupBasicsFields';

interface Props {
  open: boolean;
  group: DeviceGroup | null;
  onClose: () => void;
  onSaved: () => void;
  /** Additional query key prefixes to invalidate after a successful save. */
  invalidateExtra?: readonly QueryKey[];
}

export function DeviceGroupForm({
  open,
  group,
  onClose,
  onSaved,
  invalidateExtra = [],
}: Props) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');

  useEffect(() => {
    setName(group?.name ?? '');
    setDescription(group?.description ?? '');
  }, [group, open]);

  const invalidateQueries: readonly QueryKey[] = [
    ['admin', 'device-groups'],
    ...invalidateExtra,
  ];

  const { mutate: save, loading } = useMutation(
    () =>
      group
        ? deviceGroupsApi.update(group.id, {
            name: name.trim(),
            description: description.trim() || undefined,
          })
        : deviceGroupsApi.create({
            name: name.trim(),
            description: description.trim() || undefined,
          }),
    {
      invalidateQueries,
      successMessage: group ? t('pages:deviceGroups.updateSuccess') : t('pages:deviceGroups.createSuccess'),
      onSuccess: () => {
        onSaved();
        onClose();
      },
    },
  );

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => !v && onClose()}
      title={group ? t('pages:deviceGroups.editTitle') : t('pages:deviceGroups.createTitle')}
      size="md"
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!name.trim()) return;
          void save(undefined);
        }}
      >
        <Stack gap="md">
          <DeviceGroupBasicsFields
            name={name}
            description={description}
            onNameChange={setName}
            onDescriptionChange={setDescription}
            disabled={loading}
          />
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button type="button" variant="secondary" onClick={onClose} disabled={loading}>
              {t('common:cancel')}
            </Button>
            <Button type="submit" variant="primary" loading={loading} disabled={!name.trim()}>
              {t('common:save')}
            </Button>
          </Stack>
        </Stack>
      </form>
    </Dialog>
  );
}
