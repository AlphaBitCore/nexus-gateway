/**
 * Identity Provider detail page.
 *
 * Loads one IdP by id, renders an editable form (PUT) plus the SCIM
 * token + Group → IAM mapping subsections inline. Full page route,
 * not a Dialog.
 */
import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import type { IdentityProvider, IdentityProviderWriteRequest } from '@/api/types';
import {
  Stack,
  Skeleton,
  ErrorBanner,
  Card,
  Button,
  AlertDialog,
  Badge,
} from '@/components/ui';
import { IDP_LIST_ROUTE } from './idpRoutes';
import { IdentityProviderForm } from './IdentityProviderForm';
import { ScimTokenSection, GroupMappingSection } from './IdentityProviderPage';
import detailStyles from '../../iam/_shared/Iam.module.css';

export function IdentityProviderDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const params = useParams<{ id: string }>();
  const idpId = params.id ?? '';
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const { data, loading, error, refetch } = useApi<IdentityProvider>(
    () => iamApi.getIdentityProvider(idpId),
    ['admin', 'identity-providers', 'detail', idpId],
  );

  const { mutate: doUpdate, loading: updating } = useMutation(
    (body: IdentityProviderWriteRequest) => iamApi.updateIdentityProvider(idpId, body),
    {
      onSuccess: () => { setSubmitError(null); refetch(); },
      onError: (e) => setSubmitError(e.message),
    },
  );

  const { mutate: doDelete, loading: deleting } = useMutation(
    () => iamApi.deleteIdentityProvider(idpId),
    {
      onSuccess: () => { setConfirmDelete(false); navigate(IDP_LIST_ROUTE); },
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  const isLocal = data.type === 'local';

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
            <h1 className={detailStyles.detailTitle}>{data.name}</h1>
            <div className={detailStyles.detailMeta}>
              <span>{t('pages:identityProvider.idpId')}: {data.id}</span>
              <Badge variant="info">{(data.type || '').toUpperCase()}</Badge>
              <Badge variant={data.enabled ? 'success' : 'default'}>
                {data.enabled ? t('pages:identityProvider.enabled') : t('pages:identityProvider.disabled')}
              </Badge>
            </div>
          </div>
          {!isLocal && (
            <div className={detailStyles.detailHeaderActions}>
            <Button variant="danger" onClick={() => setConfirmDelete(true)}>
              {t('common:delete', 'Delete')}
            </Button>
            </div>
          )}
        </div>
      </section>

      {isLocal ? (
        <Card>
          <p style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-base)', color: 'var(--color-text-muted)' }}>
            {t('pages:identityProvider.localIdpNote')}
          </p>
        </Card>
      ) : (
        <>
          <IdentityProviderForm
            mode="edit"
            initial={data}
            submitting={updating}
            submitError={submitError}
            onSubmit={(body) => { setSubmitError(null); void doUpdate(body); }}
            onCancel={() => navigate(IDP_LIST_ROUTE)}
          />

          <ScimTokenSection idp={data} />

          <GroupMappingSection idp={data} />
        </>
      )}

      <AlertDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t('pages:identityProvider.confirmDeleteIdpTitle', 'Delete Identity Provider')}
        description={t('pages:identityProvider.confirmDeleteIdpBody', 'Users linked to this IdP will lose access on their next request. SCIM tokens scoped to this IdP will be revoked. This action cannot be undone.')}
        confirmLabel={t('common:delete', 'Delete')}
        variant="danger"
        onConfirm={() => void doDelete(undefined)}
        loading={deleting}
      />
    </Stack>
  );
}
