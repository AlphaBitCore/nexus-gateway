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
import { useApi } from '@/hooks/useApi';
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
  Select,
  Switch,
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
  /** Extra key/value pairs appended to the IdP authorize request (e.g. Auth0 `organization`). */
  authorizeParams: Array<{ key: string; value: string }>;
}

interface SAMLDraft {
  name: string;
  enabled: boolean;
  entityId: string;
  ssoUrl: string;
  certificatePem: string;
  /** Assertion attribute carrying the user's email; blank → backend default "email". */
  emailAttribute: string;
  /** Assertion attribute carrying group memberships; blank → backend default "groups". */
  groupsAttribute: string;
  /** Extra key/value pairs appended to the IdP SSO URL (e.g. Auth0 `organization`). */
  ssoParams: Array<{ key: string; value: string }>;
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
    authorizeParams: [],
  };
}

function emptySAML(): SAMLDraft {
  return {
    name: '',
    enabled: true,
    entityId: '',
    ssoUrl: '',
    certificatePem: '',
    emailAttribute: '',
    groupsAttribute: '',
    ssoParams: [],
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
    authorizeParams: Array.isArray(c.authorizeParams)
      ? (c.authorizeParams as Array<Record<string, unknown>>).map((p) => ({
          key: String(p.key ?? ''),
          value: String(p.value ?? ''),
        }))
      : [],
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
    emailAttribute: String(c.emailAttribute ?? ''),
    groupsAttribute: String(c.groupsAttribute ?? ''),
    ssoParams: Array.isArray(c.ssoParams)
      ? (c.ssoParams as Array<Record<string, unknown>>).map((p) => ({
          key: String(p.key ?? ''),
          value: String(p.value ?? ''),
        }))
      : [],
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
      authorizeParams: d.authorizeParams
        .map((p) => ({ key: p.key.trim(), value: p.value.trim() }))
        .filter((p) => p.key !== ''),
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
      // Blank → omit so the backend applies its "email"/"groups" defaults.
      emailAttribute: d.emailAttribute.trim() || undefined,
      groupsAttribute: d.groupsAttribute.trim() || undefined,
      ssoParams: d.ssoParams
        .map((p) => ({ key: p.key.trim(), value: p.value.trim() }))
        .filter((p) => p.key !== ''),
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

  // SAML metadata import: paste the IdP's metadata XML to auto-fill the
  // entityId / ssoUrl / certificate (and detected email/groups attributes)
  // instead of hand-copying them. metadataNote carries the post-import result.
  const [metadataXml, setMetadataXml] = useState('');
  const [metadataNote, setMetadataNote] = useState<{ ok: boolean; msg: string } | null>(null);

  // JIT defaults apply to both protocols (they live on the IdP row, not in the
  // protocol config), so they're shared component state rather than per-draft.
  const [defaultRole, setDefaultRole] = useState<string>(initial?.defaultRole ?? '');
  const [defaultControlPlaneAccess, setDefaultControlPlaneAccess] = useState<boolean>(
    initial?.defaultControlPlaneAccess ?? false,
  );

  // Group list backs the default-role picker — defaultRole is resolved server-side
  // by IamGroup name, so we offer the real groups instead of a free-text field.
  const { data: groupsResp } = useApi(() => iamApi.listGroups(), ['admin', 'iam', 'groups']);
  const groupOptions = (groupsResp?.data ?? []).map((g) => ({ value: g.name, label: g.name }));

  const buildRequest = (): IdentityProviderWriteRequest => {
    const base = protocol === 'oidc' ? oidcToRequest(oidc) : samlToRequest(saml);
    return {
      ...base,
      defaultRole: defaultRole.trim() || undefined,
      defaultControlPlaneAccess,
    };
  };

  const { mutate: doProbe, loading: probing } = useMutation(
    // In edit mode pass the saved IdP id so the backend restores the
    // masked "********" secret from storage before probing.
    () => iamApi.testCandidateIdentityProvider(buildRequest(), mode === 'edit' ? initial?.id : undefined),
    {
      onSuccess: (r) => setProbeResult(r),
      onError: (e) => setProbeResult({ ok: false, error: e.message, elapsedMs: 0 }),
    },
  );

  const { mutate: doImportMetadata, loading: importing } = useMutation(
    () => iamApi.parseSamlMetadata(metadataXml),
    {
      // The inline metadataNote next to the button is the sole error surface
      // for a parse failure; suppress the generic error toast so it isn't
      // shown twice.
      silentError: true,
      onSuccess: (r) => {
        // Parsed values win; keep any field the document didn't carry.
        setSaml((s) => ({
          ...s,
          entityId: r.entityId || s.entityId,
          ssoUrl: r.ssoUrl || s.ssoUrl,
          certificatePem: r.certificatePem || s.certificatePem,
          emailAttribute: r.emailAttribute || s.emailAttribute,
          groupsAttribute: r.groupsAttribute || s.groupsAttribute,
        }));
        setMetadataNote({ ok: true, msg: t('pages:identityProvider.wizard.metadataImported', 'Metadata imported — review the fields below.') });
      },
      onError: (e) => setMetadataNote({ ok: false, msg: e.message }),
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
                <Input value={oidc.clientId} onChange={(e) => setOidc({ ...oidc, clientId: e.target.value })} placeholder={t('pages:idp.clientIdPlaceholder')} />
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
              <FormField
                label={t('pages:identityProvider.wizard.authorizeParams', 'Extra authorize parameters')}
                helpText={t('pages:identityProvider.wizard.authorizeParamsHelp', 'Extra key/value pairs appended to the IdP sign-in request — e.g. Auth0 organization, or prompt / connection / audience.')}
              >
                <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-2)' }}>
                  {oidc.authorizeParams.map((p, i) => (
                    <div key={i} style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
                      <Input
                        value={p.key}
                        onChange={(e) => {
                          const next = [...oidc.authorizeParams];
                          next[i] = { ...next[i], key: e.target.value };
                          setOidc({ ...oidc, authorizeParams: next });
                        }}
                        placeholder={t('pages:identityProvider.wizard.authorizeParamKey', 'key (e.g. organization)')}
                      />
                      <Input
                        value={p.value}
                        onChange={(e) => {
                          const next = [...oidc.authorizeParams];
                          next[i] = { ...next[i], value: e.target.value };
                          setOidc({ ...oidc, authorizeParams: next });
                        }}
                        placeholder={t('pages:identityProvider.wizard.authorizeParamValue', 'value')}
                      />
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => setOidc({ ...oidc, authorizeParams: oidc.authorizeParams.filter((_, j) => j !== i) })}
                        aria-label={t('common:remove', 'Remove')}
                      >
                        {t('common:remove', 'Remove')}
                      </Button>
                    </div>
                  ))}
                  <div>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => setOidc({ ...oidc, authorizeParams: [...oidc.authorizeParams, { key: '', value: '' }] })}
                    >
                      {t('pages:identityProvider.wizard.authorizeParamAdd', 'Add parameter')}
                    </Button>
                  </div>
                </div>
              </FormField>
            </>
          )}

          {protocol === 'saml' && (
            <>
              <FormField
                label={t('pages:identityProvider.wizard.metadataImport', 'Import from metadata (optional)')}
                helpText={t('pages:identityProvider.wizard.metadataImportHelp', "Paste the IdP's SAML metadata XML to auto-fill Entity ID, SSO URL, signing certificate, and detected email/groups attributes. You can still edit any field afterwards.")}
              >
                <div>
                  <Textarea
                    value={metadataXml}
                    onChange={(e) => { setMetadataXml(e.target.value); setMetadataNote(null); }}
                    placeholder="<EntityDescriptor ...>…</EntityDescriptor>"
                    rows={4}
                    style={{ fontFamily: 'var(--g-font-mono)', fontSize: 'var(--g-font-size-sm)' }}
                  />
                  <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', marginTop: 'var(--g-space-2)' }}>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => { setMetadataNote(null); doImportMetadata(undefined).catch(() => { /* surfaced via metadataNote */ }); }}
                      loading={importing}
                      disabled={metadataXml.trim().length === 0}
                    >
                      {t('pages:identityProvider.wizard.metadataImportButton', 'Import metadata')}
                    </Button>
                    {metadataNote && (
                      <span style={{ fontSize: 'var(--g-font-size-sm)', color: `var(--color-${metadataNote.ok ? 'success' : 'danger'}-text)` }}>
                        {metadataNote.msg}
                      </span>
                    )}
                  </div>
                </div>
              </FormField>
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
              <FormField
                label={t('pages:identityProvider.wizard.fieldEmailAttribute', 'Email attribute')}
                helpText={t('pages:identityProvider.wizard.emailAttributeHelp', 'Assertion attribute carrying the user\'s email. Leave blank to use the default ("email") plus runtime detection of common claim names.')}
              >
                <Input value={saml.emailAttribute} onChange={(e) => setSaml({ ...saml, emailAttribute: e.target.value })} placeholder="email" />
              </FormField>
              <FormField
                label={t('pages:identityProvider.wizard.fieldGroupsAttribute', 'Groups attribute')}
                helpText={t('pages:identityProvider.wizard.groupsAttributeHelp', 'Assertion attribute carrying group memberships (mapped via group mappings). Leave blank to use the default ("groups") plus runtime detection.')}
              >
                <Input value={saml.groupsAttribute} onChange={(e) => setSaml({ ...saml, groupsAttribute: e.target.value })} placeholder="groups" />
              </FormField>
              <FormField
                label={t('pages:identityProvider.wizard.ssoParams', 'Extra SSO parameters')}
                helpText={t('pages:identityProvider.wizard.ssoParamsHelp', 'Extra key/value pairs appended to the IdP SSO URL on sign-in — e.g. Auth0 Organizations requires `organization`.')}
              >
                <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-2)' }}>
                  {saml.ssoParams.map((p, i) => (
                    <div key={i} style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
                      <Input
                        value={p.key}
                        onChange={(e) => {
                          const next = [...saml.ssoParams];
                          next[i] = { ...next[i], key: e.target.value };
                          setSaml({ ...saml, ssoParams: next });
                        }}
                        placeholder={t('pages:identityProvider.wizard.ssoParamKey', 'key (e.g. organization)')}
                      />
                      <Input
                        value={p.value}
                        onChange={(e) => {
                          const next = [...saml.ssoParams];
                          next[i] = { ...next[i], value: e.target.value };
                          setSaml({ ...saml, ssoParams: next });
                        }}
                        placeholder={t('pages:identityProvider.wizard.ssoParamValue', 'value')}
                      />
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => setSaml({ ...saml, ssoParams: saml.ssoParams.filter((_, j) => j !== i) })}
                        aria-label={t('common:remove', 'Remove')}
                      >
                        {t('common:remove', 'Remove')}
                      </Button>
                    </div>
                  ))}
                  <div>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => setSaml({ ...saml, ssoParams: [...saml.ssoParams, { key: '', value: '' }] })}
                    >
                      {t('pages:identityProvider.wizard.ssoParamAdd', 'Add parameter')}
                    </Button>
                  </div>
                </div>
              </FormField>
            </>
          )}

          {/* JIT provisioning defaults — apply to users auto-created on first
              SSO login, for both OIDC and SAML. */}
          <FormField
            label={t('pages:identityProvider.wizard.defaultRole', 'Default role')}
            helpText={t('pages:identityProvider.wizard.defaultRoleHelp', 'IAM group every auto-provisioned user joins as a baseline, on top of any external-group mappings.')}
          >
            <Select
              value={defaultRole}
              onValueChange={setDefaultRole}
              options={groupOptions}
              placeholder={t('pages:identityProvider.wizard.defaultRolePlaceholder', 'Select a group')}
            />
          </FormField>
          <FormField
            label={t('pages:identityProvider.wizard.defaultCpAccess', 'Grant Control Plane access by default')}
            helpText={t('pages:identityProvider.wizard.defaultCpAccessHelp', 'When on, users auto-provisioned through this IdP can sign in to the Control Plane. Leave off for IdPs that federate agent end-users.')}
          >
            <Switch checked={defaultControlPlaneAccess} onCheckedChange={setDefaultControlPlaneAccess} />
          </FormField>

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
