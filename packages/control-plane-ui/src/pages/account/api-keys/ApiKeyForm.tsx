import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Dialog, FormField, Input, Select, Button, Stack,
} from '@/components/ui';
import { useMutation } from '../../../hooks/useMutation';
import { useApi } from '../../../hooks/useApi';
import { iamApi } from '@/api/services';
import type { CreateAdminApiKeyInput } from '@/api/services';
import styles from './ApiKeyForm.module.css';

interface ApiKeyFormProps {
  onClose: () => void;
  onCreated: () => void;
  /** Settings page: hide owner picker; server sets owner to the signed-in user */
  settingsMode?: boolean;
}

interface CreateKeyResponse {
  key: string;
  name: string;
}

export function ApiKeyForm({ onClose, onCreated, settingsMode }: ApiKeyFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [expiresAt, setExpiresAt] = useState('');
  const [ownerUserId, setOwnerUserId] = useState('');
  const [rawKey, setRawKey] = useState<string | null>(null);

  const { data: usersData } = useApi<{ data: Array<{ id: string; displayName: string }> }>(
    () => iamApi.listUsers(),
    ['admin', 'iam', 'users', 'list', 'api-key-form'],
    { skip: !!settingsMode },
  );

  const { mutate, loading } = useMutation(
    (data: CreateAdminApiKeyInput) => iamApi.createApiKey(data) as Promise<CreateKeyResponse>,
    {
      onSuccess: (result) => {
        setRawKey(result.key);
        onCreated();
      },
      successMessage: t('pages:settings.apiKeyCreatedMsg'),
    },
  );

  const handleSubmit = () => {
    mutate({
      name,
      expiresAt: expiresAt || undefined,
      ...(!settingsMode && ownerUserId ? { ownerUserId } : {}),
    });
  };

  if (rawKey) {
    return (
      <Dialog
        open
        onOpenChange={(open) => { if (!open) onClose(); }}
        title={t('pages:settings.apiKeyCreated')}
        size="md"
      >
        <Stack gap="md">
          <p className={styles.warningText}>{t('pages:settings.copyKeyWarning')}</p>
          <code className={styles.rawKeyBlock}>{rawKey}</code>
          <Button variant="secondary" onClick={() => navigator.clipboard.writeText(rawKey)}>
            {t('pages:settings.copyToClipboard')}
          </Button>
          <Stack direction="horizontal" justify="end">
            <Button onClick={onClose}>{t('common:done')}</Button>
          </Stack>
        </Stack>
      </Dialog>
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={t('pages:settings.createApiKey')}
      size="md"
    >
      <Stack gap="md">
        <FormField label={t('pages:settings.apiKeyNameLabel')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </FormField>

        {!settingsMode && (
          <FormField label={t('pages:settings.apiKeyOwnerLabel')} helpText={t('pages:settings.apiKeyOwnerHelpText')}>
            <Select
              value={ownerUserId}
              onValueChange={setOwnerUserId}
              placeholder={t('pages:settings.noOwner')}
              options={(usersData?.data ?? []).map(u => ({ value: u.id, label: u.displayName }))}
            />
          </FormField>
        )}

        <FormField label={t('pages:settings.apiKeyExpiresAtLabel')} helpText={t('pages:settings.apiKeyExpiresAtHelpText')}>
          <Input
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
            placeholder={t('pages:settings.placeholderExpiresAt')}
          />
        </FormField>

        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button onClick={handleSubmit} loading={loading} disabled={!name}>{t('common:create')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
