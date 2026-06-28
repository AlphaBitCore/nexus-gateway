import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi, serviceUrlsApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import type { IdentityProvider, ScimToken, IdpGroupMapping, IamGroup } from '@/api/types';
import {
  PageHeader, Card, Badge, Button, Skeleton, ErrorBanner,
  DataTable, Dialog, FormField, Input, SecretDialog,
  AlertDialog,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { formatDate } from '@/lib/format';
import { IDP_NEW_ROUTE, idpDetailRoute } from './idpRoutes';

/* ── SCIM endpoint base URL ─────────────────────────────────────────────── */

const SCIM_PATH = '/scim/v2';

/**
 * Build the externally-reachable SCIM endpoint URL from the Control
 * Plane's reported publicURL. Falls back to the window-based heuristic
 * (treating dev port 3000 as a Vite proxy to CP 3001) only when the
 * publicURLs API hasn't responded yet, so the page never renders an
 * empty path while loading.
 */
function scimBaseUrl(controlPlanePublicUrl?: string): string {
  const trimmed = controlPlanePublicUrl?.trim().replace(/\/+$/, '');
  if (trimmed) return `${trimmed}${SCIM_PATH}`;
  const { protocol, hostname, port } = window.location;
  const resolvedPort = port || (protocol === 'https:' ? '443' : '80');
  const backendPort = resolvedPort === '3000' ? '3001' : resolvedPort;
  return `${protocol}//${hostname}:${backendPort}${SCIM_PATH}`;
}

/* ── SCIM Tokens sub-section ─────────────────────────────────────────────── */

export function ScimTokenSection({ idp }: { idp: IdentityProvider }) {
  const { t } = useTranslation();
  // Read CP's reported publicURL so the SCIM endpoint shown to admins
  // matches what their IdP would actually reach. No per-component
  // de-dupe needed — useApi shares cache across mounts.
  const { data: serviceURLs } = useApi(
    () => serviceUrlsApi.publicURLs(),
    ['admin', 'services', 'public-urls'],
  );
  const [showCreate, setShowCreate] = useState(false);
  const [tokenName, setTokenName] = useState('');
  const [nameError, setNameError] = useState('');
  const [createdToken, setCreatedToken] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<ScimToken | null>(null);

  const { data, loading, error, refetch } = useApi<{ data: ScimToken[]; total: number }>(
    () => iamApi.listScimTokens(idp.id),
    ['admin', 'idp', 'scim-tokens', idp.id],
  );

  const { mutate: doCreate, loading: creating } = useMutation(
    () => iamApi.createScimToken(idp.id, tokenName.trim()),
    {
      onSuccess: (result) => {
        setShowCreate(false);
        setTokenName('');
        setCreatedToken(result.token);
        refetch();
      },
    },
  );

  const { mutate: doRevoke, loading: revoking } = useMutation(
    () => iamApi.revokeScimToken(idp.id, revokeTarget!.id),
    {
      onSuccess: () => {
        setRevokeTarget(null);
        refetch();
      },
    },
  );

  const handleCreate = useCallback(() => {
    if (!tokenName.trim()) {
      setNameError(t('pages:identityProvider.tokenNameRequired'));
      return;
    }
    setNameError('');
    void doCreate(undefined);
  }, [tokenName, doCreate, t]);

  const columns: DataTableColumn<ScimToken>[] = [
    { key: 'name', label: t('common:name') },
    { key: 'tokenPrefix', label: t('pages:identityProvider.tokenPrefix'), render: (r) => <code>{r.tokenPrefix}…</code> },
    { key: 'createdBy', label: t('pages:identityProvider.tokenCreatedBy') },
    { key: 'createdAt', label: t('common:createdAt'), render: (r) => formatDate(r.createdAt) },
    {
      key: 'lastUsedAt',
      label: t('pages:identityProvider.tokenLastUsed'),
      render: (r) => r.lastUsedAt ? formatDate(r.lastUsedAt) : <span style={{ color: 'var(--color-text-muted)' }}>{t('pages:identityProvider.tokenNeverUsed')}</span>,
    },
    {
      key: 'actions',
      label: '',
      render: (r) => (
        <Button variant="danger" size="sm" onClick={() => setRevokeTarget(r)}>
          {t('pages:identityProvider.revokeToken')}
        </Button>
      ),
    },
  ];

  const endpoint = scimBaseUrl(serviceURLs?.controlPlane);

  return (
    <div style={{ marginTop: 'var(--g-space-6)' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 'var(--g-space-3)' }}>
        <div>
          <h3 style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)' }}>
            {t('pages:identityProvider.scimSection')}
          </h3>
          <p style={{ margin: 'var(--g-space-1) var(--g-space-0) var(--g-space-0)', fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)' }}>
            {t('pages:identityProvider.scimEndpointHelp')}
          </p>
        </div>
        <Button size="sm" onClick={() => setShowCreate(true)}>
          {t('pages:identityProvider.generateToken')}
        </Button>
      </div>

      <Card>
        <div style={{ marginBottom: 'var(--g-space-4)' }}>
          <FormField label={t('pages:identityProvider.scimEndpoint')}>
            <div style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
              <code style={{
                flex: 1, padding: 'var(--g-space-2) var(--g-space-3)', background: 'var(--color-bg-subtle)',
                border: '1px solid var(--color-border)', borderRadius: 'var(--g-radius-sm)',
                fontSize: 'var(--g-font-size-sm)', userSelect: 'all',
              }}>
                {endpoint}
              </code>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => navigator.clipboard.writeText(endpoint)}
              >
                {t('common:copy')}
              </Button>
            </div>
          </FormField>
        </div>

        {loading && <Skeleton.Box width="100%" height={80} />}
        {error && <ErrorBanner message={error.message} onRetry={refetch} />}
        {!loading && !error && (
          <DataTable
            hideSearch
            frameless
            serverPaginated
            columns={columns}
            data={data?.data ?? []}
            emptyMessage={t('pages:identityProvider.noTokens')}
          />
        )}
      </Card>

      {/* Create token dialog */}
      <Dialog
        open={showCreate}
        onOpenChange={(o) => { if (!o) { setShowCreate(false); setTokenName(''); setNameError(''); } }}
        title={t('pages:identityProvider.generateToken')}
        size="sm"
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-4)' }}>
          <FormField
            label={t('pages:identityProvider.tokenName')}
            error={nameError}
          >
            <Input
              value={tokenName}
              onChange={(e) => { setTokenName(e.target.value); setNameError(''); }}
              placeholder={t('pages:identityProvider.tokenNamePlaceholder')}
              autoFocus
            />
          </FormField>
          <div style={{ display: 'flex', gap: 'var(--g-space-2)', justifyContent: 'flex-end' }}>
            <Button variant="secondary" onClick={() => { setShowCreate(false); setTokenName(''); setNameError(''); }}>
              {t('common:cancel')}
            </Button>
            <Button onClick={handleCreate} disabled={creating}>
              {t('pages:identityProvider.generateToken')}
            </Button>
          </div>
        </div>
      </Dialog>

      {/* Show token once dialog */}
      <SecretDialog
        open={createdToken !== null}
        secret={createdToken}
        title={t('pages:identityProvider.tokenCreatedTitle')}
        warning={t('pages:identityProvider.tokenCreatedWarning')}
        onClose={() => setCreatedToken(null)}
      />

      {/* Revoke confirmation */}
      <AlertDialog
        open={revokeTarget !== null}
        onOpenChange={(o) => { if (!o) setRevokeTarget(null); }}
        title={t('pages:identityProvider.revokeToken')}
        description={t('pages:identityProvider.confirmRevoke')}
        confirmLabel={t('pages:identityProvider.revokeToken')}
        variant="danger"
        onConfirm={() => void doRevoke(undefined)}
        loading={revoking}
      />
    </div>
  );
}

/* ── Group Mapping sub-section ───────────────────────────────────────────── */

export function GroupMappingSection({ idp }: { idp: IdentityProvider }) {
  const { t } = useTranslation();
  const [showAdd, setShowAdd] = useState(false);
  const [extGroupId, setExtGroupId] = useState('');
  const [extGroupName, setExtGroupName] = useState('');
  const [iamGroupId, setIamGroupId] = useState('');
  const [formError, setFormError] = useState('');
  const [deleteTarget, setDeleteTarget] = useState<IdpGroupMapping | null>(null);

  const { data, loading, error, refetch } = useApi<{ data: IdpGroupMapping[]; total: number }>(
    () => iamApi.listIdpGroupMappings(idp.id),
    ['admin', 'idp', 'group-mappings', idp.id],
  );

  const { data: groupsData } = useApi<{ data: IamGroup[]; total: number }>(
    () => iamApi.listGroups({ limit: '200', offset: '0' }),
    ['admin', 'iam', 'groups', 'idp-picker'],
  );
  const iamGroups = groupsData?.data ?? [];

  const { mutate: doAdd, loading: adding } = useMutation(
    () => iamApi.createIdpGroupMapping(idp.id, {
      externalGroupId: extGroupId.trim(),
      externalGroupName: extGroupName.trim() || undefined,
      iamGroupId,
    }),
    {
      onSuccess: () => {
        setShowAdd(false);
        setExtGroupId('');
        setExtGroupName('');
        setIamGroupId('');
        refetch();
      },
    },
  );

  const { mutate: doDelete, loading: deleting } = useMutation(
    () => iamApi.deleteIdpGroupMapping(idp.id, deleteTarget!.id),
    {
      onSuccess: () => {
        setDeleteTarget(null);
        refetch();
      },
    },
  );

  const handleAdd = useCallback(() => {
    if (!extGroupId.trim() || !iamGroupId) {
      setFormError(t('pages:identityProvider.mappingFieldsRequired'));
      return;
    }
    setFormError('');
    void doAdd(undefined);
  }, [extGroupId, iamGroupId, doAdd, t]);

  const columns: DataTableColumn<IdpGroupMapping>[] = [
    { key: 'externalGroupId', label: t('pages:identityProvider.externalGroupId') },
    {
      key: 'externalGroupName',
      label: t('pages:identityProvider.externalGroupName'),
      render: (r) => r.externalGroupName ?? <span style={{ color: 'var(--color-text-muted)' }}>—</span>,
    },
    {
      key: 'iamGroupId',
      label: t('pages:identityProvider.iamGroup'),
      render: (r) => r.iamGroupName ?? r.iamGroupId,
    },
    { key: 'createdAt', label: t('common:createdAt'), render: (r) => formatDate(r.createdAt) },
    {
      key: 'actions',
      label: '',
      render: (r) => (
        <Button variant="danger" size="sm" onClick={() => setDeleteTarget(r)}>
          {t('common:delete')}
        </Button>
      ),
    },
  ];

  return (
    <div style={{ marginTop: 'var(--g-space-6)' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 'var(--g-space-3)' }}>
        <div>
          <h3 style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)' }}>
            {t('pages:identityProvider.groupMappingsSection')}
          </h3>
          <p style={{ margin: 'var(--g-space-1) var(--g-space-0) var(--g-space-0)', fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)' }}>
            {t('pages:identityProvider.groupMappingsHelp')}
          </p>
        </div>
        <Button size="sm" onClick={() => setShowAdd(true)}>
          {t('pages:identityProvider.addMapping')}
        </Button>
      </div>

      <Card>
        {loading && <Skeleton.Box width="100%" height={80} />}
        {error && <ErrorBanner message={error.message} onRetry={refetch} />}
        {!loading && !error && (
          <DataTable
            hideSearch
            frameless
            serverPaginated
            columns={columns}
            data={data?.data ?? []}
            emptyMessage={t('pages:identityProvider.noMappings')}
          />
        )}
      </Card>

      {/* Add mapping dialog */}
      <Dialog
        open={showAdd}
        onOpenChange={(o) => { if (!o) { setShowAdd(false); setExtGroupId(''); setExtGroupName(''); setIamGroupId(''); setFormError(''); } }}
        title={t('pages:identityProvider.addMapping')}
        size="sm"
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-4)' }}>
          <FormField label={t('pages:identityProvider.externalGroupId')} error={formError}>
            <Input
              value={extGroupId}
              onChange={(e) => { setExtGroupId(e.target.value); setFormError(''); }}
              placeholder={t('pages:identityProvider.externalGroupIdPlaceholder')}
              autoFocus
            />
          </FormField>
          <FormField label={`${t('pages:identityProvider.externalGroupName')} (${t('common:optional')})`}>
            <Input
              value={extGroupName}
              onChange={(e) => setExtGroupName(e.target.value)}
              placeholder={t('pages:identityProvider.externalGroupNamePlaceholder')}
            />
          </FormField>
          <FormField label={t('pages:identityProvider.iamGroup')}>
            <select
              value={iamGroupId}
              onChange={(e) => setIamGroupId(e.target.value)}
              style={{ width: '100%', padding: 'var(--g-space-2) var(--g-space-3)', borderRadius: 'var(--g-radius-sm)', border: '1px solid var(--color-border)', background: 'var(--color-bg)', color: 'var(--color-text)', fontSize: 'var(--g-font-size-base)' }}
            >
              <option value="">{t('pages:identityProvider.selectIamGroup')}</option>
              {iamGroups.map((g) => (
                <option key={g.id} value={g.id}>{g.name}</option>
              ))}
            </select>
          </FormField>
          <div style={{ display: 'flex', gap: 'var(--g-space-2)', justifyContent: 'flex-end' }}>
            <Button variant="secondary" onClick={() => { setShowAdd(false); setExtGroupId(''); setExtGroupName(''); setIamGroupId(''); setFormError(''); }}>
              {t('common:cancel')}
            </Button>
            <Button onClick={handleAdd} disabled={adding}>
              {t('pages:identityProvider.addMapping')}
            </Button>
          </div>
        </div>
      </Dialog>

      {/* Delete confirmation */}
      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        title={t('common:deleteConfirmTitle')}
        description={t('pages:identityProvider.confirmDeleteMapping')}
        confirmLabel={t('common:delete')}
        variant="danger"
        onConfirm={() => void doDelete(undefined)}
        loading={deleting}
      />
    </div>
  );
}

/* ── IdP card ────────────────────────────────────────────────────────────── */

function IdpTypeLabel({ type }: { type: string }) {
  const { t } = useTranslation();
  if (type === 'oidc') return <Badge variant="info">OIDC</Badge>;
  if (type === 'saml') return <Badge variant="info">SAML</Badge>;
  return <Badge variant="default">{t('common:local')}</Badge>;
}

/**
 * One row per external IdP. Click anywhere on the card title to open
 * the detail page (which hosts the editable form + SCIM tokens +
 * group mappings). No inline modals — consistent with the rest of
 * the system (Providers, Routing Rules, Hooks all use dedicated
 * detail routes).
 */
function IdentityProviderCard({ idp, onDelete }: { idp: IdentityProvider; onDelete: (idp: IdentityProvider) => void }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const target = idpDetailRoute(idp.id);

  return (
    <Card
      style={{ cursor: 'pointer' }}
      onClick={() => navigate(target)}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)' }}>
          <h2 style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)' }}>{idp.name}</h2>
          <IdpTypeLabel type={idp.type} />
          <Badge variant={idp.enabled ? 'success' : 'default'}>
            {idp.enabled ? t('pages:identityProvider.enabled') : t('pages:identityProvider.disabled')}
          </Badge>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)' }}>
          <Button
            variant="secondary"
            size="sm"
            onClick={(e) => { e.stopPropagation(); navigate(target); }}
          >
            {t('common:view', 'View')}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={(e) => { e.stopPropagation(); onDelete(idp); }}
          >
            {t('common:delete', 'Delete')}
          </Button>
        </div>
      </div>
      <p style={{ margin: 'var(--g-space-2) var(--g-space-0) var(--g-space-0)', fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)' }}>
        {t('pages:identityProvider.idpId')}: <code style={{ fontSize: 'var(--g-font-size-sm)' }}>{idp.id}</code>
      </p>
    </Card>
  );
}

/* ── Main page ────────────────────────────────────────────────────────────── */

/**
 * "How it works" — a subtle, collapsible tip rendered as a single
 * row above the IdP list. Uses native <details> so the default
 * state is non-expanded once the user has skimmed it; opens with a
 * single click. Anchors the SP/IdP positioning so admins know what
 * the page is for without a Card competing for attention.
 */
function HowItWorksTip() {
  const { t } = useTranslation();
  return (
    <details
      style={{
        background: 'var(--color-bg-subtle)',
        border: '1px solid var(--color-border)',
        borderLeft: '3px solid var(--color-info))',
        borderRadius: 'var(--g-radius-sm)',
        padding: 'var(--g-space-2) var(--g-space-4)',
        fontSize: 'var(--g-font-size-sm)',
        color: 'var(--color-text-muted)',
      }}
    >
      <summary style={{ cursor: 'pointer', fontWeight: 'var(--g-font-weight-medium)', color: 'var(--color-text)' }}>
        {t('pages:identityProvider.howItWorks.tipSummary', 'How does this page work?')}
      </summary>
      <p style={{ margin: 'var(--g-space-2) var(--g-space-0) var(--g-space-0)' }}>
        {t('pages:identityProvider.howItWorks.intro')}
      </p>
      <ol style={{ margin: 'var(--g-space-2) var(--g-space-0) var(--g-space-0)', paddingLeft: 'var(--g-space-5)', lineHeight: 1.6 }}>
        <li><strong>{t('pages:identityProvider.howItWorks.step1Title')}</strong> — {t('pages:identityProvider.howItWorks.step1Body')}</li>
        <li><strong>{t('pages:identityProvider.howItWorks.step2Title')}</strong> — {t('pages:identityProvider.howItWorks.step2Body')}</li>
        <li><strong>{t('pages:identityProvider.howItWorks.step3Title')}</strong> — {t('pages:identityProvider.howItWorks.step3Body')}</li>
      </ol>
      <p style={{ margin: 'var(--g-space-2) var(--g-space-0) var(--g-space-0)' }}>{t('pages:identityProvider.howItWorks.localFallbackNote')}</p>
    </details>
  );
}

/**
 * Built-in Nexus Local card. Rendered as a peer row in the list — not
 * as a footnote — so admins can see at a glance that the platform has
 * a working authn fallback in place even when zero external IdPs are
 * configured. Per the SP/IdP positioning, this is NOT styled like an
 * external IdP entry; it carries a distinct "Built-in" badge and a
 * plain explanatory line.
 */
function LocalFallbackCard({ idp }: { idp: IdentityProvider }) {
  const { t } = useTranslation();
  return (
    <Card>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', marginBottom: 'var(--g-space-2)' }}>
        <h2 style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)' }}>{idp.name}</h2>
        <Badge variant="info">{t('pages:identityProvider.localFallbackBadge', 'Built-in')}</Badge>
        <Badge variant={idp.enabled ? 'success' : 'default'}>
          {idp.enabled ? t('pages:identityProvider.enabled') : t('pages:identityProvider.disabled')}
        </Badge>
      </div>
      <p style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)' }}>
        {t('pages:identityProvider.localIdpNote')}
      </p>
    </Card>
  );
}

/**
 * Empty state — title + helper body only. The "Add Identity Provider"
 * CTA is in the PageHeader action slot (top-right), so we don't repeat
 * it here. Body text is width-constrained so it wraps at a natural
 * line-break instead of mid-clause.
 */
function EmptyState() {
  const { t } = useTranslation();
  return (
    <Card style={{ textAlign: 'center', padding: 'var(--g-space-10) var(--g-space-6)' }}>
      <h3 style={{ margin: 'var(--g-space-0) var(--g-space-0) var(--g-space-2)', fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)' }}>
        {t('pages:identityProvider.emptyState.title')}
      </h3>
      <p style={{
        margin: '0 auto',
        maxWidth: '52ch',
        fontSize: 'var(--g-font-size-base)',
        color: 'var(--color-text-muted)',
        lineHeight: 1.55,
      }}>
        {t('pages:identityProvider.emptyState.body')}
      </p>
    </Card>
  );
}

export function IdentityProviderPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [deleteTarget, setDeleteTarget] = useState<IdentityProvider | null>(null);

  const { data, loading, error, refetch } = useApi<{ data: IdentityProvider[]; total: number }>(
    () => iamApi.listIdentityProviders(),
    ['admin', 'identity-providers', 'list'],
  );

  const { mutate: doDelete, loading: deleting } = useMutation(
    () => iamApi.deleteIdentityProvider(deleteTarget!.id),
    {
      onSuccess: () => {
        setDeleteTarget(null);
        refetch();
      },
    },
  );

  const allIdps = data?.data ?? [];
  const externalIdps = allIdps.filter((i) => i.type !== 'local');
  const localIdp = allIdps.find((i) => i.type === 'local');

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-5)' }}>
      <PageHeader
        title={t('pages:identityProvider.title')}
        subtitle={t('pages:identityProvider.subtitle')}
        subtitleClassName="max-w-none whitespace-nowrap"
        action={
          <Button onClick={() => navigate(IDP_NEW_ROUTE)}>
            {t('pages:identityProvider.addIdp', 'Add Identity Provider')}
          </Button>
        }
      />

      <HowItWorksTip />

      {loading && <Skeleton.ListPageSkeleton />}
      {error && <ErrorBanner message={error.message} onRetry={refetch} />}

      {!loading && !error && (
        <>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'calc(var(--g-space-1) + var(--g-space-0-5))' }}>
            {localIdp && <LocalFallbackCard idp={localIdp} />}
            {externalIdps.map((idp) => (
              <IdentityProviderCard key={idp.id} idp={idp} onDelete={setDeleteTarget} />
            ))}
          </div>
          {externalIdps.length === 0 && <EmptyState />}
        </>
      )}

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        title={t('pages:identityProvider.confirmDeleteIdpTitle', 'Delete Identity Provider')}
        description={t('pages:identityProvider.confirmDeleteIdpBody', 'Users linked to this IdP will lose access on their next request. SCIM tokens scoped to this IdP will be revoked. This action cannot be undone.')}
        confirmLabel={t('common:delete', 'Delete')}
        variant="danger"
        onConfirm={() => void doDelete(undefined)}
        loading={deleting}
      />
    </div>
  );
}
