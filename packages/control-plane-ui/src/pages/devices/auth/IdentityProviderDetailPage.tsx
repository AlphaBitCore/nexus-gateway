/**
 * Identity Provider detail page.
 *
 * Loads one IdP by id, renders an editable form (PUT) plus the SCIM
 * token + Group → IAM mapping subsections inline. Full page route,
 * not a Dialog.
 */
import { useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import type { IdentityProvider, IdentityProviderWriteRequest } from '@/api/types';
import {
  PageHeader,
  Breadcrumb,
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
      <Breadcrumb
        items={[
          { label: t('pages:identityProvider.title'), to: IDP_LIST_ROUTE },
          { label: data.name },
        ]}
      />
      <PageHeader
        title={data.name}
        subtitle={t('pages:identityProvider.idpId') + ': ' + data.id}
        action={
          !isLocal && (
            <Button variant="danger" onClick={() => setConfirmDelete(true)}>
              {t('common:delete', 'Delete')}
            </Button>
          )
        }
      />
      <div style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
        <Badge variant="info">{(data.type || '').toUpperCase()}</Badge>
        <Badge variant={data.enabled ? 'success' : 'default'}>
          {data.enabled ? t('pages:identityProvider.enabled') : t('pages:identityProvider.disabled')}
        </Badge>
      </div>

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

          <Card>
            <ScimTokenSection idp={data} />
          </Card>

          <Card>
            <GroupMappingSection idp={data} />
          </Card>
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
