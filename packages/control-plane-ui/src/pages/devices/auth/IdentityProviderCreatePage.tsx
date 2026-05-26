/**
 * Add Identity Provider page.
 *
 * Full-page route (matches the system style for Add Provider, Add
 * Routing Rule, etc.) — not a modal Dialog. Renders a Breadcrumb +
 * PageHeader and delegates the form to IdentityProviderForm.
 */
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import type { IdentityProviderWriteRequest } from '@/api/types';
import { PageHeader, Breadcrumb, Stack } from '@/components/ui';
import { IDP_LIST_ROUTE } from './idpRoutes';
import { IdentityProviderForm } from './IdentityProviderForm';

export function IdentityProviderCreatePage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [submitError, setSubmitError] = useState<string | null>(null);

  const { mutate: doCreate, loading: creating } = useMutation(
    (body: IdentityProviderWriteRequest) => iamApi.createIdentityProvider(body),
    {
      onSuccess: (created) => navigate(`${IDP_LIST_ROUTE}/${created.id}`),
      onError: (e) => setSubmitError(e.message),
    },
  );

  return (
    <Stack gap="md">
      <Breadcrumb
        items={[
          { label: t('pages:identityProvider.title'), to: IDP_LIST_ROUTE },
          { label: t('pages:identityProvider.addIdp', 'Add Identity Provider') },
        ]}
      />
      <PageHeader
        title={t('pages:identityProvider.addIdp', 'Add Identity Provider')}
        subtitle={t('pages:identityProvider.wizard.intro', 'Connect an external IdP (Okta, Azure AD, Google Workspace, …) so your team can sign in with their company account. Nexus is the Service Provider; the IdP is configured at the IdP vendor first.')}
      />
      <IdentityProviderForm
        mode="create"
        submitting={creating}
        submitError={submitError}
        onSubmit={(body) => { setSubmitError(null); void doCreate(body); }}
        onCancel={() => navigate(IDP_LIST_ROUTE)}
      />
    </Stack>
  );
}
