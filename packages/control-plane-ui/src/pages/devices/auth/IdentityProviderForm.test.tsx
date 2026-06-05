import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ToastProvider } from '@/context/ToastContext';
import { IdentityProviderForm } from './IdentityProviderForm';

// t returns the key (ignoring the English fallback) so assertions don't depend
// on i18n initialization.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
  // The form's import graph pulls in src/i18n/index.ts, which calls
  // .use(initReactI18next) at module load — provide the plugin shape.
  initReactI18next: { type: '3rdParty', init: () => {} },
}));

// Mock the IdP admin service: parseSamlMetadata returns a fixed parsed doc;
// listGroups feeds the default-role picker; testCandidateIdentityProvider is
// unused here but referenced by the form.
const parseSamlMetadata = vi.fn();
vi.mock('@/api/services', () => ({
  iamApi: {
    parseSamlMetadata: (xml: string) => parseSamlMetadata(xml),
    listGroups: vi.fn(async () => ({ data: [] })),
    testCandidateIdentityProvider: vi.fn(async () => ({ ok: true, elapsedMs: 1 })),
  },
}));

const METADATA_PLACEHOLDER = '<EntityDescriptor ...>…</EntityDescriptor>';
const ENTITY_ID_PLACEHOLDER = 'http://www.okta.com/exk1abc2def3ghi4j5k6';
const IMPORT_BTN = 'pages:identityProvider.wizard.metadataImportButton';
const SAVE_BTN = 'pages:identityProvider.wizard.save';

function renderForm(onSubmit = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <IdentityProviderForm
          mode="create"
          submitting={false}
          submitError={null}
          onSubmit={onSubmit}
          onCancel={vi.fn()}
        />
      </ToastProvider>
    </QueryClientProvider>,
  );
  // Switch to the SAML protocol so the SAML fields render.
  fireEvent.click(screen.getByRole('button', { name: 'SAML 2.0' }));
  return { onSubmit };
}

describe('IdentityProviderForm — SAML metadata import', () => {
  beforeEach(() => {
    parseSamlMetadata.mockReset();
  });

  it('imports metadata XML and pre-fills entityId, ssoUrl, cert, and detected attributes', async () => {
    parseSamlMetadata.mockResolvedValue({
      entityId: 'urn:idp.auth0.test',
      ssoUrl: 'https://idp.auth0.test/samlp/abc',
      certificatePem: '-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----',
      emailAttribute: 'http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress',
      groupsAttribute: 'http://schemas.xmlsoap.org/claims/Group',
    });
    renderForm();

    const metaBox = screen.getByPlaceholderText(METADATA_PLACEHOLDER);
    fireEvent.change(metaBox, { target: { value: '<EntityDescriptor>real-xml</EntityDescriptor>' } });

    const importBtn = screen.getByRole('button', { name: IMPORT_BTN });
    expect(importBtn).not.toBeDisabled();
    fireEvent.click(importBtn);

    await waitFor(() => {
      expect(parseSamlMetadata).toHaveBeenCalledWith('<EntityDescriptor>real-xml</EntityDescriptor>');
    });

    const entityId = screen.getByPlaceholderText(ENTITY_ID_PLACEHOLDER) as HTMLInputElement;
    await waitFor(() => expect(entityId.value).toBe('urn:idp.auth0.test'));
    expect((screen.getByPlaceholderText('email') as HTMLInputElement).value).toBe(
      'http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress',
    );
    expect((screen.getByPlaceholderText('groups') as HTMLInputElement).value).toBe(
      'http://schemas.xmlsoap.org/claims/Group',
    );
  });

  it('submits the imported attributes inside the SAML config', async () => {
    parseSamlMetadata.mockResolvedValue({
      entityId: 'urn:idp.test',
      ssoUrl: 'https://idp.test/sso',
      certificatePem: 'PEM',
      emailAttribute: 'mail',
      groupsAttribute: 'memberOf',
    });
    const { onSubmit } = renderForm();

    fireEvent.change(screen.getByPlaceholderText('Acme Azure AD'), { target: { value: 'Acme SAML' } });
    fireEvent.change(screen.getByPlaceholderText(METADATA_PLACEHOLDER), { target: { value: '<x/>' } });
    fireEvent.click(screen.getByRole('button', { name: IMPORT_BTN }));
    await waitFor(() =>
      expect((screen.getByPlaceholderText('email') as HTMLInputElement).value).toBe('mail'),
    );

    fireEvent.click(screen.getByRole('button', { name: SAVE_BTN }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const body = onSubmit.mock.calls[0][0];
    expect(body.type).toBe('saml');
    expect(body.config.emailAttribute).toBe('mail');
    expect(body.config.groupsAttribute).toBe('memberOf');
    expect(body.config.entityId).toBe('urn:idp.test');
  });

  it('omits blank attribute names so the backend applies its defaults', () => {
    const { onSubmit } = renderForm();
    fireEvent.change(screen.getByPlaceholderText('Acme Azure AD'), { target: { value: 'No-Attrs IdP' } });
    fireEvent.change(screen.getByPlaceholderText(ENTITY_ID_PLACEHOLDER), { target: { value: 'urn:plain' } });

    fireEvent.click(screen.getByRole('button', { name: SAVE_BTN }));

    const body = onSubmit.mock.calls[0][0];
    expect(body.config.entityId).toBe('urn:plain');
    expect(body.config.emailAttribute).toBeUndefined();
    expect(body.config.groupsAttribute).toBeUndefined();
  });

  it('disables the import button until metadata XML is entered', () => {
    renderForm();
    expect(screen.getByRole('button', { name: IMPORT_BTN })).toBeDisabled();
  });

  it('shows the parse error returned by the backend', async () => {
    parseSamlMetadata.mockRejectedValue(new Error('saml metadata has no IDPSSODescriptor'));
    renderForm();
    fireEvent.change(screen.getByPlaceholderText(METADATA_PLACEHOLDER), { target: { value: '<sp-only/>' } });
    fireEvent.click(screen.getByRole('button', { name: IMPORT_BTN }));

    await waitFor(() => {
      expect(screen.getByText('saml metadata has no IDPSSODescriptor')).toBeTruthy();
    });
  });
});
