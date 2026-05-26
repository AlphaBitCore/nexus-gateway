import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { PageHeader, Breadcrumb, Stack } from '@/components/ui';
import { HookForm } from '../form/HookForm';
import type { HookConfig } from '@/api/types';

export function HookCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:hooks.title', 'Hooks'), to: '/compliance/hooks' },
        { label: t('pages:hooks.createHook') },
      ]} />
      <PageHeader
        title={t('pages:hooks.createHook')}
        subtitle={t('pages:hooks.createSubtitle', 'Configure a request-stage, response-stage, or webhook hook for the gateway pipeline.')}
      />
      <HookForm
        embedded
        onClose={() => navigate('/compliance/hooks')}
        onSaved={() => {}}
        onCreateSuccess={(created: HookConfig) => navigate(`/compliance/hooks/${created.id}`)}
      />
    </Stack>
  );
}
