import { useState, useEffect, useMemo, useCallback, useRef } from 'react';
import { Link, useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  oauthClientApi,
  type OAuthClient,
  type OAuthClientCreateResponse,
  type CreateOAuthClientInput,
  type UpdateOAuthClientInput,
} from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import {
  PageHeader, Stack, Card, Button, Input, FormField, Select, Switch,
  Skeleton, ErrorBanner, SecretDialog,
} from '@/components/ui';
import { ChipInput } from '../_shared/ChipInput';
import detailStyles from '../_shared/Iam.module.css';
import styles from './OAuthClientFormPage.module.css';

const ID_REGEX = /^[a-z][a-z0-9-]{2,63}$/;
const SCOPE_REGEX = /^[a-z][a-z0-9:_-]*$/;
const KNOWN_SCOPES = ['openid', 'profile', 'email', 'offline_access', 'admin', 'traffic:write'];
// Matches a ":*" RFC 8252 §7.3 port wildcard in the URL port position (right
// after the host, before "/" or end). Mirrors portWildcardRe in
// authserver/store/client_store.go.
const PORT_WILDCARD_RE = /^(https?:\/\/(?:\[[^\]]+\]|[^/:[]+)):\*(\/|$)/;
const ACCESS_TTL_MIN = 60;
const ACCESS_TTL_MAX = 86400;
const REFRESH_TTL_MIN = 3600;
const REFRESH_TTL_MAX = 2592000;

interface FormState {
  id: string;
  name: string;
  type: 'public' | 'confidential';
  redirectUris: string[];
  allowedScopes: string; // newline-delimited for ChipInput
  requirePkce: boolean;
  accessTtlSeconds: number;
  refreshTtlSeconds: number;
}

const DEFAULT_STATE: FormState = {
  id: '',
  name: '',
  type: 'confidential',
  redirectUris: [''],
  allowedScopes: 'openid\nprofile\nemail',
  requirePkce: true,
  accessTtlSeconds: 3600,
  refreshTtlSeconds: 86400,
};

function validRedirectUri(uri: string): boolean {
  if (!uri) return false;
  // Mirror authserver/store ValidRedirectURIPattern (the registration rule the
  // backend enforces, itself the counterpart to the authorize-time matchLoopback).
  // A naive startsWith prefix match accepts `http://localhost.evil.com/cb`,
  // which the handler then rejects with a vague 400. Accepts the RFC 8252 §7.3
  // ":*" loopback port wildcard for all three loopback hosts (localhost /
  // 127.0.0.1 / [::1]), e.g. the CLI client's http://127.0.0.1:*/callback —
  // new URL() chokes on ":*", so substitute :0 first.
  let hasPortWildcard = false;
  let parseTarget = uri;
  if (PORT_WILDCARD_RE.test(uri)) {
    hasPortWildcard = true;
    parseTarget = uri.replace(PORT_WILDCARD_RE, (_full, prefix, tail) => `${prefix}:0${tail}`);
  }
  let u: URL;
  try {
    u = new URL(parseTarget);
  } catch {
    return false;
  }
  if (u.protocol === 'https:') return !hasPortWildcard;
  if (u.protocol === 'http:') {
    const host = u.hostname.replace(/^\[|\]$/g, ''); // URL keeps IPv6 brackets; Go's Hostname() strips them
    return host === 'localhost' || host === '127.0.0.1' || host === '::1';
  }
  return false;
}

export function OAuthClientFormPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { id: editId } = useParams<{ id: string }>();
  const isEdit = Boolean(editId);

  const { data, loading, error, refetch } = useApi<{ data: OAuthClient }>(
    () => oauthClientApi.getOne(editId!),
    ['admin', 'oauth-clients', 'detail', editId ?? ''],
    { skip: !isEdit },
  );

  const [form, setForm] = useState<FormState>(DEFAULT_STATE);
  // Track which editId has been hydrated. A simple `hydrated` boolean would
  // never reset when navigating /clientA/edit → /clientB/edit within the same
  // mounted Route, leaving clientA's form values bound to a clientB PATCH —
  // the admin would silently overwrite clientB with clientA's fields.
  const hydratedFor = useRef<string | null>(null);
  const [revealedSecret, setRevealedSecret] = useState<string | null>(null);
  const [createdId, setCreatedId] = useState<string | null>(null);

  useEffect(() => {
    if (!isEdit || !data?.data) return;
    if (hydratedFor.current === editId) return;
    const c = data.data;
    setForm({
      id: c.id,
      name: c.name,
      type: c.type,
      redirectUris: c.redirectUris.length > 0 ? [...c.redirectUris] : [''],
      allowedScopes: c.allowedScopes.join('\n'),
      requirePkce: c.requirePkce,
      accessTtlSeconds: c.accessTtlSeconds,
      refreshTtlSeconds: c.refreshTtlSeconds,
    });
    hydratedFor.current = editId ?? null;
  }, [isEdit, editId, data]);

  const updateField = useCallback(<K extends keyof FormState>(key: K, value: FormState[K]) => {
    setForm((prev) => {
      const next = { ...prev, [key]: value };
      // Public clients are required to use PKCE; the handler enforces this
      // server-side, so we mirror the override for visual honesty.
      if (key === 'type' && value === 'public') {
        next.requirePkce = true;
      }
      return next;
    });
  }, []);

  const scopesArray = useMemo(
    () => form.allowedScopes.split('\n').map((s) => s.trim()).filter(Boolean),
    [form.allowedScopes],
  );

  const fieldErrors = useMemo(() => {
    const errs: Partial<Record<keyof FormState | 'submit', string>> = {};
    if (!isEdit && !ID_REGEX.test(form.id)) {
      errs.id = t('pages:iam.oauthClients.validationIdFormat');
    }
    if (!form.name || form.name.length > 100) {
      errs.name = t('pages:iam.oauthClients.validationNameRequired');
    }
    const uris = form.redirectUris.map((u) => u.trim()).filter(Boolean);
    if (uris.length < 1 || uris.length > 20) {
      errs.redirectUris = t('pages:iam.oauthClients.validationRedirectUriCount');
    } else if (!uris.every(validRedirectUri)) {
      errs.redirectUris = t('pages:iam.oauthClients.validationRedirectUriScheme');
    }
    if (scopesArray.length < 1 || scopesArray.length > 20) {
      errs.allowedScopes = t('pages:iam.oauthClients.validationScopeCount');
    } else if (!scopesArray.every((s) => SCOPE_REGEX.test(s))) {
      errs.allowedScopes = t('pages:iam.oauthClients.validationScopeFormat');
    }
    if (!Number.isFinite(form.accessTtlSeconds) || form.accessTtlSeconds < ACCESS_TTL_MIN || form.accessTtlSeconds > ACCESS_TTL_MAX) {
      errs.accessTtlSeconds = t('pages:iam.oauthClients.validationAccessTtl');
    }
    if (!Number.isFinite(form.refreshTtlSeconds) || form.refreshTtlSeconds < REFRESH_TTL_MIN || form.refreshTtlSeconds > REFRESH_TTL_MAX) {
      errs.refreshTtlSeconds = t('pages:iam.oauthClients.validationRefreshTtl');
    }
    return errs;
  }, [form, scopesArray, isEdit, t]);

  const canSubmit = Object.keys(fieldErrors).length === 0;

  const { mutate: createMut, loading: creating } = useMutation(
    (payload: CreateOAuthClientInput) => oauthClientApi.create(payload),
    {
      invalidateQueries: [['admin', 'oauth-clients']],
      onSuccess: (result) => {
        const resp = (result as { data?: OAuthClientCreateResponse }).data;
        if (!resp) return;
        setCreatedId(resp.id);
        if (resp.clientSecret) {
          setRevealedSecret(resp.clientSecret);
        } else {
          navigate(`/iam/oauth-clients/${resp.id}`);
        }
      },
      successMessage: t('pages:iam.oauthClients.toastCreated'),
      errorMessage: t('pages:iam.oauthClients.toastSaveError'),
    },
  );

  const { mutate: updateMut, loading: updating } = useMutation(
    (payload: UpdateOAuthClientInput) => oauthClientApi.update(editId!, payload),
    {
      invalidateQueries: [['admin', 'oauth-clients']],
      onSuccess: () => {
        navigate(`/iam/oauth-clients/${editId}`);
      },
      successMessage: t('pages:iam.oauthClients.toastUpdated'),
      errorMessage: t('pages:iam.oauthClients.toastSaveError'),
    },
  );

  const onSubmit = useCallback(() => {
    const uris = form.redirectUris.map((u) => u.trim()).filter(Boolean);
    if (isEdit) {
      updateMut({
        name: form.name,
        redirectUris: uris,
        allowedScopes: scopesArray,
        requirePkce: form.requirePkce,
        accessTtlSeconds: form.accessTtlSeconds,
        refreshTtlSeconds: form.refreshTtlSeconds,
      });
    } else {
      createMut({
        id: form.id,
        name: form.name,
        type: form.type,
        redirectUris: uris,
        allowedScopes: scopesArray,
        requirePkce: form.requirePkce,
        accessTtlSeconds: form.accessTtlSeconds,
        refreshTtlSeconds: form.refreshTtlSeconds,
      });
    }
  }, [form, scopesArray, isEdit, createMut, updateMut]);

  const onCancel = useCallback(() => {
    navigate(isEdit ? `/iam/oauth-clients/${editId}` : '/iam/oauth-clients');
  }, [navigate, isEdit, editId]);

  const onSecretClose = useCallback(() => {
    setRevealedSecret(null);
    if (createdId) navigate(`/iam/oauth-clients/${createdId}`);
  }, [createdId, navigate]);

  if (isEdit && loading) return <Skeleton.DetailPageSkeleton />;
  if (isEdit && error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const submitting = creating || updating;
  const title = isEdit ? data?.data?.name ?? '' : t('pages:iam.oauthClients.createButton');

  return (
    <Stack gap="lg" className={styles.page}>
      {isEdit ? (
        <section className={detailStyles.detailHeader}>
          <div className={detailStyles.detailHeaderRow}>
            <Link to={`/iam/oauth-clients/${editId}`} className={detailStyles.detailBackLink} aria-label={t('common:back')}>
              <svg className={detailStyles.detailBackIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
                <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
                <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            </Link>
            <div className={detailStyles.detailHeaderText}>
              <h1 className={detailStyles.detailTitle}>{title}</h1>
              <div className={detailStyles.detailMeta}>
                <span>{t('common:edit')}</span>
              </div>
            </div>
          </div>
        </section>
      ) : (
        <PageHeader title={title} />
      )}

      <Card className={styles.formCard}>
        <form onSubmit={(e) => { e.preventDefault(); onSubmit(); }}>
          <div className={styles.formGrid}>
            <FormField
              label={t('pages:iam.oauthClients.formIdLabel')}
              required={!isEdit}
              helpText={t('pages:iam.oauthClients.formIdHint')}
              error={fieldErrors.id}
            >
              <Input
                value={form.id}
                onChange={(e) => updateField('id', e.target.value)}
                disabled={isEdit}
                placeholder="my-app"
                spellCheck={false}
                autoCorrect="off"
                autoCapitalize="off"
              />
            </FormField>

            <FormField label={t('pages:iam.oauthClients.formNameLabel')} required error={fieldErrors.name}>
              <Input
                value={form.name}
                onChange={(e) => updateField('name', e.target.value)}
              />
            </FormField>

            <FormField
              label={t('pages:iam.oauthClients.formTypeLabel')}
              helpText={isEdit ? t('pages:iam.oauthClients.formTypeImmutable') : undefined}
            >
              <Select
                value={form.type}
                onValueChange={(v) => updateField('type', v as 'public' | 'confidential')}
                disabled={isEdit}
                options={[
                  { value: 'confidential', label: t('pages:iam.oauthClients.typeConfidential') },
                  { value: 'public', label: t('pages:iam.oauthClients.typePublic') },
                ]}
              />
            </FormField>

            <div className={`${styles.redirectField} ${styles.fullWidthField}`}>
              <div className={styles.redirectTitleRow}>
                <div>
                  <span className={styles.fieldLabel}>
                    {t('pages:iam.oauthClients.formRedirectUrisLabel')} <span aria-hidden="true">*</span>
                  </span>
                  <p className={styles.fieldHint}>{t('pages:iam.oauthClients.formRedirectUrisHint')}</p>
                  {fieldErrors.redirectUris && <p className={styles.fieldError}>{fieldErrors.redirectUris}</p>}
                </div>
                <button
                  type="button"
                  className={styles.addRedirectButton}
                  onClick={() => updateField('redirectUris', [...form.redirectUris, ''])}
                  disabled={form.redirectUris.length >= 20}
                >
                  + {t('pages:iam.oauthClients.formAddRedirectUri')}
                </button>
              </div>
              <Stack gap="sm">
                {form.redirectUris.map((uri, i) => (
                  <Stack key={i} direction="horizontal" gap="sm" className={styles.redirectRow}>
                    <Input
                      value={uri}
                      onChange={(e) => {
                        const next = [...form.redirectUris];
                        next[i] = e.target.value;
                        updateField('redirectUris', next);
                      }}
                      placeholder="https://app.example.com/callback"
                      spellCheck={false}
                      autoCorrect="off"
                      autoCapitalize="off"
                    />
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      onClick={() => {
                        const next = form.redirectUris.filter((_, idx) => idx !== i);
                        updateField('redirectUris', next.length > 0 ? next : ['']);
                      }}
                      disabled={form.redirectUris.length <= 1}
                      aria-label={`Remove URI ${i + 1}`}
                    >
                      ×
                    </Button>
                  </Stack>
                ))}
              </Stack>
            </div>

            <FormField
              className={styles.fullWidthField}
              label={t('pages:iam.oauthClients.formAllowedScopesLabel')}
              required
              helpText={t('pages:iam.oauthClients.formAllowedScopesHint')}
              error={fieldErrors.allowedScopes}
            >
              <div className={styles.scopesInput}>
                <ChipInput
                  value={form.allowedScopes}
                  onChange={(v) => updateField('allowedScopes', v)}
                  suggestions={KNOWN_SCOPES}
                  validate={(chip) => SCOPE_REGEX.test(chip)}
                  placeholder="openid"
                  ariaLabel={t('pages:iam.oauthClients.formAllowedScopesLabel')}
                />
              </div>
            </FormField>

            <FormField label={t('pages:iam.oauthClients.formRequirePkceLabel')}>
              <div className={styles.switchRow}>
                <Switch
                  checked={form.requirePkce}
                  onCheckedChange={(c) => updateField('requirePkce', c === true)}
                  disabled={form.type === 'public'}
                />
                <span>
                  {form.requirePkce
                    ? t('common:yes')
                    : t('common:no')}
                  {form.type === 'public' && (
                    <span className={styles.fieldHint}>
                      {' '}{t('pages:iam.oauthClients.requirePkceForcedByType')}
                    </span>
                  )}
                </span>
              </div>
            </FormField>

            <FormField
              label={t('pages:iam.oauthClients.formAccessTtlLabel')}
              error={fieldErrors.accessTtlSeconds}
            >
              <Input
                type="number"
                value={Number.isFinite(form.accessTtlSeconds) ? form.accessTtlSeconds : ''}
                onChange={(e) => {
                  const n = Number(e.target.value);
                  updateField('accessTtlSeconds', Number.isFinite(n) ? n : NaN);
                }}
                min={ACCESS_TTL_MIN}
                max={ACCESS_TTL_MAX}
              />
            </FormField>

            <FormField
              label={t('pages:iam.oauthClients.formRefreshTtlLabel')}
              error={fieldErrors.refreshTtlSeconds}
            >
              <Input
                type="number"
                value={Number.isFinite(form.refreshTtlSeconds) ? form.refreshTtlSeconds : ''}
                onChange={(e) => {
                  const n = Number(e.target.value);
                  updateField('refreshTtlSeconds', Number.isFinite(n) ? n : NaN);
                }}
                min={REFRESH_TTL_MIN}
                max={REFRESH_TTL_MAX}
              />
            </FormField>
          </div>

          <Stack direction="horizontal" gap="sm" className={styles.formActions}>
              <Button type="submit" disabled={!canSubmit || submitting}>
                {submitting
                  ? '...'
                  : isEdit
                    ? t('pages:iam.oauthClients.formSubmitSave')
                    : t('pages:iam.oauthClients.formSubmitCreate')}
              </Button>
              <Button type="button" variant="secondary" onClick={onCancel}>
                {t('pages:iam.oauthClients.formCancel')}
              </Button>
          </Stack>
        </form>
      </Card>

      <SecretDialog
        open={revealedSecret !== null}
        secret={revealedSecret}
        title={t('pages:iam.oauthClients.secretRevealTitle')}
        warning={t('pages:iam.oauthClients.secretRevealWarning')}
        requireAcknowledgement
        acknowledgementLabel={t('pages:iam.oauthClients.secretRevealAckCheckbox')}
        onClose={onSecretClose}
      />
    </Stack>
  );
}
