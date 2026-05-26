/**
 * IdentityProviderForm — presentational form for Add / Edit IdP.
 *
 * The platform is the SP; this form collects the protocol-
 * specific config the admin obtains from their IdP's app-registration
 * page (Okta / Azure AD / Google Workspace / JumpCloud / OneLogin).
 *
 * Rendered as inline content on a dedicated route (not a Dialog) so it
 * matches the rest of the system (Add Provider, Add Routing Rule, etc.).
 *
 * Props:
 *   - mode: 'create' | 'edit'
 *   - initial: optional pre-populated IdP (edit mode)
 *   - onSubmit: receives the IdentityProviderWriteRequest payload
 *   - onCancel: navigate back to the list
 *   - submitting: boolean to disable Save while in-flight
 *   - submitError: optional error string to render
 *
 * Test connection probe uses the same payload and renders inline below
 * the buttons.
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation } from '@/hooks/useMutation';
import { iamApi } from '@/api/services';
import type {
  IdentityProvider,
  IdentityProviderProbeResult,
  IdentityProviderWriteRequest,
} from '@/api/types';
import {
  Button,
  Input,
  Textarea,
  FormField,
  ErrorBanner,
  Badge,
  Card,
  Stack,
} from '@/components/ui';

export type Protocol = 'oidc' | 'saml';

interface OIDCDraft {
  name: string;
  enabled: boolean;
  issuer: string;
  clientId: string;
  clientSecret: string;
  redirectUri: string;
  jwksUri: string;
  authorizeUrl: string;
  tokenUrl: string;
  scopes: string;
  emailClaim: string;
  groupClaim: string;
}

interface SAMLDraft {
  name: string;
  enabled: boolean;
  entityId: string;
  ssoUrl: string;
  certificatePem: string;
}

function defaultRedirectUri(): string {
  if (typeof window !== 'undefined') {
    return `${window.location.origin}/authserver/oidc/callback`;
  }
  return 'https://YOUR_NEXUS_HOST/authserver/oidc/callback';
}

function emptyOIDC(): OIDCDraft {
  return {
    name: '',
    enabled: true,
    issuer: '',
    clientId: '',
    clientSecret: '',
    redirectUri: defaultRedirectUri(),
    jwksUri: '',
    authorizeUrl: '',
    tokenUrl: '',
    scopes: 'openid profile email',
    emailClaim: 'email',
    groupClaim: 'groups',
  };
}

function emptySAML(): SAMLDraft {
  return {
    name: '',
    enabled: true,
    entityId: '',
    ssoUrl: '',
    certificatePem: '',
  };
}

/**
 * Build form drafts from an existing IdP record. On edit, secrets come
 * back as the sentinel "********" from the API — keep that placeholder
 * so the user knows it's preserved unless they overwrite.
 */
function oidcDraftFromIdp(idp: IdentityProvider): OIDCDraft {
  const c = (idp.config ?? {}) as Record<string, unknown>;
  return {
    name: idp.name,
    enabled: idp.enabled,
    issuer: String(c.issuer ?? ''),
    clientId: String(c.clientId ?? ''),
    clientSecret: String(c.clientSecret ?? '********'),
    redirectUri: String(c.redirectUri ?? defaultRedirectUri()),
    jwksUri: String(c.jwksUri ?? ''),
    authorizeUrl: String(c.authorizeUrl ?? ''),
    tokenUrl: String(c.tokenUrl ?? ''),
    scopes: Array.isArray(c.scopes) ? (c.scopes as string[]).join(' ') : String(c.scopes ?? 'openid profile email'),
    emailClaim: String(c.emailClaim ?? 'email'),
    groupClaim: String(c.groupClaim ?? 'groups'),
  };
}

function samlDraftFromIdp(idp: IdentityProvider): SAMLDraft {
  const c = (idp.config ?? {}) as Record<string, unknown>;
  return {
    name: idp.name,
    enabled: idp.enabled,
    entityId: String(c.entityId ?? ''),
    ssoUrl: String(c.ssoUrl ?? ''),
    certificatePem: String(c.certificatePem ?? '********'),
  };
}

function oidcToRequest(d: OIDCDraft): IdentityProviderWriteRequest {
  return {
    type: 'oidc',
    name: d.name.trim(),
    enabled: d.enabled,
    config: {
      issuer: d.issuer.trim(),
      clientId: d.clientId.trim(),
      clientSecret: d.clientSecret,
      redirectUri: d.redirectUri.trim(),
      jwksUri: d.jwksUri.trim() || undefined,
      authorizeUrl: d.authorizeUrl.trim() || undefined,
      tokenUrl: d.tokenUrl.trim() || undefined,
      scopes: d.scopes.trim().split(/\s+/).filter(Boolean),
      emailClaim: d.emailClaim.trim() || undefined,
      groupClaim: d.groupClaim.trim() || undefined,
    },
  };
}

function samlToRequest(d: SAMLDraft): IdentityProviderWriteRequest {
  return {
    type: 'saml',
    name: d.name.trim(),
    enabled: d.enabled,
    config: {
      entityId: d.entityId.trim(),
      ssoUrl: d.ssoUrl.trim(),
      certificatePem: d.certificatePem.trim(),
    },
  };
}

export interface IdentityProviderFormProps {
  mode: 'create' | 'edit';
  initial?: IdentityProvider;
  submitting: boolean;
  submitError: string | null;
  onSubmit: (body: IdentityProviderWriteRequest) => void;
  onCancel: () => void;
}

export function IdentityProviderForm({ mode, initial, submitting, submitError, onSubmit, onCancel }: IdentityProviderFormProps) {
  const { t } = useTranslation();
  const initialProtocol: Protocol = (initial?.type === 'saml' ? 'saml' : 'oidc');
  const [protocol, setProtocol] = useState<Protocol>(initialProtocol);
  const [oidc, setOidc] = useState<OIDCDraft>(() => (initial && initial.type === 'oidc' ? oidcDraftFromIdp(initial) : emptyOIDC()));
  const [saml, setSaml] = useState<SAMLDraft>(() => (initial && initial.type === 'saml' ? samlDraftFromIdp(initial) : emptySAML()));
  const [probeResult, setProbeResult] = useState<IdentityProviderProbeResult | null>(null);

  const buildRequest = (): IdentityProviderWriteRequest =>
    protocol === 'oidc' ? oidcToRequest(oidc) : samlToRequest(saml);

  const { mutate: doProbe, loading: probing } = useMutation(
    () => iamApi.testCandidateIdentityProvider(buildRequest()),
    {
      onSuccess: (r) => setProbeResult(r),
      onError: (e) => setProbeResult({ ok: false, error: e.message, elapsedMs: 0 }),
    },
  );

  const currentName = protocol === 'oidc' ? oidc.name : saml.name;
  const canSave = currentName.trim().length > 0 && !submitting;
  const protocolPickerDisabled = mode === 'edit';

  return (
    <Stack gap="md">
      <Card>
        <Stack gap="md">
          <FormField label={t('pages:identityProvider.wizard.protocol', 'Protocol')}>
            <div style={{ display: 'flex', gap: 'var(--g-space-2)' }}>
              <Button
                variant={protocol === 'oidc' ? 'primary' : 'secondary'}
                size="sm"
                disabled={protocolPickerDisabled}
                onClick={() => { setProtocol('oidc'); setProbeResult(null); }}
              >
                OIDC / OpenID Connect
              </Button>
              <Button
                variant={protocol === 'saml' ? 'primary' : 'secondary'}
                size="sm"
                disabled={protocolPickerDisabled}
                onClick={() => { setProtocol('saml'); setProbeResult(null); }}
              >
                SAML 2.0
              </Button>
            </div>
          </FormField>

          <FormField label={t('pages:identityProvider.wizard.displayName', 'Display name')}>
            <Input
              value={currentName}
              onChange={(e) => {
                const v = e.target.value;
                if (protocol === 'oidc') setOidc({ ...oidc, name: v });
                else setSaml({ ...saml, name: v });
              }}
              placeholder={protocol === 'oidc' ? 'Acme Okta' : 'Acme Azure AD'}
              autoFocus={mode === 'create'}
            />
          </FormField>

          {protocol === 'oidc' && (
            <>
              <FormField label={t('pages:identityProvider.wizard.fieldIssuerUrl', 'Issuer URL')} helpText={t('pages:identityProvider.wizard.issuerHelp', 'Base URL of the IdP. Nexus uses its /.well-known/openid-configuration for discovery.')}>
                <Input value={oidc.issuer} onChange={(e) => setOidc({ ...oidc, issuer: e.target.value })} placeholder="https://acme.okta.com" />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.fieldClientId', 'Client ID')}>
                <Input value={oidc.clientId} onChange={(e) => setOidc({ ...oidc, clientId: e.target.value })} placeholder="0oa1abc2def3ghi4j5k6" />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.fieldClientSecret', 'Client Secret')} helpText={mode === 'edit' ? t('pages:identityProvider.wizard.editSecretHelp', 'Leave as "********" to keep the saved value; type a new secret to replace it.') : undefined}>
                <Input
                  type={oidc.clientSecret === '********' ? 'text' : 'password'}
                  value={oidc.clientSecret}
                  onChange={(e) => setOidc({ ...oidc, clientSecret: e.target.value })}
                  placeholder={t('pages:identityProvider.wizard.clientSecretPlaceholder', 'Provided by the IdP when you registered the app')}
                />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.fieldRedirectUri', 'Redirect URI')} helpText={t('pages:identityProvider.wizard.redirectUriHelp', 'Register this exact URL with the IdP. Defaults to this Nexus host.')}>
                <Input value={oidc.redirectUri} onChange={(e) => setOidc({ ...oidc, redirectUri: e.target.value })} />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.scopes', 'Scopes')} helpText={t('pages:identityProvider.wizard.scopesHelp', 'Space-separated. openid is required; profile/email/groups recommended.')}>
                <Input value={oidc.scopes} onChange={(e) => setOidc({ ...oidc, scopes: e.target.value })} />
              </FormField>
            </>
          )}

          {protocol === 'saml' && (
            <>
              <FormField label={t('pages:identityProvider.wizard.fieldEntityId', 'Entity ID')} helpText={t('pages:identityProvider.wizard.entityIdHelp', 'The IdP entity (Issuer) — shown on the IdP app config page.')}>
                <Input value={saml.entityId} onChange={(e) => setSaml({ ...saml, entityId: e.target.value })} placeholder="http://www.okta.com/exk1abc2def3ghi4j5k6" />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.fieldSsoUrl', 'SSO URL')} helpText={t('pages:identityProvider.wizard.ssoUrlHelp', 'IdP-side endpoint where Nexus POSTs AuthnRequests.')}>
                <Input value={saml.ssoUrl} onChange={(e) => setSaml({ ...saml, ssoUrl: e.target.value })} placeholder="https://acme.okta.com/app/.../sso/saml" />
              </FormField>
              <FormField label={t('pages:identityProvider.wizard.certPem', 'Signing certificate (PEM)')} helpText={t('pages:identityProvider.wizard.certPemHelp', 'X.509 certificate the IdP uses to sign assertions. Paste the PEM block.')}>
                <Textarea
                  value={saml.certificatePem}
                  onChange={(e) => setSaml({ ...saml, certificatePem: e.target.value })}
                  placeholder="-----BEGIN CERTIFICATE-----&#10;MIIC...&#10;-----END CERTIFICATE-----"
                  rows={6}
                  style={{ fontFamily: 'var(--g-font-mono)', fontSize: 'var(--g-font-size-sm)' }}
                />
              </FormField>
            </>
          )}

          {probeResult && (
            <div
              style={{
                padding: 'var(--g-space-3)',
                borderRadius: 'var(--g-radius-sm)',
                border: `1px solid var(--color-${probeResult.ok ? 'success' : 'danger'}-border)`,
                background: `var(--color-${probeResult.ok ? 'success' : 'danger'}-light)`,
                color: `var(--color-${probeResult.ok ? 'success' : 'danger'}-text)`,
                fontSize: 'var(--g-font-size-sm)',
              }}
            >
              <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', marginBottom: 'var(--g-space-1)' }}>
                <Badge variant={probeResult.ok ? 'success' : 'danger'}>
                  {probeResult.ok
                    ? t('pages:identityProvider.wizard.probeOk', 'Connection OK')
                    : t('pages:identityProvider.wizard.probeFailed', 'Connection failed')}
                </Badge>
                <span style={{ color: 'var(--color-text-muted)' }}>{probeResult.elapsedMs}ms</span>
              </div>
              {probeResult.error && <pre style={{ margin: 'var(--g-space-0)', whiteSpace: 'pre-wrap' }}>{probeResult.error}</pre>}
              {probeResult.ok && probeResult.detail && (
                <pre style={{ margin: 'var(--g-space-0)', whiteSpace: 'pre-wrap', fontFamily: 'var(--g-font-mono)' }}>
                  {JSON.stringify(probeResult.detail, null, 2)}
                </pre>
              )}
            </div>
          )}

          {submitError && <ErrorBanner message={submitError} />}

          <div style={{ display: 'flex', gap: 'var(--g-space-2)', justifyContent: 'flex-end', borderTop: '1px solid var(--color-border)', paddingTop: 'var(--g-space-4)' }}>
            <Button variant="secondary" onClick={onCancel}>{t('common:cancel', 'Cancel')}</Button>
            <Button variant="secondary" onClick={() => { setProbeResult(null); void doProbe(undefined); }} loading={probing}>
              {t('pages:identityProvider.wizard.testConnection', 'Test connection')}
            </Button>
            <Button onClick={() => onSubmit(buildRequest())} loading={submitting} disabled={!canSave}>
              {mode === 'create'
                ? t('pages:identityProvider.wizard.save', 'Save')
                : t('common:save', 'Save')}
            </Button>
          </div>
        </Stack>
      </Card>
    </Stack>
  );
}
