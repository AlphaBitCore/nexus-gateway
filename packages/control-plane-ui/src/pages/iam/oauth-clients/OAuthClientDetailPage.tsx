import { useState, useCallback } from 'react';
import { Link, useParams, useNavigate, Navigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { oauthClientApi, type OAuthClient, type OAuthClientRotateResponse } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { useToast } from '@/context/ToastContext';
import { formatRelativeTime } from '@/lib/format';
import {
  Stack, Card, Button, Skeleton, ErrorBanner,
  AlertDialog, SecretDialog, Tooltip,
} from '@/components/ui';
import { ScopeChip } from './components/ScopeChip';
import { DeleteClientConfirmDialog } from './components/DeleteClientConfirmDialog';
import detailStyles from '../_shared/Iam.module.css';
import styles from './OAuthClientDetailPage.module.css';

/**
 * Render a TTL second-count as a human-friendly localized string. The handler
 * accepts 60..86400 seconds for access TTLs and 3600..2592000 for refresh TTLs,
 * so the units we surface are minutes / hours / days. Uses i18next plural
 * forms so the EN "1 day / N days" / ES "1 día / N días" / ZH "1 天 / N 天"
 * variants stay together with the rest of the page copy.
 */
function useFormatSeconds(): (s: number) => string {
  const { t } = useTranslation();
  return (s: number) => {
    if (s % 86400 === 0) return t('pages:iam.oauthClients.duration.day', { count: s / 86400 });
    if (s % 3600 === 0) return t('pages:iam.oauthClients.duration.hour', { count: s / 3600 });
    if (s % 60 === 0) return t('pages:iam.oauthClients.duration.minute', { count: s / 60 });
    return t('pages:iam.oauthClients.duration.second', { count: s });
  };
}

function CopyButton({ value, ariaLabel }: { value: string; ariaLabel: string }) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const onClick = useCallback(async () => {
    await navigator.clipboard.writeText(value);
    addToast(t('common:copied'), 'success');
  }, [value, addToast, t]);
  return (
    <button
      type="button"
      onClick={onClick}
      className={styles.copyButton}
      aria-label={ariaLabel}
      data-design-system-escape="primitive-internal"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
        <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M10.5 5.5V3.5C10.5 2.67 9.83 2 9 2H3.5C2.67 2 2 2.67 2 3.5V9C2 9.83 2.67 10.5 3.5 10.5H5.5" stroke="currentColor" strokeWidth="1.5" />
      </svg>
    </button>
  );
}

export function OAuthClientDetailPage() {
  const { t } = useTranslation();
  const formatSeconds = useFormatSeconds();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const canEdit = usePermission('oauth-client:update');
  const canRotate = usePermission('oauth-client:rotate');
  const canDelete = usePermission('oauth-client:delete');

  const { data, loading, error, refetch } = useApi<{ data: OAuthClient }>(
    () => oauthClientApi.getOne(id!),
    ['admin', 'oauth-clients', 'detail', id ?? ''],
    { skip: !id },
  );

  const [showRotateConfirm, setShowRotateConfirm] = useState(false);
  const [revealedSecret, setRevealedSecret] = useState<string | null>(null);
  const [showDelete, setShowDelete] = useState(false);

  const { mutate: rotateSecret, loading: rotating } = useMutation(
    (cid: string) => oauthClientApi.rotateSecret(cid),
    {
      invalidateQueries: [['admin', 'oauth-clients']],
      onSuccess: (result) => {
        const secret = (result as { data?: OAuthClientRotateResponse }).data?.clientSecret;
        if (secret) setRevealedSecret(secret);
        setShowRotateConfirm(false);
      },
      successMessage: t('pages:iam.oauthClients.toastRotated'),
    },
  );

  const { mutate: deleteClient, loading: deleting } = useMutation(
    (cid: string) => oauthClientApi.remove(cid),
    {
      invalidateQueries: [['admin', 'oauth-clients']],
      onSuccess: () => {
        setShowDelete(false);
        navigate('/iam/oauth-clients');
      },
      successMessage: t('pages:iam.oauthClients.toastDeleted'),
    },
  );

  if (!id) return <Navigate to="/iam/oauth-clients" replace />;
  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const client = data?.data;
  if (!client) return <ErrorBanner message={t('pages:iam.oauthClients.notFound')} />;

  const isPublic = client.type === 'public';
  const activeRefreshTokenCount = client.activeRefreshTokenCount ?? 0;

  return (
    <Stack gap="lg">
      <section className={detailStyles.detailHeader}>
        <div className={detailStyles.detailHeaderRow}>
          <Link to="/iam/oauth-clients" className={detailStyles.detailBackLink} aria-label={t('common:back')}>
            <svg className={detailStyles.detailBackIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={detailStyles.detailHeaderText}>
            <h1 className={detailStyles.detailTitle}>{client.name}</h1>
            <div className={detailStyles.detailMeta}>
              <span className={styles.idMono}>{client.id}</span>
              <CopyButton value={client.id} ariaLabel={t('common:copy')} />
              <span className={isPublic ? styles.typeBadgePublic : styles.typeBadgeConfidential}>
                {isPublic
                  ? t('pages:iam.oauthClients.typePublic')
                  : t('pages:iam.oauthClients.typeConfidential')}
              </span>
              <span className={styles.createdAt}>
                {new Date(client.createdAt).toLocaleDateString()}
              </span>
            </div>
          </div>
          <Stack direction="horizontal" gap="sm" className={detailStyles.detailHeaderActions}>
            {canEdit && (
              <Button variant="secondary" onClick={() => navigate(`/iam/oauth-clients/${client.id}/edit`)}>
                {t('common:edit')}
              </Button>
            )}
            {canRotate && !isPublic && (
              <Button variant="secondary" onClick={() => setShowRotateConfirm(true)}>
                {t('pages:iam.oauthClients.rotateSecretButton')}
              </Button>
            )}
            {canDelete && (
              <Button variant="danger" onClick={() => setShowDelete(true)}>
                {t('common:delete')}
              </Button>
            )}
          </Stack>
        </div>
      </section>

      {/* Card 1 — Authentication */}
      <section className={styles.contentSection}>
        <h2 className={styles.cardTitle}>{t('pages:iam.oauthClients.cardAuthentication')}</h2>
        <Card>
          <div className={styles.fieldGrid}>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.clientIdLabel')}</span>
              <span className={styles.fieldValueInline}>
                <span className={styles.fieldValueMono}>{client.id}</span>
                <CopyButton value={client.id} ariaLabel={t('common:copy')} />
              </span>
            </div>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.clientSecretLabel')}</span>
              {isPublic ? (
                <Tooltip content={t('pages:iam.oauthClients.publicClientNoSecretTooltip')}>
                  <span className={styles.publicNoSecret}>
                    {t('pages:iam.oauthClients.publicClientNoSecret')}
                  </span>
                </Tooltip>
              ) : (
                <span className={styles.fieldValueInline}>
                  <span className={styles.secretMask} aria-label="masked secret">
                    {t('pages:iam.oauthClients.secretMasked')}
                  </span>
                  <span className={styles.lastRotated}>
                    {client.lastSecretRotatedAt
                      ? t('pages:iam.oauthClients.lastRotated', { relative: formatRelativeTime(client.lastSecretRotatedAt) })
                      : t('pages:iam.oauthClients.neverRotated')}
                  </span>
                </span>
              )}
            </div>
          </div>
        </Card>
      </section>

      {/* Card 2 — Redirect URIs */}
      <section className={styles.contentSection}>
        <h2 className={styles.cardTitle}>{t('pages:iam.oauthClients.cardRedirectUris')}</h2>
        <Card>
          <ul className={styles.uriList}>
            {client.redirectUris.map((uri) => (
              <li key={uri} className={styles.uriRow}>
                <code className={styles.uriValue}>{uri}</code>
                <CopyButton value={uri} ariaLabel={t('common:copy')} />
              </li>
            ))}
          </ul>
        </Card>
      </section>

      {/* Card 3 — Allowed scopes */}
      <section className={styles.contentSection}>
        <h2 className={styles.cardTitle}>{t('pages:iam.oauthClients.cardAllowedScopes')}</h2>
        <Card>
          <div className={styles.scopeGrid}>
            {client.allowedScopes.map((scope) => (
              <ScopeChip key={scope} scope={scope} />
            ))}
          </div>
        </Card>
      </section>

      {/* Card 4 — Security */}
      <section className={styles.contentSection}>
        <h2 className={styles.cardTitle}>{t('pages:iam.oauthClients.cardSecurity')}</h2>
        <Card>
          <div className={styles.fieldGrid}>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.requirePkceLabel')}</span>
              <span className={styles.fieldValue}>
                {client.requirePkce ? t('common:yes') : t('common:no')}
                {isPublic && (
                  <span className={styles.fieldHint}>
                    {' '}{t('pages:iam.oauthClients.requirePkceForcedByType')}
                  </span>
                )}
              </span>
            </div>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.accessTtlLabel')}</span>
              <span className={styles.fieldValue}>
                {formatSeconds(client.accessTtlSeconds)}
                <span className={styles.fieldHint}>{' '}({client.accessTtlSeconds}s)</span>
              </span>
            </div>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.refreshTtlLabel')}</span>
              <span className={styles.fieldValue}>
                {formatSeconds(client.refreshTtlSeconds)}
                <span className={styles.fieldHint}>{' '}({client.refreshTtlSeconds}s)</span>
              </span>
            </div>
          </div>
        </Card>
      </section>

      {/* Card 5 — Activity */}
      <section className={styles.contentSection}>
        <h2 className={styles.cardTitle}>{t('pages:iam.oauthClients.cardActivity')}</h2>
        <Card>
          <div className={styles.fieldGrid}>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.activeRefreshTokensLabel')}</span>
              <Tooltip content={t('pages:iam.oauthClients.activeRefreshTokensTooltip')}>
                <span className={styles.fieldValue}>{activeRefreshTokenCount}</span>
              </Tooltip>
            </div>
            <div className={styles.fieldItem}>
              <span className={styles.fieldLabel}>{t('pages:iam.oauthClients.lastUpdatedLabel')}</span>
              <span className={styles.fieldValue}>{formatRelativeTime(client.updatedAt)}</span>
            </div>
          </div>
        </Card>
      </section>

      {/* Rotate confirm — plain AlertDialog (interpolated body, no custom input). */}
      <AlertDialog
        open={showRotateConfirm}
        onOpenChange={(o) => { if (!o) setShowRotateConfirm(false); }}
        title={t('pages:iam.oauthClients.rotateConfirmTitle')}
        description={t('pages:iam.oauthClients.rotateConfirmBody', { count: activeRefreshTokenCount })}
        confirmLabel={t('pages:iam.oauthClients.rotateConfirmConfirm')}
        cancelLabel={t('pages:iam.oauthClients.rotateConfirmCancel')}
        variant="danger"
        loading={rotating}
        onConfirm={() => rotateSecret(client.id)}
      />

      {/* Secret reveal — hard-gated by the ack checkbox. */}
      <SecretDialog
        open={revealedSecret !== null}
        secret={revealedSecret}
        title={t('pages:iam.oauthClients.secretRevealTitle')}
        warning={t('pages:iam.oauthClients.secretRevealWarning')}
        requireAcknowledgement
        acknowledgementLabel={t('pages:iam.oauthClients.secretRevealAckCheckbox')}
        onClose={() => setRevealedSecret(null)}
      />

      {/* Delete — type-to-confirm. */}
      <DeleteClientConfirmDialog
        open={showDelete}
        clientId={client.id}
        activeRefreshTokenCount={activeRefreshTokenCount}
        loading={deleting}
        onCancel={() => setShowDelete(false)}
        onConfirm={() => deleteClient(client.id)}
      />
    </Stack>
  );
}
