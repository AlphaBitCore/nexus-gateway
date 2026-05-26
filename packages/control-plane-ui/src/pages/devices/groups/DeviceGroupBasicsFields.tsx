import { useTranslation } from 'react-i18next';

import { FormField, Input, Stack, Textarea } from '@/components/ui';

export interface DeviceGroupBasicsFieldsProps {
  name: string;
  description: string;
  onNameChange: (value: string) => void;
  onDescriptionChange: (value: string) => void;
  disabled?: boolean;
}

/**
 * Shared name + description fields for device group create, edit dialog,
 * and full-page create flow.
 */
export function DeviceGroupBasicsFields({
  name,
  description,
  onNameChange,
  onDescriptionChange,
  disabled,
}: DeviceGroupBasicsFieldsProps) {
  const { t } = useTranslation();

  return (
    <Stack gap="md">
      <FormField label={t('pages:deviceGroups.name')} required>
        <Input
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          placeholder={t('pages:deviceGroups.namePlaceholder')}
          disabled={disabled}
          autoComplete="off"
        />
      </FormField>
      <FormField
        label={t('pages:deviceGroups.description')}
        helpText={t('pages:deviceGroups.descriptionHelp')}
      >
        <Textarea
          value={description}
          onChange={(e) => onDescriptionChange(e.target.value)}
          placeholder={t('pages:deviceGroups.descriptionPlaceholder')}
          rows={4}
          disabled={disabled}
        />
      </FormField>
    </Stack>
  );
}
