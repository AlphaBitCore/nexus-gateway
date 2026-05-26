import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

import { deviceGroupsApi, type CreateDeviceGroupInput } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import { Breadcrumb, Button, Card, ErrorBanner, PageHeader, Stack } from '@/components/ui';

import { DeviceGroupBasicsFields } from './DeviceGroupBasicsFields';

export function DeviceGroupCreatePage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const canCreate = usePermission('device-groups:create');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');

  const dirty = name.trim().length > 0 || description.trim().length > 0;
  useUnsavedChangesWarning(dirty);

  const { mutate: createGroup, loading } = useMutation(
    (input: CreateDeviceGroupInput) => deviceGroupsApi.create(input),
    {
      invalidateQueries: [['admin', 'device-groups']],
      successMessage: t('pages:deviceGroups.createSuccess'),
      onSuccess: (created) => {
        navigate(`/devices/groups/${created.id}`);
      },
    },
  );

  if (!canCreate) {
    return (
      <ErrorBanner
        message={t('pages:deviceGroups.createForbidden')}
      />
    );
  }

  return (
    <Stack gap="lg">
      <Breadcrumb
        items={[
          { label: t('pages:deviceGroups.title'), to: '/devices/groups' },
          { label: t('pages:deviceGroups.create') },
        ]}
      />

      <PageHeader title={t('pages:deviceGroups.createTitle')} subtitle={t('pages:deviceGroups.createSubtitle')} />

      <Card>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!name.trim()) return;
            void createGroup({
              name: name.trim(),
              description: description.trim() || undefined,
            });
          }}
        >
          <Stack gap="lg">
            <DeviceGroupBasicsFields
              name={name}
              description={description}
              onNameChange={setName}
              onDescriptionChange={setDescription}
              disabled={loading}
            />
            <Stack direction="horizontal" gap="sm" justify="end">
              <Button
                type="button"
                variant="secondary"
                onClick={() => navigate('/devices/groups')}
                disabled={loading}
              >
                {t('common:cancel')}
              </Button>
              <Button type="submit" variant="primary" disabled={loading || !name.trim()} loading={loading}>
                {t('pages:deviceGroups.createSubmit')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
