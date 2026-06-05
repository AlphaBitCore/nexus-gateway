import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import i18n from '@/i18n';
import { IdentityProviderForm } from '@/pages/devices/auth/IdentityProviderForm';

// listGroups backs the default-role picker (useApi → useQuery, real here);
// parseSamlMetadata backs the SAML metadata-import helper.
const iam = vi.hoisted(() => ({
  iamApi: {
    testCandidateIdentityProvider: vi.fn(),
    listGroups: vi.fn(),
    parseSamlMetadata: vi.fn(),
  },
}));
vi.mock('@/api/services', () => iam);
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void; onError?: (e: Error) => void }) => ({
    mutate: async (arg: unknown) => { try { const r = await fn(arg); opts?.onSuccess?.(r); return r; } catch (e) { opts?.onError?.(e as Error); } },
    loading: false,
  }),
}));

function wrap(props: Partial<React.ComponentProps<typeof IdentityProviderForm>> = {}) {
  const onSubmit = props.onSubmit ?? vi.fn();
  const onCancel = props.onCancel ?? vi.fn();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <I18nextProvider i18n={i18n}>
        <IdentityProviderForm mode="create" submitting={false} onSubmit={onSubmit} onCancel={onCancel} {...props} />
      </I18nextProvider>
    </QueryClientProvider>,
  );
  return { onSubmit, onCancel };
}

describe('IdentityProviderForm', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    iam.iamApi.testCandidateIdentityProvider.mockResolvedValue({ ok: true, elapsedMs: 42 });
    iam.iamApi.listGroups.mockResolvedValue({ data: [] });
  });

  it('defaults to OIDC and shows the OIDC fields', () => {
    wrap();
    expect(screen.getByText(/Issuer URL/i)).toBeInTheDocument();
  });

  it('Save is gated on a display name, then submits an OIDC write request', () => {
    const { onSubmit } = wrap();
    const save = screen.getByRole('button', { name: /^save$/i });
    expect(save).toBeDisabled();
    fireEvent.change(screen.getByPlaceholderText('Acme Okta'), { target: { value: 'Acme Okta IdP' } });
    expect(save).toBeEnabled();
    fireEvent.click(save);
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ type: 'oidc', name: 'Acme Okta IdP' }));
  });

  it('switching to SAML reveals the SAML-specific fields', () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: /SAML 2\.0/ }));
    // Exact label match: the metadata-import help text also mentions
    // "Entity ID" / "SSO URL", so a loose regex would match multiple nodes.
    expect(screen.getByText('Entity ID')).toBeInTheDocument();
    expect(screen.getByText('SSO URL')).toBeInTheDocument();
  });

  it('SAML: extra SSO params round-trip into the saml config', () => {
    const { onSubmit } = wrap();
    fireEvent.click(screen.getByRole('button', { name: /SAML 2\.0/ }));
    fireEvent.change(screen.getByPlaceholderText('Acme Azure AD'), { target: { value: 'Acme SAML' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:identityProvider.wizard.ssoParamAdd', 'Add parameter') }));
    fireEvent.change(
      screen.getByPlaceholderText(i18n.t('pages:identityProvider.wizard.ssoParamKey', 'key (e.g. organization)')),
      { target: { value: 'organization' } },
    );
    fireEvent.change(
      screen.getByPlaceholderText(i18n.t('pages:identityProvider.wizard.ssoParamValue', 'value')),
      { target: { value: 'org_abc123' } },
    );
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:identityProvider.wizard.save', 'Save') }));
    const body = onSubmit.mock.calls[0][0];
    expect(body.type).toBe('saml');
    expect(body.config.ssoParams).toEqual([{ key: 'organization', value: 'org_abc123' }]);
  });

  it('Test connection probes the candidate IdP and surfaces the OK result', async () => {
    wrap();
    fireEvent.change(screen.getByPlaceholderText('Acme Okta'), { target: { value: 'Okta' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:identityProvider.wizard.testConnection', 'Test connection') }));
    await waitFor(() => expect(iam.iamApi.testCandidateIdentityProvider).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText(i18n.t('pages:identityProvider.wizard.probeOk', 'Connection OK'))).toBeInTheDocument());
  });

  it('edit mode hydrates from the initial IdP and locks the protocol picker', () => {
    wrap({
      mode: 'edit',
      initial: { id: 'i1', type: 'oidc', name: 'Existing Okta', enabled: true, config: { issuer: 'https://x', clientId: 'c' } } as never,
    });
    expect(screen.getByDisplayValue('Existing Okta')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /OIDC/ })).toBeDisabled();
  });

  it('Cancel invokes onCancel', () => {
    const { onCancel } = wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:cancel', 'Cancel') }));
    expect(onCancel).toHaveBeenCalled();
  });
});
