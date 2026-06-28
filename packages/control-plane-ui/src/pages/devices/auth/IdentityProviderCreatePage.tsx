/**
 * Add Identity Provider page.
 *
 * Full-page route (matches the system style for Add Provider, Add
 * Routing Rule, etc.) — not a modal Dialog. Renders a Breadcrumb +
 * PageHeader and delegates the form to IdentityProviderForm.
 */
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import type { IdentityProviderWriteRequest } from '@/api/types';
import { Stack } from '@/components/ui';
import { IDP_LIST_ROUTE } from './idpRoutes';
import { IdentityProviderForm } from './IdentityProviderForm';
import detailStyles from '../../iam/_shared/Iam.module.css';

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
      <section className={detailStyles.detailHeader}>
        <div className={detailStyles.detailHeaderRow}>
          <Link to={IDP_LIST_ROUTE} className={detailStyles.detailBackLink} aria-label={t('common:back')}>
            <svg className={detailStyles.detailBackIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={detailStyles.detailHeaderText}>
            <h1 className={detailStyles.detailTitle}>
              {t('pages:identityProvider.addIdp', 'Add Identity Provider')}
            </h1>
            <div className={detailStyles.detailMeta}>
              <span>
                {t('pages:identityProvider.wizard.intro', 'Connect an external IdP so your team can sign in with their company account.')}
              </span>
            </div>
          </div>
        </div>
      </section>
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
