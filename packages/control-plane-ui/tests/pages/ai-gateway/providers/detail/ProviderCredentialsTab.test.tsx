import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { ProviderCredentialsTab } from '@/pages/ai-gateway/providers/detail/ProviderCredentialsTab';

const cred = { id: 'c1', name: 'CI Key', enabled: true, status: 'active', circuitState: 'closed', expiresAt: null, lastUsedAt: null };
const h = { createCredential: vi.fn(), toggleCredEnabled: vi.fn(), startEditingCred: vi.fn(), setDeletingCred: vi.fn(), setShowCredForm: vi.fn(), handleCredUpdate: vi.fn(), setEditingCredId: vi.fn() };

function Harness({
  showCredForm = false,
  editingCredId = null,
  canCreateCredential = true,
  canUpdateCredential = true,
  canDeleteCredential = true,
}: {
  showCredForm?: boolean;
  editingCredId?: string | null;
  canCreateCredential?: boolean;
  canUpdateCredential?: boolean;
  canDeleteCredential?: boolean;
}) {
  const newCredForm = useForm({ defaultValues: { credName: 'New Key', credApiKey: 'sk-123', newCredEnabled: true, credExpiresAt: '' } });
  const editCredForm = useForm({ defaultValues: { editCredName: 'CI Key', editCredApiKey: '', editCredEnabled: true, editCredExpiresAt: '' } });
  const detail = {
    // The provider actions are deliberately granted here: a principal holding
    // them and NOT the credential ones must still see no credential write
    // affordance, which is the whole point of the gate below.
    id: 'p1', credentials: [cred], canUpdate: true, canDelete: true,
    canCreateCredential, canUpdateCredential, canDeleteCredential,
    showCredForm, setShowCredForm: h.setShowCredForm, newCredForm, createCredential: h.createCredential, credCreating: false,
    editingCredId, setEditingCredId: h.setEditingCredId, editCredForm, handleCredUpdate: h.handleCredUpdate, credUpdating: false,
    startEditingCred: h.startEditingCred, toggleCredEnabled: h.toggleCredEnabled, setDeletingCred: h.setDeletingCred,
  } as never;
  return <I18nextProvider i18n={i18n}><ProviderCredentialsTab detail={detail} /></I18nextProvider>;
}

describe('ProviderCredentialsTab', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders the credential row', () => {
    render(<Harness />);
    expect(screen.getByText('CI Key')).toBeInTheDocument();
  });

  it('Add credential toggles the inline form', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:providers.addCredential') }));
    expect(h.setShowCredForm).toHaveBeenCalledWith(true);
  });

  it('with the form open + filled, Create builds the credential payload', () => {
    render(<Harness showCredForm />);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:create') }));
    expect(h.createCredential).toHaveBeenCalledWith({ name: 'New Key', providerId: 'p1', apiKey: 'sk-123', enabled: true, expiresAt: undefined });
  });

  it('the row enable toggle flips toggleCredEnabled', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:enabled') }));
    expect(h.toggleCredEnabled).toHaveBeenCalledWith({ id: 'c1', enabled: false });
  });

  it('Edit + Delete invoke the row handlers', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:edit') }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:delete') }));
    expect(h.startEditingCred).toHaveBeenCalledWith(cred);
    expect(h.setDeletingCred).toHaveBeenCalledWith(cred);
  });

  it('editing mode renders the inline edit form + Save calls handleCredUpdate', () => {
    render(<Harness editingCredId="c1" />);
    expect(screen.getByText(i18n.t('pages:providers.editing', { name: 'CI Key' }))).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:save') }));
    expect(h.handleCredUpdate).toHaveBeenCalled();
  });

  // The writes behind these affordances are PUT/DELETE /api/admin/credentials/:id,
  // guarded on admin:credential.update / .delete — not on the provider action
  // that guards the page this tab is embedded in. Gating them on the provider
  // permission offers the button to a principal the backend rejects, after the
  // admin has filled the form.
  it('hides Edit when the principal cannot update credentials, even holding provider:update', () => {
    render(<Harness canUpdateCredential={false} />);
    expect(screen.queryByRole('button', { name: i18n.t('common:edit') })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: i18n.t('common:delete') })).toBeInTheDocument();
  });

  it('hides Delete when the principal cannot delete credentials, even holding provider:delete', () => {
    render(<Harness canDeleteCredential={false} />);
    expect(screen.queryByRole('button', { name: i18n.t('common:delete') })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: i18n.t('common:edit') })).toBeInTheDocument();
  });
});
